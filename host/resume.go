package host

// Projection rebuild (§7.2): mailbox state, thread contents and handle
// state are rebuildable from durable trace events; they are not
// independent competing stores. Resume replays the session's events
// into empty projections — threads from step.request item hashes (the
// append-only union, deduplicated by blob hash) plus the final
// step.response items, mailbox from input.* events, handles from
// agent.state (restored idle/closed; a handle recorded running comes
// back idle because its activation did not survive the process, with
// pending inputs intact). Workflow journal replay (§6.3) is separate.
//
// Calls a dead process left open are reconciled here (§6.3 step 3): a
// call.start with no call.end, recorded before this process's own
// host.start, gets a call.end settled outcome_unknown (is_error,
// "recovered": true) and its thread gets an outcome_unknown result in
// place of the projection's "not dispatched" placeholder, before any
// loop continues the thread. The tool may or may not have run.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

// openCall is one call.start seen while replaying, until its call.end.
type openCall struct {
	start     trace.Event
	call      model.ToolCall
	ended     bool
	recovered bool // ended by a recovery call.end (a previous Rebuild)
}

// outcomeUnknownText is the model-visible result of a reconciled call.
var outcomeUnknownText = model.Fail(model.CodeInternal, string(model.SettleOutcomeUnknown),
	"outcome unknown: the process stopped while this call was in flight; it may or may not have run, and side effects may be partial: check before repeating it").Text

type nodeMeta struct {
	thread string
	handle string
	def    string
}

// Rebuild rebuilds projections from the trace (named Rebuild, not
// Resume, because Resume is the §6.5 handle operation). runID, when
// non-empty, restricts thread rebuild to that run's nodes; mailbox and
// handles are session-scoped and always rebuilt in full.
func (s *Host) Rebuild(ctx context.Context, runID string) error {
	if s.sink == nil {
		return fmt.Errorf("host: no trace sink to resume from")
	}
	nodes := map[string]nodeMeta{}
	threadSeen := map[string]map[trace.Hash]bool{}
	threadItems := map[string][]model.Item{}
	addItems := func(key string, hashes []trace.Hash) {
		if key == "" {
			return
		}
		seen := threadSeen[key]
		if seen == nil {
			seen = map[trace.Hash]bool{}
			threadSeen[key] = seen
		}
		for _, hh := range hashes {
			if seen[hh] {
				continue
			}
			seen[hh] = true
			if it, err := trace.GetItem(ctx, s.sink, hh); err == nil {
				threadItems[key] = append(threadItems[key], it)
			}
		}
	}

	var (
		after    int64
		liveFrom int64 = -1 // cursor of this process's host.start
		calls          = map[string]*openCall{}
		order    []string
	)
	for {
		events, next, err := s.sink.ReadEvents(ctx, after, 256)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			break
		}
		after = next
		for _, e := range events {
			if e.SessionID != "" && e.SessionID != s.id {
				continue
			}
			switch e.Type {
			case "host.start":
				var d struct {
					Epoch string `json:"epoch"`
				}
				if json.Unmarshal(e.Data, &d) == nil && d.Epoch == s.epoch {
					liveFrom = e.Cursor
				}
			case "call.start", "call.end":
				if runID != "" && e.RunID != runID {
					continue
				}
				var d struct {
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Recovered bool   `json:"recovered"`
				}
				if json.Unmarshal(e.Data, &d) != nil || d.CallID == "" {
					continue
				}
				key := e.NodeID + "|" + d.CallID
				if e.Type == "call.start" {
					if calls[key] == nil {
						calls[key] = &openCall{start: e, call: model.ToolCall{ID: d.CallID, Name: d.Name}}
						order = append(order, key)
					}
				} else if c := calls[key]; c != nil {
					c.ended, c.recovered = true, d.Recovered
				}
			case "node.start":
				var d struct {
					Thread string `json:"thread"`
					Handle string `json:"handle"`
					Def    string `json:"def"`
				}
				_ = json.Unmarshal(e.Data, &d)
				nodes[e.NodeID] = nodeMeta{thread: d.Thread, handle: d.Handle, def: d.Def}
			case "step.request":
				if runID != "" && e.RunID != runID {
					continue
				}
				var d trace.StepRequestData
				if err := json.Unmarshal(e.Data, &d); err == nil {
					addItems(nodes[e.NodeID].thread, d.Items)
				}
			case "step.response":
				if runID != "" && e.RunID != runID {
					continue
				}
				var d struct {
					ItemHash string `json:"item"`
					Hash     string `json:"hash"`
				}
				_ = json.Unmarshal(e.Data, &d)
				hh := d.ItemHash
				if hh == "" {
					hh = d.Hash
				}
				if hh != "" {
					addItems(nodes[e.NodeID].thread, []trace.Hash{trace.Hash(hh)})
				}
			case "input.accepted", "input.delivered", "input.cancelled":
				s.replayInput(ctx, e)
			case "agent.state":
				s.replayHandle(e)
			}
		}
		if len(events) < 256 {
			break
		}
	}
	if err := s.reconcileCalls(ctx, calls, order, liveFrom, nodes, threadItems); err != nil {
		return err
	}
	for key, items := range threadItems {
		s.threads.restore(key, items)
	}
	return nil
}

// reconcileCalls settles calls a dead process left open (see the
// package comment). Calls started at or after liveFrom belong to this
// process and may still be running, so they are left alone; liveFrom <
// 0 (this host.start not found) treats every call as a dead
// process's. A call already reconciled by an earlier Rebuild emits
// nothing new, but its thread result is restored again (results are
// not in the trace until a later step.request carries them).
func (s *Host) reconcileCalls(ctx context.Context, calls map[string]*openCall, order []string, liveFrom int64, nodes map[string]nodeMeta, threadItems map[string][]model.Item) error {
	for _, key := range order {
		c := calls[key]
		if c.ended && !c.recovered {
			continue
		}
		if !c.ended {
			if liveFrom >= 0 && c.start.Cursor > liveFrom {
				continue
			}
			res := model.ToolResult{Text: outcomeUnknownText, IsError: true, Code: string(model.SettleOutcomeUnknown)}
			data := loop.CallEndData(c.call, res, model.SettleOutcomeUnknown)
			data["recovered"] = true
			ev := trace.Event{Type: "call.end", SessionID: c.start.SessionID,
				RunID: c.start.RunID, NodeID: c.start.NodeID, StepID: c.start.StepID,
				Data: mustJSON(data)}
			if err := s.emit(ctx, ev); err != nil {
				return fmt.Errorf("host: reconcile call %s: %w", c.call.ID, err)
			}
		}
		thread := nodes[c.start.NodeID].thread
		if thread == "" || hasResult(threadItems[thread], c.call.ID) || !hasCall(threadItems[thread], c.call.ID) {
			continue
		}
		threadItems[thread] = append(threadItems[thread], model.Item{
			Kind: model.KindToolResult, CallID: c.call.ID, IsError: true,
			Settled: model.SettleOutcomeUnknown, Text: outcomeUnknownText})
	}
	return nil
}

func hasResult(items []model.Item, callID string) bool {
	for _, it := range items {
		if it.Kind == model.KindToolResult && it.CallID == callID {
			return true
		}
	}
	return false
}

func hasCall(items []model.Item, callID string) bool {
	for _, it := range items {
		for _, c := range it.Calls {
			if c.ID == callID {
				return true
			}
		}
	}
	return false
}

func (s *Host) replayInput(ctx context.Context, e trace.Event) {
	var d struct {
		Receipt  Receipt          `json:"receipt"`
		BodyHash string           `json:"body_hash"`
		Origin   model.CallerInfo `json:"origin"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil || d.Receipt.InputID == "" {
		return
	}
	m := s.mailbox
	m.mu.Lock()
	defer m.mu.Unlock()
	// Replay deduplicates by InputID (§6.6).
	if prev, ok := m.byInput[d.Receipt.InputID]; ok {
		prev.receipt.State = d.Receipt.State
		prev.receipt.Seq = d.Receipt.Seq
		if prev.receipt.State != ReceiptAccepted {
			q := m.queues[prev.receipt.Target]
			keep := q[:0]
			for _, x := range q {
				if x.receipt.State == ReceiptAccepted {
					keep = append(keep, x)
				}
			}
			m.queues[prev.receipt.Target] = keep
		}
		return
	}
	if e.Type != "input.accepted" {
		return
	}
	text := ""
	if d.BodyHash != "" && s.sink != nil {
		if b, err := s.sink.GetBlob(ctx, trace.Hash(d.BodyHash)); err == nil {
			text = string(b)
		}
	}
	ent := &mailEntry{receipt: d.Receipt, text: text, origin: d.Origin}
	m.byInput[d.Receipt.InputID] = ent
	m.byReq[d.Receipt.Target+"|"+d.Receipt.RequestID] = ent
	if d.Receipt.State == ReceiptAccepted {
		m.queues[d.Receipt.Target] = append(m.queues[d.Receipt.Target], ent)
	}
}

func (s *Host) replayHandle(e trace.Event) {
	var d struct {
		Handle HandleInfo `json:"handle"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil || d.Handle.ID == "" {
		return
	}
	hi := d.Handle
	if _, ok := s.handles.get(hi.ID); ok {
		h, _ := s.handles.get(hi.ID)
		h.mu.Lock()
		h.prevNode = hi.PreviousNode
		if hi.LastResult != nil {
			h.last = hi.LastResult
		}
		h.state = restoredState(hi.State)
		h.mu.Unlock()
		return
	}
	h := &handle{id: hi.ID, defName: hi.Def, owner: hi.Owner,
		conv: hi.Conversation, threadKey: "handle:" + hi.ID,
		state: restoredState(hi.State), prevNode: hi.PreviousNode,
		oneShot: hi.OneShot, last: hi.LastResult}
	h.spec = ExecSpec{Name: hi.Def}
	s.handles.put(h)
}

func restoredState(st string) string {
	if st == HandleClosed {
		return HandleClosed
	}
	return HandleIdle // a running activation did not survive the restart
}
