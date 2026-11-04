package host

// The durable session mailbox (§6.6): one queue per target (thread or
// handle), shared by SendInput, subagent messages and control adapters.
// All adapters go through RequireAgentAccess (control.go), AcceptInput
// and drain (the loop's DeliverPending) — there is no separate UI queue.

import (
	"context"
	"fmt"
	"sync"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/transcript"
)

// Input modes (§6.6).
const (
	ModeMessage  = "message"
	ModeSteer    = "steer"
	ModeFollowUp = "follow_up"
)

// Receipt states (§6.6).
const (
	ReceiptAccepted  = "accepted"
	ReceiptDelivered = "delivered"
	ReceiptCancelled = "cancelled"
	ReceiptRejected  = "rejected"
)

// SendRequest is one SendInput call. Target is a handle ID or thread name
// ("" = primary thread).
type SendRequest struct {
	Target             string
	RequestID          string // idempotency key; generated when empty
	Mode               string // message | steer | follow_up (default message)
	Text               string
	ExpectedActivation string // required for steer: the live node ID
	Interrupt          bool   // interrupt-then-steer (§6.6)
	// Wake starts an idle handle to handle this input, as a follow-up
	// does, whatever the mode (agent-to-agent messages, §6.7).
	Wake   bool
	Origin model.CallerInfo
}

// Receipt is the durable acknowledgement of a SendInput. Acceptance is
// not a claim the model has seen the input.
type Receipt struct {
	InputID   string `json:"input_id,omitempty"`
	RequestID string `json:"request_id"`
	Target    string `json:"target"`
	Mode      string `json:"mode"`
	State     string `json:"state"` // accepted|delivered|cancelled|rejected
	Code      string `json:"code,omitempty"`
	Seq       int    `json:"seq,omitempty"` // transcript Seq once delivered
}

type mailEntry struct {
	receipt   Receipt
	text      string
	origin    model.CallerInfo
	interrupt bool
	wake      bool
}

type mailbox struct {
	s       *Host
	mu      sync.Mutex
	queues  map[string][]*mailEntry // target -> FIFO pending (accepted)
	byReq   map[string]*mailEntry   // target|request_id -> entry (any state)
	byInput map[string]*mailEntry
	active  map[string]string // target -> live activation node ID
}

func newMailbox(s *Host) *mailbox {
	return &mailbox{s: s, queues: map[string][]*mailEntry{},
		byReq: map[string]*mailEntry{}, byInput: map[string]*mailEntry{},
		active: map[string]string{}}
}

func (m *mailbox) setActivation(target, nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if nodeID == "" {
		delete(m.active, target)
	} else {
		m.active[target] = nodeID
	}
}

func (m *mailbox) activation(target string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[target]
}

func (m *mailbox) pendingLocked(target string) int {
	n := 0
	for _, e := range m.queues[target] {
		if e.receipt.State == ReceiptAccepted {
			n++
		}
	}
	return n
}

func (m *mailbox) pendingCount(target string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingLocked(target)
}

// wantsWake reports pending input that should start an idle handle:
// a follow-up, or a message sent with Wake.
func (m *mailbox) wantsWake(target string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.queues[target] {
		if e.receipt.State == ReceiptAccepted && (e.wake || e.receipt.Mode == ModeFollowUp) {
			return true
		}
	}
	return false
}

func (m *mailbox) hasFollowUp(target string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.queues[target] {
		if e.receipt.State == ReceiptAccepted && e.receipt.Mode == ModeFollowUp {
			return true
		}
	}
	return false
}

// accept validates and durably queues one input (§6.6). Size/queue limits
// reject before acceptance; duplicate request IDs return the same
// receipt; conflicting bodies are rejected. Steer requires the expected
// live activation, else target_changed.
func (m *mailbox) accept(ctx context.Context, target string, req SendRequest) Receipt {
	mode := req.Mode
	if mode == "" {
		mode = ModeMessage
	}
	reqID := req.RequestID
	if reqID == "" {
		reqID = model.NewID()
	}
	base := Receipt{RequestID: reqID, Target: target, Mode: mode}
	reject := func(code string) Receipt {
		r := base
		r.State, r.Code = ReceiptRejected, code
		_ = m.s.emit(ctx, trace.Event{Type: "input.rejected",
			Data: mustJSON(map[string]any{"receipt": r, "origin": req.Origin.ID})})
		return r
	}
	if mode != ModeMessage && mode != ModeSteer && mode != ModeFollowUp {
		return reject("invalid_mode")
	}
	cfg := m.s.Config()
	if cfg.Input.MaxBytes > 0 && len(req.Text) > cfg.Input.MaxBytes {
		return reject("too_large")
	}
	m.mu.Lock()
	if prev, ok := m.byReq[target+"|"+reqID]; ok {
		same := prev.text == req.Text && prev.receipt.Mode == mode
		r := prev.receipt
		m.mu.Unlock()
		if !same {
			return reject("duplicate_conflict")
		}
		return r // same receipt, stable input ID
	}
	if mode == ModeSteer && !req.Interrupt {
		live := m.active[target]
		if live == "" || req.ExpectedActivation != live {
			m.mu.Unlock()
			return reject("target_changed")
		}
	}
	if cfg.Input.MaxPending > 0 && m.pendingLocked(target) >= cfg.Input.MaxPending {
		m.mu.Unlock()
		return reject("queue_full")
	}
	e := &mailEntry{text: req.Text, origin: req.Origin, interrupt: req.Interrupt, wake: req.Wake,
		receipt: Receipt{InputID: model.NewID(), RequestID: reqID, Target: target,
			Mode: mode, State: ReceiptAccepted}}
	m.queues[target] = append(m.queues[target], e)
	m.byReq[target+"|"+reqID] = e
	m.byInput[e.receipt.InputID] = e
	r := e.receipt
	m.mu.Unlock()

	// Durable acceptance: body blob + event (§7.1).
	var bodyHash string
	if m.s.recorder != nil {
		if h, err := m.s.recorder.PutBlob(ctx, []byte(req.Text)); err == nil {
			bodyHash = string(h)
		}
	}
	_ = m.s.emit(ctx, trace.Event{Type: "input.accepted",
		Data: mustJSON(map[string]any{"receipt": r, "body_hash": bodyHash,
			"origin": req.Origin, "expected_activation": req.ExpectedActivation})})
	m.s.changed.fire()
	return r
}

// activation tracks follow-up arming for one live node: follow-ups are
// delivered only after the loop's would-be-stop check (HasFollowUp), or
// when the activation was itself started by a follow-up.
type activation struct {
	target string
	mu     sync.Mutex
	armed  bool
}

func (a *activation) arm() {
	a.mu.Lock()
	a.armed = true
	a.mu.Unlock()
}

func (a *activation) takeArmed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.armed
	a.armed = false
	return v
}

// drain is the loop's DeliverPending (§6.6): FIFO steer/message first,
// then (only when armed) follow-ups; queue_mode one = one input per call.
// Delivery and the transcript item are recorded together; replay dedupes
// by InputID. Human input projects as a user item, agent input as a
// labelled notice — never impersonated user instruction — naming the
// sender's address to reply to when it has one (§6.7).
func (m *mailbox) drain(ctx context.Context, thread *transcript.Transcript, act *activation) []model.Item {
	cfg := m.s.Config()
	m.mu.Lock()
	q := m.queues[act.target]
	var picked []*mailEntry
	for _, e := range q {
		if e.receipt.State != ReceiptAccepted {
			continue
		}
		if e.receipt.Mode == ModeFollowUp {
			continue
		}
		picked = append(picked, e)
		if cfg.Input.QueueMode != "all" {
			break
		}
	}
	if len(picked) == 0 && act.takeArmed() {
		for _, e := range q {
			if e.receipt.State == ReceiptAccepted && e.receipt.Mode == ModeFollowUp {
				picked = append(picked, e)
				if cfg.Input.QueueMode != "all" {
					break
				}
			}
		}
	}
	if len(picked) == 0 {
		m.mu.Unlock()
		return nil
	}
	baseSeq := thread.Len()
	items := make([]model.Item, 0, len(picked))
	var events []trace.Event
	for i, e := range picked {
		e.receipt.State = ReceiptDelivered
		e.receipt.Seq = baseSeq + i
		items = append(items, projectInput(e))
		events = append(events, trace.Event{Type: "input.delivered",
			Data: mustJSON(map[string]any{"receipt": e.receipt})})
	}
	// Drop delivered entries from the pending queue (kept in byReq/byInput).
	keep := q[:0]
	for _, e := range q {
		if e.receipt.State == ReceiptAccepted {
			keep = append(keep, e)
		}
	}
	m.queues[act.target] = keep
	m.mu.Unlock()
	_ = m.s.emit(ctx, events...)
	return items
}

func projectInput(e *mailEntry) model.Item {
	origin := &model.InputOrigin{CallerID: e.origin.ID, Mode: e.receipt.Mode}
	if e.origin.Kind == "agent" {
		origin.SourceNode = e.origin.Attrs["node"]
		replyTo := ""
		if addr := e.origin.Attrs["address"]; addr != "" {
			replyTo = fmt.Sprintf(` reply_to="%s"`, addr)
		}
		return model.Item{Kind: model.KindNotice, InputID: e.receipt.InputID, Origin: origin,
			Text: fmt.Sprintf(`<notice kind="agent_message" from="%s"%s mode="%s">%s</notice>`,
				e.origin.ID, replyTo, e.receipt.Mode, e.text)}
	}
	return model.Item{Kind: model.KindUser, InputID: e.receipt.InputID, Origin: origin, Text: e.text}
}

// cancel marks a pending input cancelled (§6.6: never silently discarded).
func (m *mailbox) cancel(ctx context.Context, inputID, reason string) (Receipt, bool) {
	m.mu.Lock()
	e, ok := m.byInput[inputID]
	if !ok || e.receipt.State != ReceiptAccepted {
		m.mu.Unlock()
		return Receipt{}, false
	}
	e.receipt.State = ReceiptCancelled
	e.receipt.Code = reason
	r := e.receipt
	q := m.queues[r.Target]
	keep := q[:0]
	for _, x := range q {
		if x.receipt.State == ReceiptAccepted {
			keep = append(keep, x)
		}
	}
	m.queues[r.Target] = keep
	m.mu.Unlock()
	_ = m.s.emit(ctx, trace.Event{Type: "input.cancelled", Data: mustJSON(map[string]any{"receipt": r})})
	return r, true
}

// list returns receipts (pending first-class) for one target.
func (m *mailbox) list(target string) []Receipt {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Receipt
	for _, e := range m.queues[target] {
		out = append(out, e.receipt)
	}
	return out
}

func (m *mailbox) receipt(inputID string) (Receipt, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byInput[inputID]
	if !ok {
		return Receipt{}, false
	}
	return e.receipt, true
}

// SendInput queues input for a thread or handle (§6.6). For handles it
// also implements wake/interrupt semantics via the control service.
func (s *Host) SendInput(ctx context.Context, req SendRequest) (Receipt, error) {
	if s.isClosed() {
		return Receipt{State: ReceiptRejected, Code: "session_closed"}, fmt.Errorf("host: closed")
	}
	if req.Origin.ID == "" {
		if c, ok := CallerFromContext(ctx); ok {
			req.Origin = c
		} else {
			req.Origin = s.root
		}
	}
	if h, ok := s.handles.get(req.Target); ok {
		return s.sendToHandle(ctx, h, req)
	}
	target := threadKey(req.Target)
	if err := s.requireThreadPeer(ctx, target); err != nil {
		return Receipt{Target: target, State: ReceiptRejected, Code: "forbidden"}, err
	}
	// A steer against the primary/named thread names its live activation;
	// callers may pass ExpectedActivation directly.
	return s.mailbox.accept(ctx, target, req), nil
}

// Inputs lists pending receipts for a target (handle ID or thread name).
func (s *Host) Inputs(target string) []Receipt {
	if h, ok := s.handles.get(target); ok {
		return s.mailbox.list(h.threadKey)
	}
	return s.mailbox.list(threadKey(target))
}

// CancelInput cancels a pending input by ID.
func (s *Host) CancelInput(ctx context.Context, inputID string) (Receipt, bool) {
	return s.mailbox.cancel(ctx, inputID, "cancelled_by_caller")
}

// InputReceipt returns the current receipt for an input ID.
func (s *Host) InputReceipt(inputID string) (Receipt, bool) {
	return s.mailbox.receipt(inputID)
}
