package host

// The session control service (§6.5): the sole implementation of the
// interactive subagent lifecycle. Loop built-ins (agent, agent_*,
// workflow), Starlark functions and registry capabilities are adapters
// to the methods here. An AgentHandle identifies a session-owned thread
// and its owner, distinct from an execution node; each activation is a
// new node with a previous_node link. Completed nodes never reopen.

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

// Handle states (§6.5).
const (
	HandleIdle         = "idle"
	HandleRunning      = "running"
	HandleInterrupting = "interrupting"
	HandleClosed       = "closed"
)

// HandleInfo is the authorized view of one handle (§6.5 status/list).
type HandleInfo struct {
	ID            string  `json:"id"`
	Def           string  `json:"def"`
	Owner         string  `json:"owner"`
	Conversation  string  `json:"conversation,omitempty"`
	State         string  `json:"state"`
	Activation    string  `json:"activation,omitempty"` // current node ID
	PreviousNode  string  `json:"previous_node,omitempty"`
	PendingInputs int     `json:"pending_inputs"`
	OneShot       bool    `json:"one_shot,omitempty"`
	LastResult    *Result `json:"last_result,omitempty"`
}

type handle struct {
	mu        sync.Mutex
	id        string
	defName   string
	owner     string
	conv      string // conversation whose agents may message it (§6.7)
	threadKey string
	state     string
	oneShot   bool

	// activation template: how to run the next activation.
	spec   ExecSpec
	parent *nodeCtx // nil for root-spawned: a fresh run per activation
	// anchor carries the host values (scope, observers) activations run
	// under: the spawn's ctx, replaced by each agent that wakes it.
	anchor context.Context

	currentNode string
	prevNode    string
	cancel      context.CancelFunc
	done        chan struct{}
	last        *Result
}

type handleStore struct {
	mu sync.Mutex
	m  map[string]*handle
}

func newHandleStore() *handleStore { return &handleStore{m: map[string]*handle{}} }

func (hs *handleStore) get(id string) (*handle, bool) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	h, ok := hs.m[id]
	return h, ok
}

func (hs *handleStore) put(h *handle) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.m[h.id] = h
}

func (hs *handleStore) all() []*handle {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	out := make([]*handle, 0, len(hs.m))
	for _, h := range hs.m {
		out = append(out, h)
	}
	return out
}

// RequireAgentAccess validates one operation on a handle (§6.5, §6.7).
// The root caller may do anything. An agent controls (interrupt, resume,
// close) only handles it owns, and may message, inspect and wait on any
// handle in its own conversation.
func (s *Host) RequireAgentAccess(ctx context.Context, h *handle, control bool) error {
	nc, inNode := nodeCtxFrom(ctx)
	if !inNode {
		return nil // root caller
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if control {
		if h.owner == nc.caller.ID {
			return nil
		}
		return model.Errorf(model.CodeDenied, "not_owner", "host: caller %s may not control handle %s owned by %s", nc.caller.ID, h.id, h.owner)
	}
	if h.conv == nc.conv {
		return nil
	}
	return model.Errorf(model.CodeNotFound, "unknown_handle", "host: no agent %q in this conversation", h.id)
}

func (h *handle) info(pending int) HandleInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return HandleInfo{ID: h.id, Def: h.defName, Owner: h.owner, Conversation: h.conv,
		State: h.state, Activation: h.currentNode, PreviousNode: h.prevNode,
		PendingInputs: pending, OneShot: h.oneShot, LastResult: h.last}
}

func (s *Host) emitState(ctx context.Context, h *handle, op string) {
	inf := h.info(s.mailbox.pendingCount(h.threadKey))
	_ = s.emit(ctx, trace.Event{Type: "agent.state", NodeID: inf.Activation,
		Data: mustJSON(map[string]any{"op": op, "handle": inf})})
	s.changed.fire()
}

// SpawnSpec is a spawn request: an ExecSpec plus handle options.
type SpawnSpec struct {
	ExecSpec
	OneShot bool
}

// Spawn creates a reusable handle and schedules its first activation
// (§6.5). It returns handle info immediately; the activation node ID is
// filled in by Status/Wait once scheduled. A handle spawned by an agent
// joins that agent's conversation, and the agent waits for it before
// stopping (§6.7).
func (s *Host) Spawn(ctx context.Context, spec SpawnSpec) (HandleInfo, error) {
	if s.isClosed() {
		return HandleInfo{}, model.Errorf(model.CodeCancelled, "session_closed", "host: closed")
	}
	nc, inNode := nodeCtxFrom(ctx)
	name := specName(spec.ExecSpec)
	owner, conv := s.root.ID, conversationFrom(ctx)
	var parent *nodeCtx
	if inNode {
		if err := requireDelegate(nc, name); err != nil {
			return HandleInfo{}, err
		}
		owner, conv = nc.caller.ID, nc.conv
		parent = nc
	}
	h := &handle{
		id: model.NewID(), defName: name, owner: owner,
		state: HandleIdle, oneShot: spec.OneShot,
		spec: spec.ExecSpec, parent: parent, anchor: ctx,
	}
	h.threadKey = "handle:" + h.id
	if conv == "" {
		conv = h.threadKey
	}
	h.conv = conv
	s.handles.put(h)
	if inNode {
		nc.awaits.add(h.id)
	}
	s.emitState(ctx, h, "spawn")
	s.startActivation(h, spec.Task, false)
	return h.info(s.mailbox.pendingCount(h.threadKey)), nil
}

// startActivation launches one activation goroutine. Intersect authority:
// the stored parent nodeCtx view is re-narrowed by execNode each time, so
// reuse never restores revoked capabilities; config is resolved per run.
// An activation runs in its parent's run while that run is live, else in
// a fresh run with the parent's lineage (a handle woken after the turn
// that started it, §6.7).
func (s *Host) startActivation(h *handle, task string, armFollow bool) {
	h.mu.Lock()
	if h.state == HandleClosed || h.state == HandleRunning || h.state == HandleInterrupting {
		h.mu.Unlock()
		return
	}
	h.state = HandleRunning
	anchor := h.anchor
	if anchor == nil {
		anchor = s.baseCtx
	}
	ctx, cancel := context.WithCancel(detachNode(context.WithoutCancel(anchor)))
	stopOnClose := context.AfterFunc(s.baseCtx, cancel)
	h.cancel = cancel
	done := make(chan struct{})
	h.done = done
	spec := h.spec
	parent := h.parent
	spec.Task = task
	spec.nodeID = model.NewID()
	h.currentNode = spec.nodeID
	spec.handleID = h.id
	spec.conv = h.conv
	spec.threadKey = h.threadKey
	spec.mailTarget = h.threadKey
	spec.armFollow = armFollow
	spec.prevNode = h.prevNode
	h.mu.Unlock()

	go func() {
		defer close(done)
		defer stopOnClose()
		defer cancel()
		var res Result
		var err error
		switch {
		case parent == nil:
			res, err = s.ExecAgent(ctx, spec)
		case parent.run.ended.Load():
			res, err = s.execInFreshRun(ctx, parent, spec)
		default:
			res, err = s.execNode(withNodeCtx(ctx, parent), parent, spec, false)
		}
		if err != nil && res.Status == "" {
			res.Status = "failed: " + errCode(err)
		}
		h.mu.Lock()
		h.last = &res
		h.prevNode = res.NodeID
		h.currentNode = res.NodeID
		closed := h.state == HandleClosed || h.oneShot
		if closed {
			h.state = HandleClosed
		} else {
			h.state = HandleIdle
		}
		h.cancel = nil
		h.mu.Unlock()
		s.emitState(context.WithoutCancel(ctx), h, "activation_end")
		// Input that wants a wake, accepted while running but after the
		// last delivery, wakes the now-idle handle.
		if !closed && s.mailbox.wantsWake(h.threadKey) {
			s.startActivation(h, "", s.mailbox.hasFollowUp(h.threadKey))
		}
	}()
	// Record the scheduled activation node once execNode allocates it is
	// not knowable here; currentNode is set on completion and via the
	// mailbox activation registry while running (see activationOf).
	s.emitState(s.baseCtx, h, "activation_start")
}

// execInFreshRun runs a handle activation whose parent's run has ended:
// a new run (budget, slots, config snapshot) under the same lineage, so
// the view ceiling, delegates and conversation carry over.
func (s *Host) execInFreshRun(ctx context.Context, parent *nodeCtx, spec ExecSpec) (Result, error) {
	run := s.newRun(ctx)
	p := &nodeCtx{run: run, nodeID: parent.nodeID, parentID: parent.parentID,
		depth: parent.depth, caller: parent.caller, owner: parent.owner,
		view: parent.view, delegates: parent.delegates, workflows: parent.workflows,
		layers: parent.layers, handleID: parent.handleID, conv: parent.conv,
		address: parent.address, inbox: parent.inbox, awaits: newAwaitSet()}
	ctx = withNodeCtx(ctx, p)
	_ = s.emit(ctx, trace.Event{Type: "run.start", RunID: run.id,
		Data: mustJSON(map[string]any{"workflow": "agent-wake", "agent": specName(spec), "handle": spec.handleID})})
	defer func() {
		run.end()
		_ = s.emit(context.WithoutCancel(ctx), trace.Event{Type: "run.end", RunID: run.id,
			Data: mustJSON(run.budget.snapshot())})
	}()
	return s.execNode(ctx, p, spec, false)
}

func errCode(err error) string {
	if err == nil {
		return ""
	}
	return "exec"
}

// activationOf returns the live node ID writing a handle's thread.
func (s *Host) activationOf(h *handle) string {
	if a := s.mailbox.activation(h.threadKey); a != "" {
		return a
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.currentNode
}

func (s *Host) sendToHandle(ctx context.Context, h *handle, req SendRequest) (Receipt, error) {
	if err := s.RequireAgentAccess(ctx, h, req.Interrupt); err != nil {
		return Receipt{State: ReceiptRejected, Code: "forbidden"}, err
	}
	nc, inNode := nodeCtxFrom(ctx)
	if inNode && nc.handleID == h.id {
		return Receipt{State: ReceiptRejected, Code: "forbidden"}, model.Errorf(model.CodeInvalid, "self_message", "host: an agent cannot message itself")
	}
	if req.Mode == ModeSteer || req.ExpectedActivation == "" {
		if live := s.activationOf(h); req.ExpectedActivation == "" {
			req.ExpectedActivation = live
		}
	}
	// Steer validates against the mailbox target activation below.
	r := s.mailbox.accept(ctx, h.threadKey, req)
	if r.State != ReceiptAccepted {
		return r, nil
	}
	if req.Interrupt {
		// Interrupt-then-steer (§6.6): persisted above; interrupt and
		// settle, then start a new activation with that input.
		_, _ = s.Interrupt(ctx, h.id)
		s.startActivation(h, "", true)
		return r, nil
	}
	if req.Mode == ModeFollowUp || req.Wake {
		// Wake an idle (not closed) handle; a plain message never does
		// (§6.5/§6.6). An agent that wakes a handle anchors the
		// activation in its own context and waits for it (§6.7).
		h.mu.Lock()
		idle := h.state == HandleIdle
		if idle && inNode {
			h.anchor = ctx
		}
		h.mu.Unlock()
		if idle {
			if inNode {
				nc.awaits.add(h.id)
			}
			s.startActivation(h, "", req.Mode == ModeFollowUp)
		}
	}
	return r, nil
}

// Interrupt cancels the current activation and its subtree, waits for
// settlement, then transitions to idle (§6.5).
func (s *Host) Interrupt(ctx context.Context, handleID string) (HandleInfo, error) {
	h, ok := s.handles.get(handleID)
	if !ok {
		return HandleInfo{}, unknownHandle(handleID)
	}
	if err := s.RequireAgentAccess(ctx, h, true); err != nil {
		return HandleInfo{}, err
	}
	_ = s.emit(ctx, trace.Event{Type: "agent.control",
		Data: mustJSON(map[string]any{"op": "interrupt", "handle": handleID})})
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	if h.state == HandleRunning {
		h.state = HandleInterrupting
	}
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return h.info(s.mailbox.pendingCount(h.threadKey)), ctx.Err()
		}
	}
	h.mu.Lock()
	if h.state == HandleInterrupting {
		h.state = HandleIdle
	}
	h.mu.Unlock()
	s.emitState(ctx, h, "interrupt_settled")
	return h.info(s.mailbox.pendingCount(h.threadKey)), nil
}

// Resume reopens a closed handle as idle, restoring its transcript and
// pending inputs. It does not repeat previous work (§6.5).
func (s *Host) Resume(ctx context.Context, handleID string) (HandleInfo, error) {
	h, ok := s.handles.get(handleID)
	if !ok {
		return HandleInfo{}, unknownHandle(handleID)
	}
	if err := s.RequireAgentAccess(ctx, h, true); err != nil {
		return HandleInfo{}, err
	}
	h.mu.Lock()
	if h.state == HandleClosed {
		h.state = HandleIdle
	}
	h.mu.Unlock()
	s.emitState(ctx, h, "resume")
	if !s.isClosed() && s.mailbox.wantsWake(h.threadKey) {
		s.startActivation(h, "", s.mailbox.hasFollowUp(h.threadKey))
	}
	return h.info(s.mailbox.pendingCount(h.threadKey)), nil
}

// CloseHandle interrupts and settles first, then releases runtime
// resources; history and pending inputs are retained (§6.5).
func (s *Host) CloseHandle(ctx context.Context, handleID string) (HandleInfo, error) {
	h, ok := s.handles.get(handleID)
	if !ok {
		return HandleInfo{}, unknownHandle(handleID)
	}
	if err := s.RequireAgentAccess(ctx, h, true); err != nil {
		return HandleInfo{}, err
	}
	h.mu.Lock()
	running := h.state == HandleRunning
	h.mu.Unlock()
	if running {
		if _, err := s.Interrupt(ctx, handleID); err != nil {
			return HandleInfo{}, err
		}
	}
	h.mu.Lock()
	h.state = HandleClosed
	h.mu.Unlock()
	s.emitState(ctx, h, "close")
	return h.info(s.mailbox.pendingCount(h.threadKey)), nil
}

// Status returns one authorized handle (§6.5).
func (s *Host) Status(ctx context.Context, handleID string) (HandleInfo, error) {
	h, ok := s.handles.get(handleID)
	if !ok {
		return HandleInfo{}, unknownHandle(handleID)
	}
	if err := s.RequireAgentAccess(ctx, h, false); err != nil {
		return HandleInfo{}, err
	}
	inf := h.info(s.mailbox.pendingCount(h.threadKey))
	if a := s.activationOf(h); a != "" {
		inf.Activation = a
	}
	return inf, nil
}

// ListAgents returns handles the caller may see (§6.5): every handle for
// the root caller, the caller's conversation for an agent (§6.7).
func (s *Host) ListAgents(ctx context.Context) []HandleInfo {
	var out []HandleInfo
	for _, h := range s.handles.all() {
		if err := s.RequireAgentAccess(ctx, h, false); err == nil {
			inf := h.info(s.mailbox.pendingCount(h.threadKey))
			if a := s.activationOf(h); a != "" {
				inf.Activation = a
			}
			out = append(out, inf)
		}
	}
	return out
}

// WaitResult is one handle's observed state from Wait.
type WaitResult struct {
	Info    HandleInfo `json:"info"`
	Timeout bool       `json:"timeout"`
	// MessageWaiting: the wait ended early because the waiting agent
	// received a message (§6.7); the handle may still be running.
	MessageWaiting bool `json:"message_waiting,omitempty"`
}

// Wait blocks until every handle is non-running or the timeout passes.
// Timeout does not cancel work; waiting on an idle handle returns idle.
// An agent's wait also ends when it receives a message, so it can answer
// the agents it is waiting on (§6.7).
func (s *Host) Wait(ctx context.Context, handleIDs []string, timeout time.Duration) ([]WaitResult, error) {
	deadline := time.Now().Add(timeout)
	inbox := callerInbox(ctx)
	nc, inNode := nodeCtxFrom(ctx)
	out := make([]WaitResult, len(handleIDs))
	for i, id := range handleIDs {
		h, ok := s.handles.get(id)
		if !ok {
			return nil, unknownHandle(id)
		}
		if err := s.RequireAgentAccess(ctx, h, false); err != nil {
			return nil, err
		}
		for {
			changed := s.changed.wait()
			if !h.running() {
				if inNode {
					nc.awaits.remove(id) // collected: Join need not wait on it
				}
				break
			}
			if inbox != "" && s.mailbox.pendingCount(inbox) > 0 {
				out[i].MessageWaiting = true
				break
			}
			remain := time.Until(deadline)
			if remain <= 0 {
				out[i].Timeout = true
				break
			}
			timer := time.NewTimer(remain)
			select {
			case <-changed:
			case <-ctx.Done():
				timer.Stop()
				return out, ctx.Err()
			case <-timer.C:
			}
			timer.Stop()
		}
		inf, _ := s.Status(ctx, id)
		out[i].Info = inf
	}
	return out, nil
}

func requireDelegate(nc *nodeCtx, name string) error {
	for _, d := range nc.delegates {
		if d == name || d == "*" {
			return nil
		}
	}
	return model.Errorf(model.CodeDenied, "not_delegate", "host: %q is not an allowed delegate of %s", name, nc.caller.ID)
}

// --- loop.Delegate adapter (§4.3 built-ins) ---

type delegateImpl struct {
	s  *Host
	nc *nodeCtx
}

type agentArgs struct {
	Name      string          `json:"name"`
	Task      string          `json:"task"`
	Profile   string          `json:"profile"`
	To        string          `json:"to"`
	Handle    string          `json:"handle"`
	Handles   []string        `json:"handles"`
	Text      string          `json:"text"`
	TimeoutMS int             `json:"timeout_ms"`
	Workflow  string          `json:"workflow"`
	Source    string          `json:"source"`
	Args      json.RawMessage `json:"args"`
}

func builtin(name, effect, desc, schema string) model.Tool {
	return model.Tool{Name: name, Effect: effect, Description: desc, Schema: json.RawMessage(schema)}
}

const handleSchema = `{"type":"object","required":["handle"],"properties":{"handle":{"type":"string","description":"agent id from agent_list"}},"additionalProperties":false}`

// messagingTools are offered to every agent (§6.7).
var messagingTools = []model.Tool{
	builtin("agent_list", "read-only",
		"List the agents in this conversation: id (the address for agent_send), definition, state (running, idle or closed), whether it is you, and its last status.",
		`{"type":"object","properties":{},"additionalProperties":false}`),
	builtin("agent_send", "mutating",
		"Message another agent in this conversation by its id from agent_list. A running agent sees it at its next step; an idle (finished) agent is woken to handle it and goes idle again when done, and you wait for it before you finish. Messages arrive as <notice kind=\"agent_message\" reply_to=\"...\">: answer with agent_send to that reply_to id. Send questions, answers and requests, not acknowledgements.",
		`{"type":"object","required":["to","text"],"properties":{"to":{"type":"string","description":"agent id from agent_list"},"text":{"type":"string"}},"additionalProperties":false}`),
	builtin("agent_status", "read-only",
		"Show one agent in this conversation: state, pending inputs and its last result.", handleSchema),
	builtin("agent_wait", "read-only",
		"Wait for agents in this conversation to finish their current work, then return their state and last results. Returns early (message_waiting) when you receive a message, so you can answer it.",
		`{"type":"object","required":["handles"],"properties":{"handles":{"type":"array","items":{"type":"string"}},"timeout_ms":{"type":"integer","description":"default 30000"}},"additionalProperties":false}`),
}

// delegationTools are offered when the definition names delegates.
var delegationTools = []model.Tool{
	builtin("agent", "mutating",
		"Run a delegate agent on a task and wait for its result. The agent stays in this conversation afterwards (its handle is in the result): agent_send it follow-up questions. Returns early with status running if you receive a message meanwhile; agent_wait for the result after answering.",
		`{"type":"object","required":["name","task"],"properties":{"name":{"type":"string","description":"a delegate of this agent"},"task":{"type":"string"},"profile":{"type":"string"}},"additionalProperties":false}`),
	builtin("agent_spawn", "mutating",
		"Start a delegate agent on a task in the background and return its handle at once. Collect it with agent_wait; you also wait for it before you finish.",
		`{"type":"object","required":["name","task"],"properties":{"name":{"type":"string","description":"a delegate of this agent"},"task":{"type":"string"},"profile":{"type":"string"}},"additionalProperties":false}`),
	builtin("agent_interrupt", "mutating", "Interrupt an agent you started; it goes idle.", handleSchema),
	builtin("agent_resume", "mutating", "Reopen an agent you closed.", handleSchema),
	builtin("agent_close", "mutating", "Close an agent you started; it can no longer be woken until resumed.", handleSchema),
}

var workflowTool = builtin("workflow", "mutating", "Run a workflow: a catalogued one by name, or Starlark source you write.",
	`{"type":"object","properties":{"workflow":{"type":"string","description":"catalogued workflow name"},"source":{"type":"string","description":"Starlark workflow source, instead of a name"},"args":{"type":"object"}},"additionalProperties":false}`)

// Tools lists this node's built-ins: messaging always, delegation when
// the definition names delegates, workflow when it enables workflows.
func (d *delegateImpl) Tools() []model.Tool {
	out := append([]model.Tool(nil), messagingTools...)
	if len(d.nc.delegates) > 0 {
		out = append(out, delegationTools...)
	}
	if d.nc.workflows {
		out = append(out, workflowTool)
	}
	return out
}

// errResult is a built-in's failure: a coded session error keeps its
// code and reason (see model.Failure).
func errResult(err error) (model.ToolResult, error) {
	return model.Failure(err), nil
}

// unknownHandle is the error for a handle this session does not hold.
func unknownHandle(id string) error {
	return model.Errorf(model.CodeNotFound, "unknown_handle", "host: unknown handle %s", id)
}

func jsonResult(v any) (model.ToolResult, error) {
	return model.ToolResult{Text: string(mustJSON(v))}, nil
}

// Call serves one advertised built-in.
func (d *delegateImpl) Call(ctx context.Context, name string, args json.RawMessage) (model.ToolResult, error) {
	var a agentArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return errResult(model.Errorf(model.CodeInvalid, "invalid_args", "invalid arguments: %v", err))
		}
	}
	s := d.s
	// Blocking calls release this node's slot while awaiting others (§11).
	releaseWait := func() func() {
		if d.nc.slot != nil && d.nc.slot.releaseIfHeld() {
			return func() { d.nc.slot.reacquire(ctx) }
		}
		return func() {}
	}
	switch name {
	case "agent":
		return d.agent(ctx, a)
	case "workflow":
		defer releaseWait()()
		src := a.Workflow
		if src == "" {
			src = a.Source
		}
		res, err := s.RunWorkflow(ctx, src, decodeObject(a.Args))
		if err != nil {
			return errResult(err)
		}
		return jsonResult(res)
	case "agent_spawn":
		inf, err := s.Spawn(ctx, SpawnSpec{ExecSpec: ExecSpec{Name: a.Name, Task: a.Task, Profile: a.Profile}})
		if err != nil {
			return errResult(err)
		}
		return jsonResult(inf)
	case "agent_send":
		r, err := s.SendInput(ctx, SendRequest{Target: addressThread(a.To), Text: a.Text, Wake: true})
		if err != nil {
			return errResult(err)
		}
		if r.State == ReceiptRejected {
			return model.ToolResult{IsError: true, Text: string(mustJSON(r))}, nil
		}
		return jsonResult(r)
	case "agent_interrupt":
		defer releaseWait()()
		return infoResult(s.Interrupt(ctx, a.Handle))
	case "agent_resume":
		return infoResult(s.Resume(ctx, a.Handle))
	case "agent_close":
		defer releaseWait()()
		return infoResult(s.CloseHandle(ctx, a.Handle))
	case "agent_status":
		return infoResult(s.Status(ctx, a.Handle))
	case "agent_list":
		return jsonResult(s.Peers(ctx))
	case "agent_wait":
		defer releaseWait()()
		to := time.Duration(a.TimeoutMS) * time.Millisecond
		if to <= 0 {
			to = 30 * time.Second
		}
		res, err := s.Wait(ctx, a.Handles, to)
		if err != nil {
			return errResult(err)
		}
		return jsonResult(res)
	default:
		return errResult(model.Errorf(model.CodeNotFound, "unknown_tool", "unknown built-in %q", name))
	}
}

func infoResult(inf HandleInfo, err error) (model.ToolResult, error) {
	if err != nil {
		return errResult(err)
	}
	return jsonResult(inf)
}

func decodeObject(raw json.RawMessage) map[string]any {
	var m map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

// agent implements the blocking agent built-in over a reusable handle, so
// the delegate stays listed and wakeable afterwards (§6.7). It releases
// this node's slot while awaiting (§11), cancels the delegate if this
// node is cancelled, and returns early when this node receives a message.
func (d *delegateImpl) agent(ctx context.Context, a agentArgs) (model.ToolResult, error) {
	s := d.s
	inf, err := s.Spawn(ctx, SpawnSpec{ExecSpec: ExecSpec{Name: a.Name, Task: a.Task, Profile: a.Profile}})
	if err != nil {
		return errResult(err)
	}
	h, _ := s.handles.get(inf.ID)
	if d.nc.slot != nil && d.nc.slot.releaseIfHeld() {
		defer d.nc.slot.reacquire(ctx)
	}
	for {
		changed := s.changed.wait()
		if !h.running() {
			break
		}
		if s.mailbox.pendingCount(d.nc.inbox) > 0 {
			return jsonResult(map[string]any{"status": "running", "handle": h.id,
				"note": "You received a message (it follows this result). " + h.defName +
					" is still running: answer if needed, then agent_wait for its result."})
		}
		select {
		case <-changed:
		case <-ctx.Done():
			h.mu.Lock()
			cancel := h.cancel
			h.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			return model.Fail(model.CodeCancelled, "cancelled", "cancelled while waiting; the agent was cancelled too"), nil
		}
	}
	d.nc.awaits.remove(h.id)
	res := h.info(0).LastResult
	if res == nil {
		res = &Result{}
	}
	out := map[string]any{"status": res.Status, "text": res.Text, "node": res.NodeID, "handle": h.id}
	if len(res.Output) > 0 {
		out["output"] = json.RawMessage(res.Output)
	}
	return model.ToolResult{Text: string(mustJSON(out)), IsError: res.Status != loop.StatusDone}, nil
}
