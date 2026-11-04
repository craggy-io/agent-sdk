package query_test

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/trace/query"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// §7.1: step.request carries tool IDs as plain event metadata (name,
// effect, schema hash) alongside the blobs, so audits never need to
// open the params blob to know what was offered.
func TestStepRequestToolMetadata(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	rec := trace.NewRecorder(s, nil)
	meta := trace.Event{SessionID: "sess-1", RunID: "r1", NodeID: "n1", StepID: "step-1"}
	if err := rec.RecordStepRequest(ctx, meta, testItems(), testParams(), nil); err != nil {
		t.Fatal(err)
	}
	evs, err := query.Events(ctx, s, query.Filter{Type: "step.request"})
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %d, err %v", len(evs), err)
	}
	var d trace.StepRequestData
	if err := json.Unmarshal(evs[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Model != "openai/gpt-5.2" {
		t.Errorf("model metadata = %q", d.Model)
	}
	if len(d.Tools) != 1 || d.Tools[0].Name != "fs.read" || d.Tools[0].Effect != "read-only" {
		t.Fatalf("tools metadata = %+v", d.Tools)
	}
	if want := trace.ToolHash(testParams().Tools[0]); d.Tools[0].SchemaHash != want {
		t.Errorf("schema hash = %q, want %q", d.Tools[0].SchemaHash, want)
	}
	// A changed schema must change the hash (definition drift is visible).
	changed := testParams().Tools[0]
	changed.Schema = json.RawMessage(`{"type":"object"}`)
	if trace.ToolHash(changed) == d.Tools[0].SchemaHash {
		t.Error("ToolHash did not change with schema")
	}
}

// Usage monitoring: totals and breakdowns aggregate provider-reported
// usage by served model (fallbacks attributed correctly), run, node
// and tool, with scoping filters.
func TestUsageRollups(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	rec := trace.NewRecorder(s, nil)
	resp := func(run, node, step, served string, u model.Usage) trace.Event {
		data, _ := json.Marshal(map[string]any{"served_model": served, "usage": u, "finish": "stop"})
		return trace.Event{Type: "step.response", SessionID: "s1", RunID: run, NodeID: node, StepID: step, Data: data}
	}
	call := func(run, node, step, name string, isErr bool) trace.Event {
		data, _ := json.Marshal(map[string]any{"name": name, "is_error": isErr, "settled": "ran"})
		return trace.Event{Type: "call.end", SessionID: "s1", RunID: run, NodeID: node, StepID: step, Data: data}
	}
	evs := []trace.Event{
		resp("r1", "n1", "st1", "meta/muse-spark-1.3-contributor",
			model.Usage{PromptTokens: 1000, CachedTokens: 100, CompletionTokens: 200, ReasoningTokens: 50, CostUSD: 0.01}),
		// Requested model differs from served (fallback): served wins.
		resp("r1", "n2", "st2", "openai/gpt-5.2",
			model.Usage{PromptTokens: 500, CompletionTokens: 100, CostUSD: 0.02}),
		resp("r2", "n1", "st3", "meta/muse-spark-1.3-contributor",
			model.Usage{PromptTokens: 300, CompletionTokens: 60, CostUSD: 0.005}),
		call("r1", "n1", "st1", "fs.read", false),
		call("r1", "n1", "st1", "fs.read", true),
		call("r2", "n1", "st3", "fs.write", false),
	}
	if err := rec.Emit(ctx, evs...); err != nil {
		t.Fatal(err)
	}

	rep, err := query.Usage(ctx, s, query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Steps != 3 || rep.Total.Calls != 3 || rep.Total.PromptTokens != 1800 ||
		rep.Total.CachedTokens != 100 || rep.Total.ReasoningTokens != 50 || !near(rep.Total.CostUSD, 0.035) {
		t.Fatalf("total = %+v", rep.Total)
	}
	if len(rep.ByModel) != 2 || rep.ByModel[0].Key != "openai/gpt-5.2" || !near(rep.ByModel[0].CostUSD, 0.02) {
		t.Fatalf("by model (cost order) = %+v", rep.ByModel)
	}
	muse := rep.ByModel[1]
	if muse.Steps != 2 || muse.PromptTokens != 1300 || !near(muse.CostUSD, 0.015) {
		t.Fatalf("served-model row = %+v", muse)
	}
	if len(rep.ByTool) != 2 || rep.ByTool[0].Key != "fs.read" || rep.ByTool[0].Calls != 2 || rep.ByTool[0].Errors != 1 {
		t.Fatalf("by tool = %+v", rep.ByTool)
	}

	// Scoped to one run: only its steps/calls count.
	r1, err := query.Usage(ctx, s, query.Filter{RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Total.Steps != 2 || r1.Total.Calls != 2 || !near(r1.Total.CostUSD, 0.03) || len(r1.ByRun) != 1 {
		t.Fatalf("run-scoped = %+v byrun=%+v", r1.Total, r1.ByRun)
	}
	n1, err := query.Usage(ctx, s, query.Filter{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if n1.Total.Steps != 2 || n1.Total.Calls != 3 {
		t.Fatalf("node-scoped = %+v", n1.Total)
	}
}

// Failed attempts are counted, their reported usage joins the totals,
// and Reported is the union over what was summed (nil stays nil for
// legacy events that never said).
func TestUsageFailedAttemptsAndPresence(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	rec := trace.NewRecorder(s, nil)
	ev := func(typ, node string, data map[string]any) trace.Event {
		b, _ := json.Marshal(data)
		return trace.Event{Type: typ, SessionID: "s1", RunID: "r1", NodeID: node, Data: b}
	}
	evs := []trace.Event{
		ev("step.response", "n1", map[string]any{"served_model": "m", "usage": model.Usage{
			CompletionTokens: 4, Reported: []string{model.UsagePromptTokens, model.UsageCompletionTokens}}}),
		ev("step.attempt_failed", "n1", map[string]any{"code": "transient", "usage": model.Usage{
			PromptTokens: 9, CostUSD: 0.002, Reported: []string{model.UsagePromptTokens, model.UsageCostUSD}}}),
		ev("step.attempt_failed", "n2", map[string]any{"code": "rate_limited"}),
		ev("step.response", "n2", map[string]any{"served_model": "m", "usage": model.Usage{PromptTokens: 1}}),
	}
	if err := rec.Emit(ctx, evs...); err != nil {
		t.Fatal(err)
	}
	rep, err := query.Usage(ctx, s, query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	tot := rep.Total
	if tot.Steps != 2 || tot.FailedAttempts != 2 || tot.PromptTokens != 10 || tot.CompletionTokens != 4 || !near(tot.CostUSD, 0.002) {
		t.Fatalf("total = %+v", tot)
	}
	if got := tot.Reported; len(got) != 3 || got[0] != "prompt_tokens" || got[1] != "completion_tokens" || got[2] != "cost_usd" {
		t.Errorf("total reported = %v", got)
	}
	for _, r := range rep.ByNode {
		if r.Key == "n2" && (r.FailedAttempts != 1 || r.Reported != nil) {
			t.Errorf("legacy node row = %+v", r)
		}
	}
}
