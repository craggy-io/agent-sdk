package host

// Live event subscription (§7.2): durable events replay from the trace
// cursor, then bridge to live delivery by polling the sink from the
// same watermark (single cursor stream, so no replay/live duplicates).
// Stream deltas live in a bounded in-memory ring (persist_deltas records
// them durably instead); a reconnect outside the ring gets stream.gap.
// Each subscriber has a bounded queue: a slow subscriber is disconnected
// (channel closed) with its last delivered cursor in hand to resume —
// publishers never block on observers.

import (
	"context"
	"sync"
	"time"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

// Envelope is one delivered event. Exactly one of Event/Delta is set,
// except stream.gap/disconnect envelopes which carry only metadata.
type Envelope struct {
	Cursor    int64        `json:"cursor,omitempty"` // durable cursor (0 for deltas)
	Seq       int64        `json:"seq,omitempty"`    // monotonic live sequence
	Epoch     string       `json:"epoch,omitempty"`
	Type      string       `json:"type"`
	SessionID string       `json:"session_id,omitempty"`
	RunID     string       `json:"run_id,omitempty"`
	NodeID    string       `json:"node_id,omitempty"`
	Event     *trace.Event `json:"event,omitempty"`
	Delta     *model.Delta `json:"delta,omitempty"`
	Gap       bool         `json:"gap,omitempty"`
	// ResumeCursor is set on overflow disconnects: resubscribe After it.
	ResumeCursor int64  `json:"resume_cursor,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// EventOptions selects a subscription start point.
type EventOptions struct {
	After      int64 // durable cursor to replay after
	AfterDelta int64 // live delta sequence already seen (ring replay)
	Deltas     bool  // include live stream deltas
}

type ringEntry struct {
	seq   int64
	bytes int
	env   Envelope
}

type hub struct {
	s *Host

	mu       sync.Mutex
	seq      int64
	ring     []ringEntry
	ringUsed int
	subs     map[*subscriber]bool
	closed   bool
}

type subscriber struct {
	ch     chan Envelope
	deltas bool
	cursor int64 // last durable cursor queued (guarded by owner goroutine)

	mu     sync.Mutex // serializes send vs close
	closed bool
}

func (sub *subscriber) close() {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if !sub.closed {
		sub.closed = true
		close(sub.ch)
	}
}

func newHub(s *Host) *hub {
	return &hub{s: s, subs: map[*subscriber]bool{}}
}

func (h *hub) ringCap() int {
	if c := h.s.Config().Events.RingBytes; c > 0 {
		return c
	}
	return 8 << 20
}

func (h *hub) queueSize() int {
	if n := h.s.Config().Events.SubscriberEvents; n > 0 {
		return n
	}
	return 256
}

// publishDelta records a provisional delta (§7.2 streaming lifecycle).
func (h *hub) publishDelta(nodeID, runID string, d model.Delta) {
	cfg := h.s.Config()
	if cfg.Events.PersistDeltas {
		_ = h.s.emit(h.s.baseCtx, trace.Event{Type: "stream.delta", RunID: runID, NodeID: nodeID,
			Data: mustJSON(d)})
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.seq++
	dd := d
	env := Envelope{Seq: h.seq, Epoch: h.s.epoch, Type: "stream.delta",
		SessionID: h.s.id, RunID: runID, NodeID: nodeID, Delta: &dd}
	sz := len(d.Text) + 64
	h.ring = append(h.ring, ringEntry{seq: h.seq, bytes: sz, env: env})
	h.ringUsed += sz
	for len(h.ring) > 0 && h.ringUsed > h.ringCap() {
		h.ringUsed -= h.ring[0].bytes
		h.ring = h.ring[1:]
	}
	subs := make([]*subscriber, 0, len(h.subs))
	for sub := range h.subs {
		if sub.deltas {
			subs = append(subs, sub)
		}
	}
	h.mu.Unlock()
	for _, sub := range subs {
		h.deliver(sub, env)
	}
}

// deliver queues env without blocking; on overflow the subscriber is
// disconnected with its resume cursor (§7.2 backpressure).
func (h *hub) deliver(sub *subscriber, env Envelope) bool {
	sub.mu.Lock()
	if sub.closed {
		sub.mu.Unlock()
		return false
	}
	select {
	case sub.ch <- env:
		sub.mu.Unlock()
		return true
	default:
	}
	sub.mu.Unlock()
	h.mu.Lock()
	_, live := h.subs[sub]
	if live {
		delete(h.subs, sub)
	}
	h.mu.Unlock()
	if live {
		// Close is the disconnect signal; the durable cursor in the
		// last received envelope is the resume point (§7.2).
		sub.close()
	}
	return false
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for sub := range h.subs {
		sub.close()
	}
	h.subs = map[*subscriber]bool{}
}

// Events subscribes to durable trace events (and optionally live
// deltas), replaying after opts.After with no duplicates: one poller
// goroutine owns the cursor from replay through live delivery.
// Cancelling ctx ends the subscription only, never an agent (§7.2).
func (s *Host) Events(ctx context.Context, opts EventOptions) <-chan Envelope {
	h := s.hub
	sub := &subscriber{ch: make(chan Envelope, h.queueSize()), deltas: opts.Deltas, cursor: opts.After}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(sub.ch)
		return sub.ch
	}
	// Ring miss for deltas: explicit gap (§7.2 reconnect contract).
	var gap *Envelope
	if opts.Deltas && opts.AfterDelta > 0 && len(h.ring) > 0 && h.ring[0].seq > opts.AfterDelta+1 {
		g := Envelope{Type: "stream.gap", Epoch: s.epoch, SessionID: s.id, Gap: true,
			Reason: "delta ring no longer holds the requested sequence"}
		gap = &g
	}
	var replay []Envelope
	if opts.Deltas && opts.AfterDelta > 0 {
		for _, e := range h.ring {
			if e.seq > opts.AfterDelta {
				replay = append(replay, e.env)
			}
		}
	}
	h.subs[sub] = true
	h.mu.Unlock()

	go func() {
		if gap != nil && !h.deliver(sub, *gap) {
			return
		}
		for _, env := range replay {
			if !h.deliver(sub, env) {
				return
			}
		}
		cursor := opts.After
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			if s.sink == nil {
				// No durable stream; wait for cancellation/close.
				select {
				case <-ctx.Done():
					h.remove(sub)
					return
				case <-tick.C:
					if !h.isLive(sub) {
						return
					}
				}
				continue
			}
			events, next, err := s.sink.ReadEvents(ctx, cursor, 128)
			if err != nil {
				if ctx.Err() != nil {
					h.remove(sub)
					return
				}
				time.Sleep(5 * time.Millisecond)
				continue
			}
			for _, e := range events {
				if e.SessionID != "" && e.SessionID != s.id {
					continue
				}
				ev := e
				sub.cursor = ev.Cursor
				env := Envelope{Cursor: ev.Cursor, Epoch: s.epoch, Type: ev.Type,
					SessionID: ev.SessionID, RunID: ev.RunID, NodeID: ev.NodeID, Event: &ev}
				if !h.deliver(sub, env) {
					return // disconnected; deliver() closed the channel
				}
				cursor = ev.Cursor
			}
			if len(events) == 0 {
				select {
				case <-ctx.Done():
					h.remove(sub)
					return
				case <-tick.C:
					if !h.isLive(sub) {
						return
					}
				}
			}
			_ = next
		}
	}()
	return sub.ch
}

func (h *hub) isLive(sub *subscriber) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.subs[sub]
}

func (h *hub) remove(sub *subscriber) {
	h.mu.Lock()
	_, live := h.subs[sub]
	if live {
		delete(h.subs, sub)
	}
	h.mu.Unlock()
	if live {
		sub.close()
	}
}
