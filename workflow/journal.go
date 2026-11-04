package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

// journalEntry is one recorded wf.call result (or failure).
type journalEntry struct {
	Key     string          `json:"key"`
	Builtin string          `json:"builtin"`
	Result  json.RawMessage `json:"result,omitempty"` // decoded from blob on load
	Error   string          `json:"error,omitempty"`
}

// journal writes wf.call intent/result pairs and replays them (§6.3).
// A nil recorder degrades to an in-memory journal for this execution
// only (no resume), which keeps trace-less tests working.
type journal struct {
	rec   *trace.Recorder
	sink  trace.Sink
	runID string

	// mu guards results/intents/ops: parallel() branches commit
	// concurrently (§6.2), and Go maps are not concurrent-safe.
	mu      sync.RWMutex
	results map[string]*journalEntry // recorded results by call key
	intents map[string]map[string]any
	ops     map[string]*opState  // operation key -> intent/outcome/resolve
	asks    map[string]*askState // ask call key -> requested/provided
}

type opState struct {
	Key      string `json:"key"`
	Tool     string `json:"tool"`
	Effect   string `json:"effect"`
	ArgsHash string `json:"args_hash"`
	Outcome  string `json:"outcome,omitempty"` // ok|error|outcome_unknown|resolved*
	JobID    string `json:"job_id,omitempty"`
	// resolution (recovery.resolve)
	Decision   string `json:"decision,omitempty"` // success|failure|retry
	Evidence   string `json:"evidence,omitempty"`
	ResultText string `json:"result_text,omitempty"`
	Caller     string `json:"caller,omitempty"`
}

func newJournal(rec *trace.Recorder, sink trace.Sink, runID string) *journal {
	return &journal{rec: rec, sink: sink, runID: runID,
		results: map[string]*journalEntry{}, intents: map[string]map[string]any{},
		ops: map[string]*opState{}, asks: map[string]*askState{}}
}

func scanEvents(ctx context.Context, sink trace.Sink, keep func(trace.Event) bool) ([]trace.Event, error) {
	var out []trace.Event
	var after int64
	for {
		page, next, err := sink.ReadEvents(ctx, after, 512)
		if err != nil {
			return nil, err
		}
		for _, ev := range page {
			if keep(ev) {
				out = append(out, ev)
			}
		}
		if len(page) < 512 {
			return out, nil
		}
		after = next
	}
}

// load reads the recorded journal for this run from the trace.
func (j *journal) load(ctx context.Context) error {
	if j.sink == nil {
		return nil
	}
	evs, err := scanEvents(ctx, j.sink, func(ev trace.Event) bool {
		if ev.RunID != j.runID {
			return false
		}
		switch ev.Type {
		case "wf.call", "operation.intent", "operation.outcome", "recovery.resolve",
			"input.requested", "input.provided":
			return true
		}
		return false
	})
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, ev := range evs {
		var d map[string]any
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		switch ev.Type {
		case "wf.call":
			key, _ := d["key"].(string)
			if key == "" {
				continue
			}
			switch d["phase"] {
			case "intent":
				j.intents[key] = d
			case "result":
				ent := &journalEntry{Key: key, Builtin: strOf(d["builtin"]), Error: strOf(d["error"])}
				if h := strOf(d["result_blob"]); h != "" {
					if b, err := j.sink.GetBlob(ctx, trace.Hash(h)); err == nil {
						ent.Result = b
					}
				}
				j.results[key] = ent
			}
		case "operation.intent", "operation.outcome", "recovery.resolve":
			applyOpEvent(j.ops, ev.Type, d)
		case "input.requested", "input.provided":
			applyAskEvent(j.asks, j.runID, ev.Type, d)
		}
	}
	return nil
}

func applyOpEvent(ops map[string]*opState, typ string, d map[string]any) {
	key := strOf(d["op_key"])
	if key == "" {
		key = strOf(d["key"])
	}
	if key == "" {
		return
	}
	op := ops[key]
	if op == nil {
		op = &opState{Key: key}
		ops[key] = op
	}
	switch typ {
	case "operation.intent":
		op.Tool, op.Effect, op.ArgsHash = strOf(d["tool"]), strOf(d["effect"]), strOf(d["args_hash"])
		if jid := strOf(d["job_id"]); jid != "" {
			op.JobID = jid
		}
	case "operation.outcome":
		op.Outcome = strOf(d["outcome"])
		if jid := strOf(d["job_id"]); jid != "" {
			op.JobID = jid
		}
	case "recovery.resolve":
		op.Decision, op.Evidence = strOf(d["decision"]), strOf(d["evidence"])
		op.ResultText, op.Caller = strOf(d["result_text"]), strOf(d["caller"])
		if op.Outcome == "" || op.Outcome == "outcome_unknown" {
			op.Outcome = "resolved:" + op.Decision
		}
	}
}

func strOf(v any) string { s, _ := v.(string); return s }

// lookup returns a recorded result for key, if any.
func (j *journal) lookup(key string) (*journalEntry, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	e, ok := j.results[key]
	return e, ok
}

// opSnapshot returns a copy of the recorded op state for key, if any.
func (j *journal) opSnapshot(key string) (*opState, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	op, ok := j.ops[key]
	if !ok || op == nil {
		return nil, false
	}
	cp := *op
	return &cp, true
}

func (j *journal) applyOp(typ string, d map[string]any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	applyOpEvent(j.ops, typ, d)
}

// do runs fn under the journal: replay a recorded result when present,
// otherwise commit intent, run, commit result, then return (§6.3).
func (j *journal) do(ctx context.Context, e *exec, key, builtin string, args any, fn func() (any, error)) (any, bool, error) {
	if ent, ok := j.lookup(key); ok {
		_ = j.emitCall(ctx, e, key, builtin, "replayed", args, ent)
		if ent.Error != "" {
			return nil, true, fmt.Errorf("%s", ent.Error)
		}
		var v any
		if len(ent.Result) > 0 {
			if err := json.Unmarshal(ent.Result, &v); err != nil {
				return nil, true, err
			}
		}
		return v, true, nil
	}
	_ = j.emitCall(ctx, e, key, builtin, "intent", args, nil)
	v, err := fn()
	ent := &journalEntry{Key: key, Builtin: builtin}
	if err != nil {
		ent.Error = err.Error()
	} else if v != nil {
		b, merr := json.Marshal(v)
		if merr != nil {
			return nil, false, merr
		}
		ent.Result = b
	}
	if jerr := j.commit(ctx, e, ent, args); jerr != nil {
		return nil, false, fmt.Errorf("workflow: journal commit: %w", jerr)
	}
	return v, false, err
}

func (j *journal) commit(ctx context.Context, e *exec, ent *journalEntry, args any) error {
	j.mu.Lock()
	j.results[ent.Key] = ent
	j.mu.Unlock()
	return j.emitCall(ctx, e, ent.Key, ent.Builtin, "result", args, ent)
}

func (j *journal) emitCall(ctx context.Context, e *exec, key, builtin, phase string, args any, ent *journalEntry) error {
	if j.rec == nil {
		return nil
	}
	d := map[string]any{"key": key, "builtin": builtin, "phase": phase,
		"args_hash": hash16(args), "replayed": phase == "replayed"}
	if e != nil && e.phase != "" {
		d["phase_name"] = e.phase
	}
	if ab, err := json.Marshal(args); err == nil && phase == "intent" {
		if h, err := j.rec.PutBlob(ctx, ab); err == nil {
			d["args_blob"] = string(h)
		}
	}
	if ent != nil {
		if ent.Error != "" {
			d["error"] = ent.Error
		}
		if len(ent.Result) > 0 {
			if h, err := j.rec.PutBlob(ctx, ent.Result); err == nil {
				d["result_blob"] = string(h)
				d["result_hash"] = hashBytes(ent.Result)
			}
		}
	}
	b, _ := json.Marshal(d)
	node := ""
	if e != nil {
		node = e.nodeID
	}
	return j.rec.Emit(ctx, trace.Event{Type: "wf.call", RunID: j.runID, NodeID: node, Data: b})
}

// opKey derives the stable operation key for a tool call (§6.3).
func opKey(callKey string) string { return "op:" + callKey }

func (j *journal) emitOp(ctx context.Context, e *exec, typ string, op *opState, extra map[string]any) error {
	d := map[string]any{"op_key": op.Key, "tool": op.Tool, "effect": op.Effect, "args_hash": op.ArgsHash}
	for k, v := range extra {
		d[k] = v
	}
	if j.rec == nil {
		j.applyOp(typ, d)
		return nil
	}
	b, _ := json.Marshal(d)
	node := ""
	if e != nil {
		node = e.nodeID
	}
	if err := j.rec.Emit(ctx, trace.Event{Type: typ, RunID: j.runID, NodeID: node, Data: b}); err != nil {
		return err
	}
	j.applyOp(typ, d)
	return nil
}

// RecoveryOp is one unresolved operation for recovery.list (§6.3/§7.3).
type RecoveryOp struct {
	Key      string `json:"op_key"`
	Tool     string `json:"tool,omitempty"`
	Effect   string `json:"effect,omitempty"`
	ArgsHash string `json:"args_hash,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	JobID    string `json:"job_id,omitempty"`
	Decision string `json:"decision,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// ListRecovery lists operations needing reconciliation (§6.3): an
// intent whose outcome is unknown/missing, or whose wf.call result was
// never committed (the crash window), with no resolution yet.
// Session-wide when runID is empty.
func ListRecovery(ctx context.Context, sink trace.Sink, runID string) ([]RecoveryOp, error) {
	ops, err := loadOps(ctx, sink, runID)
	if err != nil {
		return nil, err
	}
	recorded := map[string]bool{}
	if sink != nil {
		evs, err := scanEvents(ctx, sink, func(ev trace.Event) bool {
			return ev.Type == "wf.call" && (runID == "" || ev.RunID == runID)
		})
		if err != nil {
			return nil, err
		}
		for _, ev := range evs {
			var d map[string]any
			if json.Unmarshal(ev.Data, &d) == nil && d["phase"] == "result" {
				recorded[strOf(d["key"])] = true
			}
		}
	}
	var out []RecoveryOp
	for _, op := range ops {
		if op.Decision != "" {
			continue
		}
		callKey := strings.TrimPrefix(op.Key, "op:")
		if op.Outcome == "" || op.Outcome == "outcome_unknown" || !recorded[callKey] {
			out = append(out, RecoveryOp{Key: op.Key, Tool: op.Tool, Effect: op.Effect,
				ArgsHash: op.ArgsHash, Outcome: unknownOutcome(op.Outcome), JobID: op.JobID})
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Key < out[k].Key })
	return out, nil
}

func unknownOutcome(o string) string {
	if o == "" {
		return "outcome_unknown"
	}
	return o
}

func loadOps(ctx context.Context, sink trace.Sink, runID string) (map[string]*opState, error) {
	ops := map[string]*opState{}
	if sink == nil {
		return ops, nil
	}
	evs, err := scanEvents(ctx, sink, func(ev trace.Event) bool {
		if runID != "" && ev.RunID != runID {
			return false
		}
		switch ev.Type {
		case "operation.intent", "operation.outcome", "recovery.resolve":
			return true
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	for _, ev := range evs {
		var d map[string]any
		if json.Unmarshal(ev.Data, &d) == nil {
			applyOpEvent(ops, ev.Type, d)
		}
	}
	return ops, nil
}

// ResolveRecovery appends a recovery.resolve decision (§6.3): it never
// rewrites the original settlement. decision is "success" or "failure"
// (with evidence and, for success, the authoritative result text) or
// "retry" (explicit authorization for a new attempt that may duplicate
// the effect). caller is the authorizing caller's ID.
func ResolveRecovery(ctx context.Context, rec *trace.Recorder, runID, op, decision, evidence, resultText, caller string) error {
	if rec == nil {
		return fmt.Errorf("workflow: recovery resolve needs a trace recorder")
	}
	switch decision {
	case "success", "failure", "retry":
	default:
		return fmt.Errorf("workflow: recovery decision %q, want success|failure|retry", decision)
	}
	if !strings.HasPrefix(op, "op:") {
		op = "op:" + op
	}
	d := map[string]any{"op_key": op, "decision": decision, "evidence": evidence,
		"result_text": resultText, "caller": caller}
	b, _ := json.Marshal(d)
	return rec.Emit(ctx, trace.Event{Type: "recovery.resolve", RunID: runID, Data: b})
}

// effectOf finds a tool's effect in a view (fail closed as mutating).
func effectOf(tools []model.Tool, name string) string {
	for _, t := range tools {
		if t.Name == name {
			if t.Effect == "" {
				return "mutating"
			}
			return t.Effect
		}
	}
	return "mutating"
}
