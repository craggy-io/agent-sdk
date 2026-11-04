package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.starlark.net/starlark"

	"github.com/craggy-io/agent-sdk/internal/jschema"
	"github.com/craggy-io/agent-sdk/trace"
)

// ask() is the workflow user-input gate (§6.2): on first execution it
// records an input.requested event and suspends the run (RunEx
// returns StatusWaiting with Result.Pending). A host answers with
// Answer, which validates the value against the recorded schema and
// records input.provided. Resume replays the script; the ask call
// finds its provided value, commits it as its journaled result and
// returns it. A deadline that passes unanswered is recorded as a
// timed_out journal result on the next pass, so replay stays
// deterministic: ask then returns {"timed_out": true} and never
// raises. Nested workflow_run asks suspend the whole run. ask is
// only reachable on the workflow's own Starlark thread — parallel/
// start branches are agent specs and cannot call it.

// StatusWaiting is the RunEx/Result status of a run suspended at an
// ask() gate.
const StatusWaiting = "waiting"

// PendingInput is one unanswered ask() gate.
type PendingInput struct {
	RunID    string          `json:"run_id"`
	Key      string          `json:"key"`
	Kind     string          `json:"kind"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
	Deadline time.Time       `json:"deadline,omitempty"` // zero: no deadline
}

// errSuspended unwinds the Starlark thread at an ask gate. The exec's
// pending field carries the request; the error text is only a marker.
var errSuspended = errors.New("workflow: suspended at ask() gate")

// askState is the journal-side record of one ask call.
type askState struct {
	PendingInput
	Provided    json.RawMessage
	HasProvided bool
}

func (j *journal) ask(key string) (*askState, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	st, ok := j.asks[key]
	if !ok {
		return nil, false
	}
	cp := *st
	return &cp, true
}

func (j *journal) recordAsk(st *askState) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.asks == nil {
		j.asks = map[string]*askState{}
	}
	j.asks[st.Key] = st
}

func applyAskEvent(asks map[string]*askState, runID, typ string, d map[string]any) {
	key := strOf(d["key"])
	if key == "" {
		return
	}
	st := asks[key]
	if st == nil {
		st = &askState{}
		st.RunID, st.Key = runID, key
		asks[key] = st
	}
	switch typ {
	case "input.requested":
		st.Kind = strOf(d["kind"])
		if v, ok := d["payload"]; ok && v != nil {
			st.Payload, _ = json.Marshal(v)
		}
		if v, ok := d["schema"]; ok && v != nil {
			st.Schema, _ = json.Marshal(v)
		}
		if dl := strOf(d["deadline"]); dl != "" {
			st.Deadline, _ = time.Parse(time.RFC3339Nano, dl)
		}
	case "input.provided":
		if v, ok := d["value"]; ok {
			st.Provided, _ = json.Marshal(v)
			st.HasProvided = true
		}
	}
}

func builtinAsk(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var kind string
	var payloadV, schemaV, timeoutV starlark.Value
	if err := starlark.UnpackArgs("ask", args, kwargs,
		"kind", &kind, "payload?", &payloadV, "schema?", &schemaV, "timeout?", &timeoutV); err != nil {
		return nil, err
	}
	if kind == "" {
		return nil, fmt.Errorf("ask: kind is required")
	}
	payload := any(map[string]any{})
	if payloadV != nil && payloadV != starlark.None {
		v, err := fromStar(payloadV)
		if err != nil {
			return nil, fmt.Errorf("ask: payload: %w", err)
		}
		payload = v
	}
	schemaRaw, err := schemaArgJSON(schemaV)
	if err != nil {
		return nil, fmt.Errorf("ask: schema: %w", err)
	}
	timeout, err := timeoutArg(timeoutV)
	if err != nil {
		return nil, fmt.Errorf("ask: %w", err)
	}
	keyArgs := map[string]any{"kind": kind, "payload": payload,
		"schema": json.RawMessage(schemaRaw), "timeout": timeout.Seconds()}
	key := e.nextKey("ask", keyArgs)

	// A committed result (an earlier provided value, or a recorded
	// timeout) replays without touching the gate again (§6.3).
	if ent, ok := e.journal.lookup(key); ok {
		_ = e.journal.emitCall(e.ctx, e, key, "ask", "replayed", keyArgs, ent)
		if ent.Error != "" {
			return nil, fmt.Errorf("%s", ent.Error)
		}
		var v any
		if len(ent.Result) > 0 {
			if err := json.Unmarshal(ent.Result, &v); err != nil {
				return nil, err
			}
		}
		return toStar(v), nil
	}

	commit := func(v any) (starlark.Value, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if err := e.journal.commit(e.ctx, e, &journalEntry{Key: key, Builtin: "ask", Result: b}, keyArgs); err != nil {
			return nil, fmt.Errorf("ask: journal commit: %w", err)
		}
		return toStar(v), nil
	}

	st, requested := e.journal.ask(key)
	if requested && st.HasProvided {
		var v any
		if err := json.Unmarshal(st.Provided, &v); err != nil {
			return nil, fmt.Errorf("ask: provided value: %w", err)
		}
		return commit(v)
	}
	if requested && !st.Deadline.IsZero() && time.Now().After(st.Deadline) {
		_ = e.emit("input.timed_out", map[string]any{"key": key, "kind": kind})
		return commit(map[string]any{"timed_out": true})
	}
	if !requested {
		deadline := time.Time{}
		if timeout > 0 {
			deadline = time.Now().Add(timeout)
		}
		payloadRaw, _ := json.Marshal(payload)
		st = &askState{PendingInput: PendingInput{
			RunID: e.runID, Key: key, Kind: kind,
			Payload: payloadRaw, Schema: schemaRaw, Deadline: deadline,
		}}
		d := map[string]any{"key": key, "kind": kind, "payload": payload,
			"schema": json.RawMessage(schemaRaw)}
		if !deadline.IsZero() {
			d["deadline"] = deadline.UTC().Format(time.RFC3339Nano)
		}
		if err := e.emit("input.requested", d); err != nil {
			return nil, fmt.Errorf("ask: record request: %w", err)
		}
		e.journal.recordAsk(st)
	}
	cp := st.PendingInput
	e.pending = &cp
	return nil, errSuspended
}

// schemaArgJSON converts an ask schema= argument (schema value, dict,
// JSON string, or None) to a JSON Schema document.
func schemaArgJSON(v starlark.Value) (json.RawMessage, error) {
	return schemaJSON(v)
}

// timeoutArg reads ask timeout= as seconds (int or float); None or
// omitted means no deadline.
func timeoutArg(v starlark.Value) (time.Duration, error) {
	switch t := v.(type) {
	case nil, starlark.NoneType:
		return 0, nil
	case starlark.Int:
		n, ok := t.Int64()
		if !ok {
			return 0, fmt.Errorf("timeout out of range")
		}
		return time.Duration(n) * time.Second, nil
	case starlark.Float:
		return time.Duration(float64(t) * float64(time.Second)), nil
	default:
		return 0, fmt.Errorf("timeout must be seconds (int or float) or None, got %s", v.Type())
	}
}

// Answer records the host's answer to a pending ask gate: it
// validates value against the schema recorded in input.requested
// and appends input.provided. Answers to keys that are not pending
// (unknown, already answered, already resulted, or past their
// deadline) are rejected. Authorization is the host's business.
func Answer(ctx context.Context, rec *trace.Recorder, runID, key string, value json.RawMessage) error {
	if rec == nil || rec.Sink == nil {
		return fmt.Errorf("workflow: answer needs a trace recorder")
	}
	if len(value) == 0 {
		value = json.RawMessage(`null`)
	}
	st, resulted, err := loadAsk(ctx, rec.Sink, runID, key)
	if err != nil {
		return err
	}
	if st == nil || resulted {
		return fmt.Errorf("workflow: answer: key %q is not pending in run %s", key, runID)
	}
	if st.HasProvided {
		return fmt.Errorf("workflow: answer: key %q in run %s was already answered", key, runID)
	}
	if !st.Deadline.IsZero() && time.Now().After(st.Deadline) {
		return fmt.Errorf("workflow: answer: key %q in run %s timed out at %s", key, runID, st.Deadline.Format(time.RFC3339))
	}
	if len(st.Schema) > 0 {
		if err := jschema.Validate(st.Schema, value); err != nil {
			return fmt.Errorf("workflow: answer: %w", err)
		}
	}
	var v any
	if err := json.Unmarshal(value, &v); err != nil {
		return fmt.Errorf("workflow: answer: value: %w", err)
	}
	b, _ := json.Marshal(map[string]any{"key": key, "kind": st.Kind, "value": v})
	return rec.Emit(ctx, trace.Event{Type: "input.provided", RunID: runID, Data: b})
}

// loadAsk reads one ask gate's state from the trace; resulted reports
// a committed wf.call result for the key (provided-and-committed or
// timed out), after which the gate no longer exists.
func loadAsk(ctx context.Context, sink trace.Sink, runID, key string) (*askState, bool, error) {
	evs, err := scanEvents(ctx, sink, func(ev trace.Event) bool {
		if ev.RunID != runID {
			return false
		}
		switch ev.Type {
		case "input.requested", "input.provided", "wf.call":
			return true
		}
		return false
	})
	if err != nil {
		return nil, false, err
	}
	asks := map[string]*askState{}
	resulted := false
	for _, ev := range evs {
		var d map[string]any
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		if ev.Type == "wf.call" {
			if strOf(d["key"]) == key && d["phase"] == "result" {
				resulted = true
			}
			continue
		}
		applyAskEvent(asks, runID, ev.Type, d)
	}
	return asks[key], resulted, nil
}

// ListWaiting lists every ask gate still waiting for an answer,
// across all runs in the sink, so a host can rebuild pending gates
// after a restart. A gate whose deadline has passed is still listed
// until a resume records its timeout; its Deadline says so.
func ListWaiting(ctx context.Context, sink trace.Sink) ([]PendingInput, error) {
	if sink == nil {
		return nil, nil
	}
	evs, err := scanEvents(ctx, sink, func(ev trace.Event) bool {
		switch ev.Type {
		case "input.requested", "input.provided", "wf.call":
			return true
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	type runKey struct{ run, key string }
	asks := map[runKey]*askState{}
	resulted := map[runKey]bool{}
	for _, ev := range evs {
		var d map[string]any
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		k := strOf(d["key"])
		if k == "" {
			continue
		}
		rk := runKey{ev.RunID, k}
		if ev.Type == "wf.call" {
			if d["phase"] == "result" && strOf(d["builtin"]) == "ask" {
				resulted[rk] = true
			}
			continue
		}
		one := map[string]*askState{}
		if st := asks[rk]; st != nil {
			one[k] = st
		}
		applyAskEvent(one, ev.RunID, ev.Type, d)
		asks[rk] = one[k]
	}
	var out []PendingInput
	for rk, st := range asks {
		if st == nil || st.HasProvided || resulted[rk] {
			continue
		}
		out = append(out, st.PendingInput)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RunID != out[j].RunID {
			return out[i].RunID < out[j].RunID
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}
