package loop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/transcript"
)

// --- test doubles -----------------------------------------------------------

type fakeTools struct {
	mu     sync.Mutex
	defs   []model.Tool
	groups []model.ToolGroup
	fns    map[string]func(ctx context.Context, args json.RawMessage) (model.ToolResult, error)
	calls  []string
	args   []json.RawMessage
	jobs   map[string]func(ctx context.Context) (model.ToolResult, bool)
}

func newFakeTools() *fakeTools {
	return &fakeTools{fns: map[string]func(context.Context, json.RawMessage) (model.ToolResult, error){},
		jobs: map[string]func(context.Context) (model.ToolResult, bool){}}
}

func (f *fakeTools) add(name, effect, schema string, fn func(ctx context.Context, args json.RawMessage) (model.ToolResult, error)) *fakeTools {
	f.defs = append(f.defs, model.Tool{Name: name, Effect: effect, Schema: json.RawMessage(schema)})
	f.fns[name] = fn
	return f
}

func (f *fakeTools) echoTools(names ...string) *fakeTools {
	for _, n := range names {
		name := n
		f.add(name, "read-only", `{"type":"object"}`, func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "ok:" + name}, nil
		})
	}
	return f
}

func (f *fakeTools) Tools() []model.Tool { return f.defs }

func (f *fakeTools) Groups() []model.ToolGroup { return f.groups }

func (f *fakeTools) Run(ctx context.Context, name string, args json.RawMessage) (model.ToolResult, error) {
	f.mu.Lock()
	fn, ok := f.fns[name]
	f.calls = append(f.calls, name)
	f.args = append(f.args, append(json.RawMessage(nil), args...))
	f.mu.Unlock()
	if !ok {
		return model.ToolResult{IsError: true, Text: "no such tool"}, nil
	}
	return fn(ctx, args)
}

func (f *fakeTools) WaitJob(ctx context.Context, id string) (model.ToolResult, bool) {
	f.mu.Lock()
	fn, ok := f.jobs[id]
	f.mu.Unlock()
	if !ok {
		return model.ToolResult{}, false
	}
	return fn(ctx)
}

func (f *fakeTools) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type harness struct {
	thread *transcript.Transcript
	rec    *trace.Recorder
}

func (h *harness) items() []model.Item { return h.thread.Items() }

// check asserts §13.1 invariants on the raw transcript and on its
// normalised projection, after every scenario.
func (h *harness) check(t *testing.T) {
	t.Helper()
	items := h.items()
	agenttest.CheckTranscript(t, items)
	proj, _ := transcript.Normalise(items, "node-1", "scripted")
	if err := transcript.Check(proj); err != nil {
		t.Fatalf("normalised projection violates invariants: %v", err)
	}
}

func call(id, name, args string) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Args: json.RawMessage(args), ArgsOK: true}
}

func resp(text string, finish model.Finish, calls ...model.ToolCall) model.Response {
	return model.Response{
		Item:        model.Item{Kind: model.KindAssistant, Text: text, Finish: finish, Calls: calls, ServedBy: "scripted"},
		ServedModel: "scripted",
	}
}

func runLoop(t *testing.T, prov model.Provider, tools loop.ToolRunner, pol loop.Policy, mutate func(*loop.Input, *loop.Deps)) (*harness, loop.Result) {
	t.Helper()
	h := &harness{thread: transcript.New(), rec: trace.NewRecorder(memory.New(), nil)}
	in := loop.Input{
		NodeID: "node-1", RunID: "run-1", SessionID: "sess-1", DefName: "test",
		System: "you are a test agent", Thread: h.thread, Task: "do the task",
		Policy:      pol,
		Profile:     config.Profile{Model: "scripted", MaxOutputTokens: 100, ParallelToolCalls: true},
		ProfileName: "default",
	}
	deps := loop.Deps{Provider: prov, Tools: tools, Trace: h.rec, Budget: loop.NewBudget()}
	if mutate != nil {
		mutate(&in, &deps)
	}
	res, err := loop.Run(context.Background(), deps, in)
	if err != nil {
		t.Fatalf("loop.Run error: %v", err)
	}
	h.check(t)
	return h, res
}

func resultsOf(h *harness) []model.Item {
	var out []model.Item
	for _, it := range h.items() {
		if it.Kind == model.KindToolResult {
			out = append(out, it)
		}
	}
	return out
}

func noticesOf(h *harness) []string {
	var out []string
	for _, it := range h.items() {
		if it.Kind == model.KindNotice {
			out = append(out, it.Text)
		}
	}
	return out
}

func hasNotice(h *harness, kind string) bool {
	for _, n := range noticesOf(h) {
		if strings.Contains(n, `kind="`+kind+`"`) {
			return true
		}
	}
	return false
}

// --- §5.2 CLASSIFY rows -----------------------------------------------------

// tool_calls happy path: read-only calls run concurrently, mutating calls
// run alone after them, results land in call order (§5.2).
func TestToolCallsParallelReadOnlyExclusiveOrder(t *testing.T) {
	var mu sync.Mutex
	active, maxActive := 0, 0
	mutStage := -1
	roDone := 0
	barrier := make(chan struct{})
	var barrierOnce sync.Once
	ft := newFakeTools()
	ro := func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		if active == 2 {
			barrierOnce.Do(func() { close(barrier) })
		}
		mu.Unlock()
		select {
		case <-barrier:
		case <-time.After(5 * time.Second):
			return model.ToolResult{IsError: true, Text: "read-only calls did not overlap"}, nil
		case <-ctx.Done():
			return model.ToolResult{}, ctx.Err()
		}
		mu.Lock()
		active--
		roDone++
		mu.Unlock()
		return model.ToolResult{Text: "ro"}, nil
	}
	ft.add("ro1", "read-only", `{"type":"object"}`, ro)
	ft.add("ro2", "read-only", `{"type":"object"}`, ro)
	ft.add("mut", "mutating", `{"type":"object"}`, func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
		mu.Lock()
		mutStage = roDone
		mu.Unlock()
		return model.ToolResult{Text: "mut"}, nil
	})
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "ro1", `{}`), call("b", "ro2", `{}`), call("c", "mut", `{}`)),
		agenttest.TextResponse("all done"),
	)
	h, res := runLoop(t, prov, ft, loop.Policy{}, nil)
	if res.Status != "done" || res.Text != "all done" {
		t.Fatalf("status=%q text=%q", res.Status, res.Text)
	}
	rs := resultsOf(h)
	if len(rs) != 3 || rs[0].CallID != "a" || rs[1].CallID != "b" || rs[2].CallID != "c" {
		t.Fatalf("results not in call order: %+v", rs)
	}
	for _, r := range rs {
		if r.Settled != model.SettleRan || r.IsError {
			t.Fatalf("result %+v not a clean run", r)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 2 {
		t.Errorf("read-only max concurrency = %d, want 2", maxActive)
	}
	if mutStage != 2 {
		t.Errorf("mutating call ran with %d read-only calls done, want 2 (exclusive, after)", mutStage)
	}
}

// Trace commit order (rule 5): step.request < step.response < call.end.
func TestTraceWriteBeforeSendOrder(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "t1", `{}`)),
		agenttest.TextResponse("done"),
	)
	h, _ := runLoop(t, prov, newFakeTools().echoTools("t1"), loop.Policy{}, nil)
	evs, _, err := h.rec.Sink.ReadEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	first := map[string]int64{}
	for _, e := range evs {
		if _, ok := first[e.Type]; !ok {
			first[e.Type] = e.Cursor
		}
	}
	if !(first["step.request"] > 0 && first["step.request"] < first["step.response"] && first["step.response"] < first["call.end"]) {
		t.Fatalf("trace order wrong: %v", first)
	}
}

// Invalid arguments are per-call: siblings still run (§5.2).
func TestInvalidArgsPerCallSiblingsRun(t *testing.T) {
	ft := newFakeTools()
	ft.add("need", "read-only", `{"type":"object","required":["q"],"properties":{"q":{"type":"string"}},"additionalProperties":false}`,
		func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "found"}, nil
		})
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(
			call("bad1", "need", `{"q":`),       // malformed JSON
			call("good", "need", `{"q":"x"}`),   // valid
			call("bad2", "need", `{"other":1}`), // schema violation
		),
		agenttest.TextResponse("done"),
	)
	h, res := runLoop(t, prov, ft, loop.Policy{}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	if got := ft.called(); len(got) != 1 {
		t.Fatalf("tool ran %v times (%v), want exactly the valid call", len(got), got)
	}
	rs := resultsOf(h)
	want := []model.SettleReason{model.SettleInvalidArgs, model.SettleRan, model.SettleInvalidArgs}
	if len(rs) != 3 {
		t.Fatalf("results=%d", len(rs))
	}
	for i, w := range want {
		if rs[i].Settled != w {
			t.Errorf("result %d settled=%q want %q (text %q)", i, rs[i].Settled, w, rs[i].Text)
		}
	}
	if rs[1].Text != "found" {
		t.Errorf("sibling result text=%q", rs[1].Text)
	}
}

// Double-encoded arguments are decoded once, then re-checked (§5.2).
func TestDoubleEncodedArgs(t *testing.T) {
	r := agenttest.CallsResponse(call("a", "t1", `{"q":7}`))
	r = agenttest.ApplyFault(r, agenttest.FaultDoubleEncodedArgs, nil)
	ft := newFakeTools().add("t1", "read-only", `{"type":"object","required":["q"],"properties":{"q":{"type":"integer"}}}`,
		func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "args=" + string(args)}, nil
		})
	prov := agenttest.NewScripted(r, agenttest.TextResponse("done"))
	h, res := runLoop(t, prov, ft, loop.Policy{}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	rs := resultsOf(h)
	if len(rs) != 1 || !strings.Contains(rs[0].Text, `{"q":7}`) {
		t.Fatalf("tool did not receive decoded args: %+v", rs)
	}
	if called := ft.called(); len(called) != 1 {
		t.Fatalf("called=%v", called)
	}
	if string(ft.args[0]) != `{"q":7}` {
		t.Errorf("runner args=%s, want decoded object", ft.args[0])
	}
}

func TestUnknownTool(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "nosuch", `{}`)),
		agenttest.TextResponse("done"),
	)
	h, res := runLoop(t, prov, newFakeTools().echoTools("t1"), loop.Policy{}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	rs := resultsOf(h)
	if len(rs) != 1 || rs[0].Settled != model.SettleUnknownTool || !rs[0].IsError {
		t.Fatalf("results=%+v", rs)
	}
}

// length with calls: settle truncated_output, never dispatch, 3 in a row
// fails with output_limit (§5.2, §5.7).
func TestLengthWithCallsSettlesThenOutputLimit(t *testing.T) {
	ft := newFakeTools().echoTools("t1")
	lenResp := func(id string) model.Response {
		return resp("", model.FinishLength, call(id, "t1", `{"big":"`))
	}
	prov := agenttest.NewScripted(lenResp("a"), lenResp("b"), lenResp("c"))
	h, res := runLoop(t, prov, ft, loop.Policy{}, nil)
	if res.Status != "failed: output_limit" {
		t.Fatalf("status=%q", res.Status)
	}
	if len(ft.called()) != 0 {
		t.Fatalf("truncated calls were dispatched: %v", ft.called())
	}
	rs := resultsOf(h)
	if len(rs) != 3 {
		t.Fatalf("results=%d", len(rs))
	}
	for _, r := range rs {
		if r.Settled != model.SettleTruncatedOutput {
			t.Errorf("settled=%q want truncated_output", r.Settled)
		}
	}
}

func TestLengthTextOnlyContinues(t *testing.T) {
	prov := agenttest.NewScripted(
		resp("partial ans", model.FinishLength),
		agenttest.TextResponse("final"),
	)
	h, res := runLoop(t, prov, newFakeTools(), loop.Policy{}, nil)
	if res.Status != "done" || res.Text != "final" {
		t.Fatalf("status=%q text=%q", res.Status, res.Text)
	}
	if !hasNotice(h, "continue") {
		t.Errorf("missing continue notice: %v", noticesOf(h))
	}
}

func TestStopTextDone(t *testing.T) {
	prov := agenttest.NewScripted(agenttest.TextResponse("hello"))
	_, res := runLoop(t, prov, newFakeTools(), loop.Policy{}, nil)
	if res.Status != "done" || res.Text != "hello" || res.Steps != 1 {
		t.Fatalf("res=%+v", res)
	}
}

// Structured mode: one reminder, then no_completion (§5.2, §5.7).
func TestStructuredReminderThenFail(t *testing.T) {
	prov := agenttest.NewScripted(agenttest.TextResponse("text1"), agenttest.TextResponse("text2"))
	h, res := runLoop(t, prov, newFakeTools(), loop.Policy{
		Completion: "structured", Output: json.RawMessage(`{"type":"object"}`),
	}, nil)
	if res.Status != "failed: no_completion" {
		t.Fatalf("status=%q", res.Status)
	}
	if !hasNotice(h, "completion") {
		t.Errorf("missing completion reminder: %v", noticesOf(h))
	}
}

func TestEmptyResponseNoticeThenFail(t *testing.T) {
	prov := agenttest.NewScripted(resp("", model.FinishStop), resp("", model.FinishStop))
	h, res := runLoop(t, prov, newFakeTools(), loop.Policy{}, nil)
	if res.Status != "failed: empty_response" {
		t.Fatalf("status=%q", res.Status)
	}
	if !hasNotice(h, "continue") {
		t.Errorf("missing continuation notice: %v", noticesOf(h))
	}
	// Empty once, then a real answer: done.
	prov2 := agenttest.NewScripted(resp("", model.FinishStop), agenttest.TextResponse("recovered"))
	_, res2 := runLoop(t, prov2, newFakeTools(), loop.Policy{}, nil)
	if res2.Status != "done" || res2.Text != "recovered" {
		t.Fatalf("second scenario status=%q text=%q", res2.Status, res2.Text)
	}
}

func TestContentFilter(t *testing.T) {
	ft := newFakeTools().echoTools("t1")
	prov := agenttest.NewScripted(resp("", model.FinishContentFilter, call("a", "t1", `{}`)))
	h, res := runLoop(t, prov, ft, loop.Policy{}, nil)
	if res.Status != "failed: content_filter" {
		t.Fatalf("status=%q", res.Status)
	}
	if len(ft.called()) != 0 {
		t.Fatalf("content_filter calls were dispatched")
	}
	rs := resultsOf(h)
	if len(rs) != 1 || rs[0].Settled != model.SettleNotDispatched {
		t.Fatalf("results=%+v", rs)
	}
}

// --- complete (§4.3) --------------------------------------------------------

func TestCompleteAloneViolationThenAloneOK(t *testing.T) {
	ft := newFakeTools().echoTools("t1")
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(
			call("c1", "complete", `{"summary":"s","output":{"verdict":"ok"}}`),
			call("x", "t1", `{}`),
		),
		agenttest.CallsResponse(call("c2", "complete", `{"summary":"s2","output":{"verdict":"ok"}}`)),
	)
	h, res := runLoop(t, prov, ft, loop.Policy{
		Completion: "structured",
		Output:     json.RawMessage(`{"type":"object","required":["verdict"],"properties":{"verdict":{"type":"string"}}}`),
	}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	if len(ft.called()) != 0 {
		t.Fatalf("calls ran in complete-alone violation batch: %v", ft.called())
	}
	rs := resultsOf(h)
	if len(rs) != 3 {
		t.Fatalf("results=%d", len(rs))
	}
	for _, r := range rs[:2] {
		if !r.IsError || !strings.Contains(r.Text, "complete must be the only call") {
			t.Errorf("violation result=%+v", r)
		}
	}
	var out struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil || out.Verdict != "ok" {
		t.Fatalf("output=%s err=%v", res.Output, err)
	}
	if res.Text != "s2" {
		t.Errorf("summary text=%q", res.Text)
	}
}

func TestCompleteSchemaFailThenSuccess(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("c1", "complete", `{"summary":"s","output":{"verdict":42}}`)),
		agenttest.CallsResponse(call("c2", "complete", `{"summary":"s","output":{"verdict":"fixed"}}`)),
	)
	h, res := runLoop(t, prov, newFakeTools(), loop.Policy{
		Completion: "structured",
		Output:     json.RawMessage(`{"type":"object","required":["verdict"],"properties":{"verdict":{"type":"string"}}}`),
	}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	rs := resultsOf(h)
	if len(rs) != 2 || !rs[0].IsError || !strings.Contains(rs[0].Text, "schema") {
		t.Fatalf("schema failure not returned to model: %+v", rs)
	}
}

// --- cancellation / panic (§5.2) --------------------------------------------

func TestCancelMidToolSettlesAborted(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	ft := newFakeTools().add("slow", "mutating", `{"type":"object"}`,
		func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return model.ToolResult{}, ctx.Err()
		})
	prov := agenttest.NewScripted(agenttest.CallsResponse(call("a", "slow", `{}`)))
	thread := transcript.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	res, err := loop.Run(ctx, loop.Deps{Provider: prov, Tools: ft, Budget: loop.NewBudget()}, loop.Input{
		NodeID: "node-1", System: "sys", Thread: thread, Task: "go",
		Profile: config.Profile{Model: "scripted", MaxOutputTokens: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{thread: thread}
	h.check(t)
	if res.Status != "cancelled" {
		t.Fatalf("status=%q", res.Status)
	}
	rs := resultsOf(h)
	if len(rs) != 1 || rs[0].Settled != model.SettleAborted {
		t.Fatalf("results=%+v, want one aborted settlement", rs)
	}
	if !hasNotice(h, "turn_aborted") {
		t.Errorf("missing turn_aborted notice: %v", noticesOf(h))
	}
	// The notice comes after the settled result (never between results).
	items := h.items()
	if items[len(items)-1].Kind != model.KindNotice {
		t.Errorf("last item kind=%s, want notice after settlement", items[len(items)-1].Kind)
	}
}

func TestToolPanicBecomesErrorResult(t *testing.T) {
	ft := newFakeTools().add("boom", "mutating", `{"type":"object"}`,
		func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
			panic("kaboom")
		})
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "boom", `{}`)),
		agenttest.TextResponse("survived"),
	)
	h, res := runLoop(t, prov, ft, loop.Policy{}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	rs := resultsOf(h)
	if len(rs) != 1 || !rs[0].IsError || !strings.Contains(rs[0].Text, "panicked: kaboom") {
		t.Fatalf("results=%+v", rs)
	}
}

// --- retry (§5.3) -----------------------------------------------------------

func TestRetryTransientThenSuccessNoDispatchBeforeRetry(t *testing.T) {
	ft := newFakeTools().echoTools("t1")
	prov := agenttest.NewScripted()
	prov.PushError(&model.Error{Status: 503, Code: "unavailable", Message: "down", Transient: true})
	prov.Push(agenttest.CallsResponse(call("a", "t1", `{}`)))
	prov.Push(agenttest.TextResponse("done"))
	_, res := runLoop(t, prov, ft, loop.Policy{}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	if len(ft.called()) != 1 {
		t.Fatalf("tool called %v, want once (retry happens before dispatch)", ft.called())
	}
	if len(prov.Requests) != 3 {
		t.Fatalf("requests=%d, want 3 (fail, retry, final)", len(prov.Requests))
	}
}

func TestRetryBudgetsExhausted(t *testing.T) {
	// Per-step budget: 1 attempt + 3 retries, then fail (§5.3).
	prov := agenttest.NewScripted()
	for i := 0; i < 6; i++ {
		prov.PushError(&model.Error{Status: 429, Code: "rate_limited", Transient: true, RetryAfter: time.Millisecond})
	}
	_, res := runLoop(t, prov, newFakeTools(), loop.Policy{}, nil)
	if res.Status != "failed: rate_limited" {
		t.Fatalf("status=%q", res.Status)
	}
	if len(prov.Requests) != 4 {
		t.Fatalf("requests=%d, want 4 (1+3 retries/step)", len(prov.Requests))
	}
	// Run budget: RetriesLeft consumed.
	prov2 := agenttest.NewScripted()
	for i := 0; i < 6; i++ {
		prov2.PushError(&model.Error{Status: 502, Code: "bad_gateway", Transient: true, RetryAfter: time.Millisecond})
	}
	b := loop.NewBudget()
	b.MaxRetries = 1
	_, res2 := runLoop(t, prov2, newFakeTools(), loop.Policy{}, func(in *loop.Input, deps *loop.Deps) {
		deps.Budget = b
	})
	if res2.Status != "failed: bad_gateway" || len(prov2.Requests) != 2 {
		t.Fatalf("run-budget scenario: status=%q requests=%d, want failed+2", res2.Status, len(prov2.Requests))
	}
	// Never transient: 400 is fatal without retry (§5.3).
	prov3 := agenttest.NewScripted()
	prov3.PushError(&model.Error{Status: 400, Code: "bad_request", Message: "nope"})
	_, res3 := runLoop(t, prov3, newFakeTools(), loop.Policy{}, nil)
	if res3.Status != "failed: bad_request" || len(prov3.Requests) != 1 {
		t.Fatalf("400 scenario: status=%q requests=%d", res3.Status, len(prov3.Requests))
	}
}

func TestMissingFinishValidDispatches(t *testing.T) {
	ft := newFakeTools().echoTools("t1")
	prov := agenttest.NewScripted(
		resp("", model.FinishMissing, call("a", "t1", `{}`)),
		agenttest.TextResponse("done"),
	)
	_, res := runLoop(t, prov, ft, loop.Policy{}, nil)
	if res.Status != "done" || len(ft.called()) != 1 {
		t.Fatalf("status=%q called=%v", res.Status, ft.called())
	}
}

func TestMissingFinishInvalidStops(t *testing.T) {
	ft := newFakeTools().echoTools("t1")
	prov := agenttest.NewScripted(resp("", model.FinishMissing, call("a", "t1", `{"x":`)))
	h, res := runLoop(t, prov, ft, loop.Policy{}, nil)
	if res.Status != "failed: missing_finish" {
		t.Fatalf("status=%q", res.Status)
	}
	if len(ft.called()) != 0 {
		t.Fatalf("invalid missing-finish call dispatched")
	}
	rs := resultsOf(h)
	if len(rs) != 1 || rs[0].Settled != model.SettleNotDispatched {
		t.Fatalf("results=%+v", rs)
	}
}

// --- compaction (§5.6) ------------------------------------------------------

func bulkyThread(n int, size int) *transcript.Transcript {
	th := transcript.New()
	th.Append(model.Item{Kind: model.KindSystem, Text: "sys"})
	for i := 0; i < n; i++ {
		th.Append(model.Item{Kind: model.KindUser, Text: fmt.Sprintf("question %d ", i) + strings.Repeat("u", size)})
		th.Append(model.Item{Kind: model.KindAssistant, Text: fmt.Sprintf("answer %d ", i) + strings.Repeat("a", size),
			Finish: model.FinishStop, ServedBy: "scripted"})
	}
	return th
}

func summariesOf(h *harness) []model.Item {
	var out []model.Item
	for _, it := range h.items() {
		if it.Kind == model.KindSummary {
			out = append(out, it)
		}
	}
	return out
}

// context_length_exceeded triggers one compaction and then one retry
// (§5.3, §5.6). The summariser sees plain text with no tools.
func TestContextExceededCompactsAndRetries(t *testing.T) {
	prov := agenttest.NewScripted()
	prov.PushError(&model.Error{Status: 400, Code: "context_length_exceeded", ContextExceeded: true})
	prov.Push(agenttest.TextResponse("compact summary of everything"))
	prov.Push(agenttest.TextResponse("final answer"))
	th := bulkyThread(6, 300)
	h, res := runLoop(t, prov, newFakeTools(), loop.Policy{KeepRecentTokens: 20}, func(in *loop.Input, deps *loop.Deps) {
		in.Thread = th
		in.Task = ""
	})
	// runLoop seeded its own harness thread; use the real one for checks.
	realH := &harness{thread: th}
	realH.check(t)
	_ = h
	if res.Status != "done" || res.Text != "final answer" {
		t.Fatalf("status=%q text=%q", res.Status, res.Text)
	}
	sums := summariesOf(realH)
	if len(sums) != 1 || len(sums[0].Covers) == 0 {
		t.Fatalf("summaries=%+v", sums)
	}
	// Request 2 is the compaction step: no tools, tool_choice none, and
	// the rendered input is a single user message of plain text (§5.6).
	if len(prov.Requests) != 3 {
		t.Fatalf("requests=%d want 3", len(prov.Requests))
	}
	creq := prov.Requests[1]
	if len(creq.Tools) != 0 || creq.ToolChoice != "none" {
		t.Fatalf("compaction request tools=%v choice=%q", creq.Tools, creq.ToolChoice)
	}
	userCount := 0
	for _, it := range creq.Items {
		if it.Kind == model.KindUser {
			userCount++
			if !strings.Contains(it.Text, "assistant (seq") || strings.Contains(it.Text, "tool_calls") {
				t.Errorf("compaction input is not plain-text rendering: %.120q", it.Text)
			}
		}
		if len(it.Calls) > 0 {
			t.Errorf("compaction input carries tool protocol")
		}
	}
	if userCount != 1 {
		t.Errorf("compaction user messages=%d want 1", userCount)
	}
	// Coverage excludes the system item and the newest user instruction.
	for _, rg := range sums[0].Covers {
		if rg.First == 0 {
			t.Errorf("summary covers the system item: %+v", sums[0].Covers)
		}
	}
}

// Repeated compaction: the second summary supersedes the first and unions
// its coverage (§5.6); invariants hold on raw and projected transcripts.
func TestCompactionMultiRoundSupersede(t *testing.T) {
	big := strings.Repeat("B", 600)
	ft := newFakeTools().add("big", "read-only", `{"type":"object"}`,
		func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: big}, nil
		})
	prov := agenttest.NewScripted(
		agenttest.TextResponse("summary one"),
		agenttest.CallsResponse(call("g1", "big", `{}`)),
		agenttest.CallsResponse(call("g2", "big", `{}`)),
		agenttest.TextResponse("summary two, a little longer"),
		agenttest.TextResponse("final"),
	)
	th := bulkyThread(6, 200)
	h := &harness{thread: th, rec: trace.NewRecorder(memory.New(), nil)}
	res, err := loop.Run(context.Background(),
		loop.Deps{Provider: prov, Tools: ft, Trace: h.rec, Budget: loop.NewBudget()},
		loop.Input{
			NodeID: "node-1", RunID: "r", SessionID: "s", System: "sys", Thread: th,
			Policy: loop.Policy{
				CompactAt: 0.5, KeepRecentTokens: 30, SummaryTokens: 50,
				ToolResultBytes: 1 << 20,
			},
			Profile:       config.Profile{Model: "scripted", MaxOutputTokens: 10},
			ContextWindow: 680, // two rounds, with result_read always advertised
		})
	if err != nil {
		t.Fatal(err)
	}
	h.check(t)
	if res.Status != "done" {
		t.Fatalf("status=%q (requests=%d)", res.Status, len(prov.Requests))
	}
	sums := summariesOf(h)
	if len(sums) != 2 {
		t.Fatalf("summaries=%d, want 2 rounds", len(sums))
	}
	first, second := sums[0], sums[1]
	found := false
	for _, s := range second.Supersedes {
		if s == first.Seq {
			found = true
		}
	}
	if !found {
		t.Fatalf("second summary supersedes=%v, want it to include %d", second.Supersedes, first.Seq)
	}
	// Coverage union: every seq covered by the first stays covered.
	covered := map[int]bool{}
	for _, rg := range second.Covers {
		for s := rg.First; s <= rg.Last; s++ {
			covered[s] = true
		}
	}
	for _, rg := range first.Covers {
		for s := rg.First; s <= rg.Last; s++ {
			if !covered[s] {
				t.Errorf("seq %d covered by first summary lost in second", s)
			}
		}
	}
	// The projection shows only the active (second) summary (§5.4.5).
	proj, _ := transcript.Normalise(h.items(), "node-1", "scripted")
	active := 0
	for _, it := range proj {
		if it.Kind == model.KindSummary {
			active++
			if it.Seq != second.Seq {
				t.Errorf("projection kept superseded summary seq %d", it.Seq)
			}
		}
	}
	if active != 1 {
		t.Errorf("active summaries in projection=%d", active)
	}
}

// An empty summary is a failure, never success (§5.6).
func TestCompactionEmptySummaryFails(t *testing.T) {
	prov := agenttest.NewScripted(agenttest.TextResponse("   "))
	th := bulkyThread(6, 200)
	res, err := loop.Run(context.Background(),
		loop.Deps{Provider: prov, Tools: newFakeTools(), Budget: loop.NewBudget()},
		loop.Input{
			NodeID: "node-1", System: "sys", Thread: th,
			Policy:        loop.Policy{CompactAt: 0.5, KeepRecentTokens: 30, SummaryTokens: 50},
			Profile:       config.Profile{Model: "scripted", MaxOutputTokens: 10},
			ContextWindow: 600,
		})
	if err != nil {
		t.Fatal(err)
	}
	(&harness{thread: th}).check(t)
	if res.Status != "failed: compaction_failed" {
		t.Fatalf("status=%q", res.Status)
	}
}

// --- loop detector, budgets, hooks (§5.7, §5.8) -----------------------------

func TestLoopDetectorNoticeThenStop(t *testing.T) {
	ft := newFakeTools().echoTools("t1")
	prov := agenttest.NewScripted()
	for i := 0; i < 8; i++ {
		prov.Push(agenttest.CallsResponse(call(fmt.Sprintf("c%d", i), "t1", `{"same":true}`)))
	}
	h, res := runLoop(t, prov, ft, loop.Policy{}, nil)
	if res.Status != "stopped: loop" {
		t.Fatalf("status=%q", res.Status)
	}
	if !hasNotice(h, "loop") {
		t.Errorf("loop notice (at 3 repeats) missing: %v", noticesOf(h))
	}
	if len(ft.called()) != 5 {
		t.Errorf("calls ran=%d, want 5 (notice at 3, stop when >4 of last 10)", len(ft.called()))
	}
}

func TestBudgetPreChargeStop(t *testing.T) {
	prov := agenttest.NewScripted(agenttest.TextResponse("never"))
	prov.Info = model.ModelInfo{ContextWindow: 128000, InputPricePerToken: 1.0}
	_, res := runLoop(t, prov, newFakeTools(), loop.Policy{}, func(in *loop.Input, deps *loop.Deps) {
		deps.Budget = loop.NewBudget(config.Limits{USD: 0.001})
	})
	if res.Status != "stopped: budget(usd)" {
		t.Fatalf("status=%q", res.Status)
	}
	if len(prov.Requests) != 0 {
		t.Fatalf("step started despite pre-charge rejection (%d requests)", len(prov.Requests))
	}
	// max_steps is a budget stop too (§5.7).
	prov2 := agenttest.NewScripted()
	for i := 0; i < 5; i++ {
		prov2.Push(agenttest.CallsResponse(call(fmt.Sprintf("c%d", i), "t1", fmt.Sprintf(`{"i":%d}`, i))))
	}
	_, res2 := runLoop(t, prov2, newFakeTools().echoTools("t1"), loop.Policy{MaxSteps: 2}, nil)
	if res2.Status != "stopped: budget(steps)" || res2.Steps != 2 {
		t.Fatalf("max_steps scenario: status=%q steps=%d", res2.Status, res2.Steps)
	}
}

func TestBeforeStopContinuesAtMostTwice(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.TextResponse("try1"), agenttest.TextResponse("try2"), agenttest.TextResponse("try3"),
		agenttest.TextResponse("try4"),
	)
	hookCalls := 0
	h, res := runLoop(t, prov, newFakeTools(), loop.Policy{
		Hooks: loop.Hooks{BeforeStop: func(ctx context.Context, s loop.StopView) ([]model.Item, error) {
			hookCalls++
			return []model.Item{{Text: "keep going"}}, nil
		}},
	}, nil)
	if res.Status != "done" || res.Text != "try3" {
		t.Fatalf("status=%q text=%q, want done at third final (2 continuations max)", res.Status, res.Text)
	}
	if hookCalls != 3 {
		t.Errorf("hook calls=%d want 3", hookCalls)
	}
	// Hook items arrive as notices, never system (rule 7).
	if !hasNotice(h, "hook") {
		t.Errorf("hook continuation not delivered as notice: %v", noticesOf(h))
	}
}

// --- results: spill + result_read, skill (§5.5, §4.3) -----------------------

// spillProvider answers dynamically: step 1 calls the big tool; step 2
// reads the elided middle via result_read; step 3 finishes.
type spillProvider struct {
	requests []model.Request
	full     string
}

func (p *spillProvider) Sample(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	p.requests = append(p.requests, req)
	switch len(p.requests) {
	case 1:
		return agenttest.CallsResponse(call("a", "big", `{}`)), nil
	case 2:
		last := req.Items[len(req.Items)-1]
		m := regexp.MustCompile(`result_read\(handle="(r_[^"]+)"\)`).FindStringSubmatch(last.Text)
		if m == nil {
			return model.Response{}, fmt.Errorf("no spill handle in result: %q", last.Text)
		}
		args := fmt.Sprintf(`{"handle":%q,"offset":450,"limit":120}`, m[1])
		return agenttest.CallsResponse(call("b", "result_read", args)), nil
	default:
		return agenttest.TextResponse("done"), nil
	}
}

func (p *spillProvider) Model(ctx context.Context, id string) (model.ModelInfo, error) {
	return model.ModelInfo{ID: id, ContextWindow: 128000}, nil
}

func TestOversizedResultSpillAndResultRead(t *testing.T) {
	full := strings.Repeat("x", 500) + "MIDDLEMARKER" + strings.Repeat("y", 500)
	ft := newFakeTools().add("big", "read-only", `{"type":"object"}`,
		func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: full}, nil
		})
	prov := &spillProvider{full: full}
	h, res := runLoop(t, prov, ft, loop.Policy{ToolResultBytes: 100}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	rs := resultsOf(h)
	if len(rs) != 2 {
		t.Fatalf("results=%d", len(rs))
	}
	if !strings.Contains(rs[0].Text, "elided") || !strings.Contains(rs[0].Text, `result_read(handle="r_`) {
		t.Fatalf("first result not a spill marker: %q", rs[0].Text)
	}
	if len(rs[0].Text) > 300 {
		t.Errorf("spilled result still %d bytes", len(rs[0].Text))
	}
	if !strings.Contains(rs[1].Text, "MIDDLEMARKER") {
		t.Fatalf("result_read did not page the middle: %q", rs[1].Text)
	}
	// result_read is advertised from step 1, so a spill never changes
	// the tool list mid-run (§4.3).
	for i, req := range prov.requests[:2] {
		advertised := false
		for _, tl := range req.Tools {
			if tl.Name == "result_read" {
				advertised = true
			}
		}
		if !advertised {
			t.Errorf("result_read not advertised on step %d", i+1)
		}
	}
	if len(prov.requests[0].Tools) != len(prov.requests[1].Tools) {
		t.Errorf("tool list changed after the spill: %d -> %d tools", len(prov.requests[0].Tools), len(prov.requests[1].Tools))
	}
	// Full bytes are in the trace blob store (§5.5).
	blob := trace.HashOf([]byte(full))
	got, err := h.rec.Sink.GetBlob(context.Background(), blob)
	if err != nil || string(got) != full {
		t.Errorf("spilled blob missing: err=%v len=%d", err, len(got))
	}
}

// --- long jobs: loop.wait_jobs (§5.5) ---------------------------------------

func jobTool(ft *fakeTools, id string, settle func(ctx context.Context) (model.ToolResult, bool)) *fakeTools {
	ft.jobs[id] = settle
	return ft.add("long", "long", `{"type":"object"}`, func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
		return model.ToolResult{JobID: id, Text: `{"job":"` + id + `","status":{"state":"running"}}`}, nil
	})
}

func callEnds(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	evs, _, err := h.rec.Sink.ReadEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range evs {
		if e.Type == "call.end" {
			var d map[string]any
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
	}
	return out
}

// A job that settles within wait_jobs reaches the model as its result,
// like a synchronous call; call.end still names the job.
func TestLongJobSettledWithinWaitReturnsResult(t *testing.T) {
	ft := jobTool(newFakeTools(), "job-1", func(ctx context.Context) (model.ToolResult, bool) {
		return model.ToolResult{Text: "decompiled"}, true
	})
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "long", `{}`)),
		agenttest.TextResponse("done"),
	)
	h, res := runLoop(t, prov, ft, loop.Policy{WaitJobs: time.Second}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	if rs := resultsOf(h); len(rs) != 1 || rs[0].Text != "decompiled" || rs[0].IsError {
		t.Fatalf("result = %+v, want the job's result", rs)
	}
	if ends := callEnds(t, h); len(ends) != 1 || ends[0]["job_id"] != "job-1" {
		t.Fatalf("call.end = %v, want job_id job-1", ends)
	}
}

// A job still running when wait_jobs ends keeps its handle.
func TestLongJobStillRunningKeepsHandle(t *testing.T) {
	ft := jobTool(newFakeTools(), "job-2", func(ctx context.Context) (model.ToolResult, bool) {
		<-ctx.Done()
		return model.ToolResult{}, false
	})
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "long", `{}`)),
		agenttest.TextResponse("done"),
	)
	h, _ := runLoop(t, prov, ft, loop.Policy{WaitJobs: 20 * time.Millisecond}, nil)
	if rs := resultsOf(h); len(rs) != 1 || !strings.Contains(rs[0].Text, `"job":"job-2"`) ||
		!strings.Contains(rs[0].Text, "\nnote: job job-2 is still running after ") {
		t.Fatalf("result = %+v, want the job handle and a still-running note", rs)
	}
}

// Every call.end follows a call.start for the same call, including
// calls settled without running (invalid arguments, unknown tools).
func TestEveryCallEndHasCallStart(t *testing.T) {
	ft := newFakeTools().echoTools("t1")
	ft.add("need", "read-only", `{"type":"object","required":["q"],"properties":{"q":{"type":"string"}}}`,
		func(ctx context.Context, args json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "found"}, nil
		})
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "t1", `{}`), call("b", "need", `{"q":["x"]}`), call("c", "nope", `{}`)),
		agenttest.TextResponse("done"),
	)
	h, _ := runLoop(t, prov, ft, loop.Policy{}, nil)
	evs, _, err := h.rec.Sink.ReadEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	started := map[string]int{}
	ended := 0
	for _, e := range evs {
		var d struct {
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(e.Data, &d)
		switch e.Type {
		case "call.start":
			started[d.CallID]++
		case "call.end":
			ended++
			if started[d.CallID] != 1 {
				t.Errorf("call.end for %s has %d call.start before it, want 1", d.CallID, started[d.CallID])
			}
		}
	}
	if ended != 3 {
		t.Fatalf("call.end events = %d, want 3", ended)
	}
}

func TestSkillBuiltinReturnsBodyOnce(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "skill", `{"name":"review"}`)),
		agenttest.TextResponse("done"),
	)
	h, res := runLoop(t, prov, newFakeTools(), loop.Policy{}, func(in *loop.Input, deps *loop.Deps) {
		in.Skills = map[string]string{"review": "THE SKILL BODY"}
	})
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	count := 0
	for _, it := range h.items() {
		count += strings.Count(it.Text, "THE SKILL BODY")
	}
	if count != 1 {
		t.Errorf("skill body appears %d times, want exactly 1 (as a tool result)", count)
	}
}

// Delegate built-ins are advertised only when a Delegate is set (§4.3).
func TestDelegateBuiltinsNilSafe(t *testing.T) {
	prov := agenttest.NewScripted(agenttest.TextResponse("hi"))
	runLoop(t, prov, newFakeTools(), loop.Policy{}, nil)
	for _, tl := range prov.Requests[0].Tools {
		if tl.Name == "agent" || tl.Name == "workflow" || strings.HasPrefix(tl.Name, "agent_") {
			t.Errorf("delegate built-in %q advertised without a Delegate", tl.Name)
		}
	}
}

// --- chaos (§13.3): invariants survive every injected fault -----------------

func TestChaosFaultsKeepInvariants(t *testing.T) {
	for _, fault := range agenttest.AllFaults {
		t.Run(string(fault), func(t *testing.T) {
			inner := agenttest.NewScripted()
			for i := 0; i < 4; i++ {
				inner.Push(agenttest.CallsResponse(
					call(fmt.Sprintf("a%d", i), "t1", fmt.Sprintf(`{"i":%d}`, i)),
					call(fmt.Sprintf("b%d", i), "t1", fmt.Sprintf(`{"i":%d}`, i+10)),
				))
				inner.Push(agenttest.TextResponse("done"))
			}
			chaos := agenttest.NewChaos(inner, 1, 0)
			chaos.Force = fault
			// runLoop checks invariants on raw + projection. Any status is
			// acceptable; corruption is not (§13.3 property).
			_, _ = runLoop(t, chaos, newFakeTools().echoTools("t1"), loop.Policy{MaxSteps: 6}, nil)
		})
	}
}
