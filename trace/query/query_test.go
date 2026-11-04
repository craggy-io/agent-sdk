package query_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/trace/query"
)

func testItems() []model.Item {
	return []model.Item{
		{Seq: 0, Kind: model.KindSystem, Text: "You are helpful."},
		{Seq: 9, Kind: model.KindSummary, Text: "earlier summary", Covers: []model.SeqRange{{First: 1, Last: 4}}},
		{Seq: 5, Kind: model.KindUser, Text: "do it"},
		{
			Seq: 6, Kind: model.KindAssistant, ServedBy: "openai/gpt-5.2", Finish: model.FinishToolCalls,
			Calls:     []model.ToolCall{{ID: "call_1", Name: "fs.read", Args: json.RawMessage(`{"path":"a.txt"}`), ArgsOK: true}},
			Reasoning: &model.Reasoning{Details: json.RawMessage(`[{"type":"reasoning.text","text":"thinking","index":0}]`)},
		},
		{Seq: 7, Kind: model.KindToolResult, CallID: "call_1", Text: "file body", Settled: model.SettleRan},
	}
}

func testParams() trace.RequestParams {
	return trace.RequestParams{
		Model:     "openai/gpt-5.2",
		Fallbacks: []string{"anthropic/claude-sonnet-5.5"},
		Tools: []model.Tool{{
			Name: "fs.read", Description: "Read a file", Effect: "read-only",
			Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
		Reasoning:         model.ReasoningCfg{Effort: "medium"},
		MaxOutputTokens:   16000,
		ParallelToolCalls: true,
		ToolChoice:        "auto",
		Provider:          model.ProviderPrefs{RequireParameters: true, DataCollection: "deny"},
		SessionID:         "sess-1",
	}
}

// Hand-built expected OpenRouter body (§8), independent of the builder:
// pinned role mapping, tool_calls with string arguments, reasoning_details
// with index stripped, summary as user, plugins disabling gateway
// compression.
const wantBody = `{"model":"openai/gpt-5.2","models":["openai/gpt-5.2","anthropic/claude-sonnet-5.5"],"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"earlier summary"},{"role":"user","content":"do it"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"fs.read","arguments":"{\"path\":\"a.txt\"}"}}],"reasoning_details":[{"text":"thinking","type":"reasoning.text"}]},{"role":"tool","content":"file body","tool_call_id":"call_1"}],"tools":[{"type":"function","function":{"name":"fs.read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"medium"},"max_tokens":16000,"provider":{"require_parameters":true,"data_collection":"deny"},"session_id":"sess-1","plugins":[{"id":"context-compression","enabled":false}],"stream":true}`

func checkRebuild(t *testing.T, s trace.Sink) {
	t.Helper()
	ctx := context.Background()
	rec := trace.NewRecorder(s, nil)
	meta := trace.Event{SessionID: "sess-1", RunID: "r1", NodeID: "n1", StepID: "step-9"}
	if err := rec.RecordStepRequest(ctx, meta, testItems(), testParams(), nil); err != nil {
		t.Fatal(err)
	}
	got, err := query.RebuildRequest(ctx, s, "step-9")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wantBody {
		t.Errorf("rebuild mismatch\n got: %s\nwant: %s", got, wantBody)
	}
	if _, err := query.RebuildRequest(ctx, s, "nope"); err == nil {
		t.Errorf("unknown step: want error")
	}
}

// §7.1 exact replay, byte-equal (the durable sink runs the same check in
// adapters/tracesqlite).
func TestRebuildRequestByteEqual(t *testing.T) {
	checkRebuild(t, memory.New())
}

// §8: tool_choice none sends no tools.
func TestBuildBodyToolChoiceNone(t *testing.T) {
	p := testParams()
	p.ToolChoice = "none"
	b, err := query.BuildBody(p, testItems()[:1])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["tools"]; ok {
		t.Errorf("tools present under tool_choice none: %s", b)
	}
	if string(m["tool_choice"]) != `"none"` {
		t.Errorf("tool_choice=%s", m["tool_choice"])
	}
}

// Reasoning details without an index pass through byte-exact (§8).
func TestStripReasoningIndexPassthrough(t *testing.T) {
	in := json.RawMessage(`[{"text":"x","type":"reasoning.text","signature":"sig"}]`)
	out, err := query.StripReasoningIndex(in)
	if err != nil || string(out) != string(in) {
		t.Errorf("out=%s err=%v", out, err)
	}
}

func TestRunsTreeAndStepEvents(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	rec := trace.NewRecorder(s, nil)
	emit := func(e trace.Event) {
		t.Helper()
		if err := rec.Emit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	emit(trace.Event{Type: "run.start", SessionID: "s", RunID: "r1", Data: json.RawMessage(`{"workflow":"w"}`)})
	emit(trace.Event{Type: "node.start", SessionID: "s", RunID: "r1", NodeID: "n1", Data: json.RawMessage(`{"kind":"agent"}`)})
	emit(trace.Event{Type: "node.start", SessionID: "s", RunID: "r1", NodeID: "n2", Data: json.RawMessage(`{"parent":"n1","kind":"agent"}`)})
	emit(trace.Event{Type: "step.request", SessionID: "s", RunID: "r1", NodeID: "n2", StepID: "st1"})
	emit(trace.Event{Type: "config.fallback", SessionID: "s", RunID: "r1", NodeID: "n2"})
	emit(trace.Event{Type: "node.end", SessionID: "s", RunID: "r1", NodeID: "n1", Data: json.RawMessage(`{"status":"done"}`)})
	emit(trace.Event{Type: "run.end", SessionID: "s", RunID: "r1", Data: json.RawMessage(`{"status":"done"}`)})

	runs, err := query.Runs(ctx, s)
	if err != nil || len(runs) != 1 || runs[0].Status != "done" || runs[0].Events != 7 {
		t.Errorf("Runs=%+v err=%v", runs, err)
	}
	tree, err := query.Tree(ctx, s, "r1")
	if err != nil || len(tree) != 2 || tree[1].Parent != "n1" || tree[0].Status != "done" || tree[1].Status != "running" {
		t.Errorf("Tree=%+v err=%v", tree, err)
	}
	se, err := query.StepEvents(ctx, s, "st1")
	if err != nil || len(se) != 1 || se[0].Type != "step.request" {
		t.Errorf("StepEvents=%+v err=%v", se, err)
	}
	g, err := query.Grep(ctx, s, "r1", "config.fallback", "repair")
	if err != nil || len(g) != 1 {
		t.Errorf("Grep=%+v err=%v", g, err)
	}
}
