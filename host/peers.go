package host

// Agent-to-agent messaging (§6.7). Every agent belongs to one
// conversation; agents in a conversation can list each other and send
// each other messages in any direction. Control (interrupt, resume,
// close) stays with a handle's owner, and spawning with the definition's
// delegates. A message an agent sends to a finished handle wakes it to
// handle the message; it goes idle again when done. An agent does not
// stop while agents it started or woke are still running (Join), so the
// agent that asked is still there to receive the answer.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

type conversationKey struct{}

// WithConversation scopes agents started under ctx to one conversation:
// they can discover and message each other and no other agent. Hosts set
// it per chat. Without it a top-level run's conversation is its thread
// (a fresh delegated or workflow agent's is its run).
func WithConversation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, conversationKey{}, id)
}

func conversationFrom(ctx context.Context) string {
	id, _ := ctx.Value(conversationKey{}).(string)
	return id
}

// primaryAddress is how agents name the primary thread's agent.
const primaryAddress = "primary"

// threadAddress is the peer address of the agent writing thread key.
func threadAddress(key string) string {
	if key == threadKey("") {
		return primaryAddress
	}
	return strings.TrimPrefix(key, "thread:")
}

// addressThread maps a thread peer's address back to SendInput's target.
func addressThread(addr string) string {
	if addr == primaryAddress {
		return ""
	}
	return addr
}

// signal wakes every waiter whenever mailbox or agent state changes.
// Take the channel before checking state so no change is missed.
type signal struct {
	mu sync.Mutex
	ch chan struct{}
}

func newSignal() *signal { return &signal{ch: make(chan struct{})} }

func (sg *signal) wait() <-chan struct{} {
	sg.mu.Lock()
	defer sg.mu.Unlock()
	return sg.ch
}

func (sg *signal) fire() {
	sg.mu.Lock()
	defer sg.mu.Unlock()
	close(sg.ch)
	sg.ch = make(chan struct{})
}

// awaitSet is the handles one node started or woke and has not yet
// collected; Join waits for the running ones.
type awaitSet struct {
	mu  sync.Mutex
	ids map[string]bool
}

func newAwaitSet() *awaitSet { return &awaitSet{ids: map[string]bool{}} }

func (a *awaitSet) add(id string) {
	a.mu.Lock()
	a.ids[id] = true
	a.mu.Unlock()
}

func (a *awaitSet) remove(id string) {
	a.mu.Lock()
	delete(a.ids, id)
	a.mu.Unlock()
}

func (a *awaitSet) list() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.ids))
	for id := range a.ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// threadPeer is the agent writing a named or primary thread: a host's
// chat agent, or a workflow agent on a thread. It is addressable but not
// a handle: hosts own its turns, so a message to an idle thread agent
// waits for its next turn instead of waking it.
type threadPeer struct {
	key, conv, def string
	node           string // live activation; "" when idle
	last           *Result
}

type threadPeers struct {
	mu sync.Mutex
	m  map[string]*threadPeer // thread key -> peer
}

func newThreadPeers() *threadPeers { return &threadPeers{m: map[string]*threadPeer{}} }

func (tp *threadPeers) start(key, conv, def, node string) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	p, ok := tp.m[key]
	if !ok {
		p = &threadPeer{key: key}
		tp.m[key] = p
	}
	p.conv, p.def, p.node = conv, def, node
}

func (tp *threadPeers) end(key string, res Result) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	if p, ok := tp.m[key]; ok {
		p.node, p.last = "", &res
	}
}

func (tp *threadPeers) get(key string) (threadPeer, bool) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	p, ok := tp.m[key]
	if !ok {
		return threadPeer{}, false
	}
	return *p, true
}

func (tp *threadPeers) inConversation(conv string) []threadPeer {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	var out []threadPeer
	for _, p := range tp.m {
		if p.conv == conv {
			out = append(out, *p)
		}
	}
	return out
}

// Peer is one agent the caller can message (agent_list).
type Peer struct {
	ID            string `json:"id"`   // agent_send address
	Kind          string `json:"kind"` // handle | thread
	Def           string `json:"def"`
	State         string `json:"state"` // running | idle | closed
	You           bool   `json:"you,omitempty"`
	Owner         string `json:"owner,omitempty"`
	PendingInputs int    `json:"pending_inputs"`
	LastStatus    string `json:"last_status,omitempty"`
}

// Peers lists the agents in the calling node's conversation, itself
// included and marked. Outside a node it lists every agent.
func (s *Host) Peers(ctx context.Context) []Peer {
	nc, inNode := nodeCtxFrom(ctx)
	var out []Peer
	for _, inf := range s.ListAgents(ctx) {
		p := Peer{ID: inf.ID, Kind: "handle", Def: inf.Def, State: inf.State,
			Owner: inf.Owner, PendingInputs: inf.PendingInputs}
		if inf.LastResult != nil {
			p.LastStatus = inf.LastResult.Status
		}
		p.You = inNode && nc.address == inf.ID
		out = append(out, p)
	}
	var threads []threadPeer
	if inNode {
		threads = s.peers.inConversation(nc.conv)
	} else {
		s.peers.mu.Lock()
		for _, p := range s.peers.m {
			threads = append(threads, *p)
		}
		s.peers.mu.Unlock()
	}
	for _, tp := range threads {
		p := Peer{ID: threadAddress(tp.key), Kind: "thread", Def: tp.def,
			State: HandleIdle, PendingInputs: s.mailbox.pendingCount(tp.key)}
		if tp.node != "" {
			p.State = HandleRunning
		}
		if tp.last != nil {
			p.LastStatus = tp.last.Status
		}
		p.You = inNode && nc.address == p.ID
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// requireThreadPeer admits an in-node sender to a thread target only when
// that thread's agent is in the sender's conversation and is not the
// sender itself. Root callers (hosts, UIs) may target any thread.
func (s *Host) requireThreadPeer(ctx context.Context, key string) error {
	nc, inNode := nodeCtxFrom(ctx)
	if !inNode {
		return nil
	}
	if key == nc.inbox {
		return fmt.Errorf("host: an agent cannot message itself")
	}
	p, ok := s.peers.get(key)
	if !ok || p.conv != nc.conv {
		return fmt.Errorf("host: no agent %q in this conversation", threadAddress(key))
	}
	return nil
}

// callerInbox is the mailbox target of the calling node ("" outside one).
func callerInbox(ctx context.Context) string {
	if nc, ok := nodeCtxFrom(ctx); ok {
		return nc.inbox
	}
	return ""
}

// running reports whether a handle has a live activation.
func (h *handle) running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state == HandleRunning || h.state == HandleInterrupting
}

// join is the loop's Join (§6.7): at a would-be stop, wait while agents
// this node started or woke are running, releasing the execution slot.
// It returns when input is pending (delivered at the next boundary) or
// when awaited agents settle (their results as notices).
func (s *Host) join(ctx context.Context, nc *nodeCtx, act *activation) ([]model.Item, bool) {
	var running []*handle
	for _, id := range nc.awaits.list() {
		if h, ok := s.handles.get(id); ok && h.running() {
			running = append(running, h)
		}
	}
	if len(running) == 0 {
		return nil, false
	}
	if nc.slot != nil && nc.slot.releaseIfHeld() {
		defer nc.slot.reacquire(ctx)
	}
	ids := make([]string, len(running))
	for i, h := range running {
		ids[i] = h.id
	}
	_ = s.emit(ctx, trace.Event{Type: "node.waiting", RunID: nc.run.id, NodeID: nc.nodeID,
		Data: mustJSON(map[string]any{"reason": "agents", "handles": ids})})
	for {
		changed := s.changed.wait()
		if s.mailbox.pendingCount(act.target) > 0 {
			if s.mailbox.hasFollowUp(act.target) {
				act.arm()
			}
			return nil, true
		}
		var items []model.Item
		still := running[:0]
		for _, h := range running {
			if h.running() {
				still = append(still, h)
				continue
			}
			nc.awaits.remove(h.id)
			items = append(items, settledNotice(h))
		}
		if len(items) > 0 {
			return items, true
		}
		running = still
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// settledNotice reports a settled handle's latest result to an agent that
// was waiting on it.
func settledNotice(h *handle) model.Item {
	inf := h.info(0)
	status, text := "", ""
	if inf.LastResult != nil {
		status, text = inf.LastResult.Status, inf.LastResult.Text
		if text == "" && len(inf.LastResult.Output) > 0 {
			text = string(inf.LastResult.Output)
		}
	}
	return model.Item{Kind: model.KindNotice, Text: fmt.Sprintf(
		`<notice kind="agent_settled" from="%s" def="%s" status="%s">%s</notice>`,
		inf.ID, inf.Def, status, text)}
}
