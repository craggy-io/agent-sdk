package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/defs"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/workflow"
)

// taskProvider answers by inspecting the task text, so parallel order
// cannot scramble scripted responses.
type taskProvider struct {
	mu      sync.Mutex
	calls   []string // task texts seen, in arrival order
	tools   [][]string
	failOn  string // substring -> fatal provider error
	systems []string
}

func (p *taskProvider) Sample(_ context.Context, req model.Request, _ func(model.Delta)) (model.Response, error) {
	task, system := "", ""
	for _, it := range req.Items {
		if it.Kind == model.KindSystem {
			system = it.Text
		}
		if it.Kind == model.KindUser {
			task = it.Text
		}
	}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	p.mu.Lock()
	p.calls = append(p.calls, task)
	p.tools = append(p.tools, names)
	p.systems = append(p.systems, system)
	fail := p.failOn != "" && strings.Contains(task, p.failOn)
	p.mu.Unlock()
	if fail {
		return model.Response{}, &model.Error{Status: 400, Code: "boom", Message: "fatal test error"}
	}
	var text string
	switch {
	case strings.Contains(task, "Plan this task"):
		text = "the plan"
	case strings.Contains(task, "Check correctness"):
		text = "rev-correct"
	case strings.Contains(task, "Find missing cases"):
		text = "rev-missing"
	case strings.Contains(task, "Reviews:"):
		text = "synthesis[" + task + "]"
	default:
		text = "answer to: " + task
	}
	return model.Response{
		Item:        model.Item{Kind: model.KindAssistant, Text: text, Finish: model.FinishStop, ServedBy: "test"},
		ServedModel: "test",
	}, nil
}

func (p *taskProvider) Model(_ context.Context, id string) (model.ModelInfo, error) {
	return model.ModelInfo{ID: id, ContextWindow: 128000}, nil
}

func (p *taskProvider) count(contains string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if strings.Contains(c, contains) {
			n++
		}
	}
	return n
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func testView(ran *atomic.Int32) *host.MapView {
	tools := []model.Tool{
		{Name: "echo", Description: "echo", Effect: "read-only", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "secret", Description: "secret", Effect: "mutating", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "fs.read", Description: "read", Effect: "read-only", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "fs.write", Description: "write", Effect: "mutating", Schema: json.RawMessage(`{"type":"object"}`)},
	}
	fn := func(name string) host.ToolFunc {
		return func(_ context.Context, args json.RawMessage) (model.ToolResult, error) {
			ran.Add(1)
			return model.ToolResult{Text: name + ":" + string(args)}, nil
		}
	}
	return host.NewMapView(tools, map[string]host.ToolFunc{
		"echo": fn("echo"), "secret": fn("secret"), "fs.read": fn("fs.read"), "fs.write": fn("fs.write"),
	})
}

func openTest(t *testing.T, prov model.Provider, sink trace.Sink, view host.ToolView) *host.Host {
	t.Helper()
	s, err := host.Open(context.Background(), host.Options{
		Config: config.Defaults(), View: view, Provider: prov,
		Defs: agentdef.DirRoots(testRoot(t)), Trace: sink,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const layeredSrc = `
WORKFLOW = workflow(
    description = "Plan, review in parallel, synthesise.",
    input = {"message": schema.string(required = True)},
    agents = ["assistant", "reviewer"],
)
def run(input):
    plan = agent("assistant", task = "Plan this task:\n" + input.message, phase = "plan")
    reviews = parallel([
        spec("reviewer", task = "Check correctness:\n" + plan.text),
        spec("reviewer", task = "Find missing cases:\n" + plan.text),
    ], phase = "review")
    joined = "\n\n".join([r.text for r in reviews])
    statuses = ",".join([r.status for r in reviews])
    final = agent("assistant", phase = "synthesise", task = render(
        "Task:\n{t}\n\nPlan:\n{p}\n\nReviews:\n{r}", t = input.message, p = plan.text, r = joined))
    return {"text": final.text, "reviews": joined, "statuses": statuses, "plan": plan.text}
`

// §6.1/§6.2: layered end-to-end on a scripted provider; parallel
// results return in input order.
func TestLayeredEndToEnd(t *testing.T) {
	var ran atomic.Int32
	prov := &taskProvider{}
	s := openTest(t, prov, tracemem.New(), testView(&ran))

	res, err := workflow.RunEx(context.Background(), s, layeredSrc,
		map[string]any{"message": "build a thing"}, workflow.Options{})
	if err != nil {
		t.Fatalf("RunEx: %v", err)
	}
	if res.Status != "done" || res.RunID == "" {
		t.Fatalf("res = %+v", res)
	}
	m, ok := res.Value.(map[string]any)
	if !ok {
		t.Fatalf("value %T", res.Value)
	}
	if m["plan"] != "the plan" {
		t.Fatalf("plan = %v", m["plan"])
	}
	// Input order preserved despite concurrent branches.
	if m["reviews"] != "rev-correct\n\nrev-missing" {
		t.Fatalf("reviews out of order: %q", m["reviews"])
	}
	if !strings.HasPrefix(fmt.Sprint(m["text"]), "synthesis[") ||
		!strings.Contains(fmt.Sprint(m["text"]), "rev-correct") {
		t.Fatalf("synthesis = %v", m["text"])
	}
	if prov.count("Plan this task") != 1 || prov.count("Check correctness") != 1 ||
		prov.count("Find missing cases") != 1 || prov.count("Reviews:") != 1 {
		t.Fatalf("provider calls = %v", prov.calls)
	}
	// host.RunWorkflow reaches the same engine via the init hook.
	wf, err := s.RunWorkflow(context.Background(), layeredSrc, map[string]any{"message": "again"})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}
	if wf.Status != "done" {
		t.Fatalf("RunWorkflow status %q", wf.Status)
	}
	// §2: one session run owns the workflow — run.start/run.end bracket it
	// and every agent node joins that run (regression: driving the engine
	// directly used to fragment each agent() into its own run and leave
	// the workflow run forever "running" in trace queries).
	if wf.RunID == "" {
		t.Fatal("RunWorkflow returned empty RunID")
	}
	evs, _, err := s.Sink().ReadEvents(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var starts, ends, nodes int
	for _, e := range evs {
		if e.RunID != wf.RunID {
			continue
		}
		switch e.Type {
		case "run.start":
			starts++
		case "run.end":
			ends++
		case "node.start":
			nodes++
		}
	}
	if starts != 1 || ends != 1 || nodes < 4 {
		t.Fatalf("workflow run %s: run.start=%d run.end=%d node.start=%d, want 1/1/>=4",
			wf.RunID, starts, ends, nodes)
	}
}

// §6.2: a failed parallel branch returns a failed Result, not a raise.
func TestLayeredFailedBranch(t *testing.T) {
	var ran atomic.Int32
	prov := &taskProvider{failOn: "Find missing cases"}
	s := openTest(t, prov, tracemem.New(), testView(&ran))
	res, err := workflow.RunEx(context.Background(), s, layeredSrc,
		map[string]any{"message": "x"}, workflow.Options{})
	if err != nil {
		t.Fatalf("failed branch must not raise: %v", err)
	}
	m := res.Value.(map[string]any)
	if !strings.HasPrefix(fmt.Sprint(m["statuses"]), "done,failed") {
		t.Fatalf("statuses = %v", m["statuses"])
	}
}

// crashSink fails one chosen wf.call result commit, simulating a crash
// in the window between dispatch and result commit (§6.3/§13.5).
type crashSink struct {
	trace.Sink
	failNth  int
	seen     atomic.Int32
	disabled atomic.Bool
}

func (c *crashSink) Append(ctx context.Context, events ...trace.Event) error {
	for _, ev := range events {
		if ev.Type == "wf.call" && strings.Contains(string(ev.Data), `"phase":"result"`) &&
			strings.Contains(string(ev.Data), `"builtin":"agent"`) {
			if c.seen.Add(1) == int32(c.failNth) && !c.disabled.Load() {
				return fmt.Errorf("simulated crash before result commit")
			}
		}
	}
	return c.Sink.Append(ctx, events...)
}

const twoAgentSrc = `
WORKFLOW = workflow(description = "two agents", input = {"message": schema.string(required = True)})
def run(input):
    a = agent("assistant", task = "FIRST " + input.message)
    b = agent("assistant", task = "SECOND " + input.message)
    return {"a": a.text, "b": b.text}
`

// §6.3: resume reuses journaled results; the provider is not re-asked
// for a completed call, and the interrupted call runs.
func TestResumeReusesJournal(t *testing.T) {
	var ran atomic.Int32
	prov := &taskProvider{}
	sink := &crashSink{Sink: tracemem.New(), failNth: 2}
	s := openTest(t, prov, sink, testView(&ran))

	runID := "wf-resume-test"
	_, err := workflow.RunEx(context.Background(), s, twoAgentSrc,
		map[string]any{"message": "m"}, workflow.Options{RunID: runID})
	if err == nil {
		t.Fatal("expected the simulated crash to fail the first run")
	}
	if prov.count("FIRST") != 1 || prov.count("SECOND") != 1 {
		t.Fatalf("first-run provider calls = %v", prov.calls)
	}
	sink.disabled.Store(true)
	res, err := workflow.Resume(context.Background(), s, runID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("resume status %q value %v", res.Status, res.Value)
	}
	if prov.count("FIRST") != 1 {
		t.Fatalf("FIRST re-asked on resume: calls=%v", prov.calls)
	}
	if prov.count("SECOND") != 2 {
		t.Fatalf("SECOND should run exactly once more: calls=%v", prov.calls)
	}
	m := res.Value.(map[string]any)
	if !strings.Contains(fmt.Sprint(m["a"]), "FIRST") || !strings.Contains(fmt.Sprint(m["b"]), "SECOND") {
		t.Fatalf("value = %v", m)
	}
	// A second resume replays everything, asking nothing.
	before := len(prov.calls)
	res2, err := workflow.Resume(context.Background(), s, runID)
	if err != nil || res2.Status != "done" {
		t.Fatalf("second resume: %v %+v", err, res2)
	}
	if len(prov.calls) != before {
		t.Fatalf("second resume re-asked the provider: %v", prov.calls[before:])
	}
	// Changed config must stop with resume_incompatible (§6.3 pins).
	cfg := config.Defaults()
	cfg.Limits.USD = 99
	s.SetConfig(cfg)
	if _, err := workflow.Resume(context.Background(), s, runID); err == nil ||
		!strings.Contains(err.Error(), "config") {
		t.Fatalf("config pin mismatch: err=%v", err)
	}
}

const defineSrc = `
WORKFLOW = workflow(description = "dyn", input = {"message": schema.string(required = True)})
def run(input):
    d = define("brief", "assistant", prompt = "Always answer very briefly.")
    r = agent(d, task = input.message)
    return {"text": r.text, "status": r.status, "def": d.name}
`

// §4.4/§6.2: define() + agent() with a dynamic definition.
func TestDefineAndAgent(t *testing.T) {
	var ran atomic.Int32
	prov := &taskProvider{}
	s := openTest(t, prov, tracemem.New(), testView(&ran))
	res, err := workflow.RunEx(context.Background(), s, defineSrc,
		map[string]any{"message": "hello"}, workflow.Options{})
	if err != nil {
		t.Fatalf("RunEx: %v", err)
	}
	m := res.Value.(map[string]any)
	if m["status"] != "done" || m["def"] != "brief" {
		t.Fatalf("value = %v", m)
	}
	prov.mu.Lock()
	sys := strings.Join(prov.systems, "\n")
	prov.mu.Unlock()
	if !strings.Contains(sys, "Always answer very briefly.") {
		t.Fatalf("dynamic prompt not in system prompt:\n%s", sys)
	}
	// Dynamic definitions must extends an existing def (§4.4).
	bad := strings.Replace(defineSrc, `"assistant"`, `"nope"`, 1)
	if _, err := workflow.RunEx(context.Background(), s, bad,
		map[string]any{"message": "x"}, workflow.Options{}); err == nil {
		t.Fatal("define extending an unknown def must fail")
	}
}

const narrowSrc = `
WORKFLOW = workflow(
    description = "narrow",
    input = {"message": schema.string(required = True)},
    agents = ["assistant"],
)
def run(input):
    blocked = agent("reviewer", task = "should not run")
    ok = agent("assistant", task = "narrowed please", tools = ["echo"])
    direct = call("fs.read", {"path": "a.txt"})
    via_tools = tools.fs.read(path = "b.txt")
    return {"blocked": blocked.status, "ok": ok.status,
            "direct": direct.text, "via_tools": via_tools.text}
`

// §6.1 manifest agents allowlist + §6.2 tools= narrowing + call/tools.
func TestManifestAllowlistAndToolNarrowing(t *testing.T) {
	var ran atomic.Int32
	prov := &taskProvider{}
	sink := tracemem.New()
	s := openTest(t, prov, sink, testView(&ran))
	res, err := workflow.RunEx(context.Background(), s, narrowSrc,
		map[string]any{"message": "x"}, workflow.Options{})
	if err != nil {
		t.Fatalf("RunEx: %v", err)
	}
	m := res.Value.(map[string]any)
	if m["blocked"] != "failed: not_allowed" {
		t.Fatalf("blocked = %v", m["blocked"])
	}
	if prov.count("should not run") != 0 {
		t.Fatalf("allowlist-blocked agent was sampled: %v", prov.calls)
	}
	if m["ok"] != "done" {
		t.Fatalf("ok = %v", m["ok"])
	}
	// tools=["echo"] must narrow the advertised set for that call.
	prov.mu.Lock()
	var narrowedNames []string
	for i, c := range prov.calls {
		if c == "narrowed please" {
			narrowedNames = prov.tools[i]
		}
	}
	prov.mu.Unlock()
	// Loop built-ins (agent/workflow controls, §4.3) are added on top of
	// the narrowed view; no other registry tool may appear.
	for _, n := range narrowedNames {
		if n == "secret" || n == "fs.read" || n == "fs.write" {
			t.Fatalf("narrowed tools leaked %q: %v", n, narrowedNames)
		}
	}
	if !contains(narrowedNames, "echo") {
		t.Fatalf("narrowed tools missing echo: %v", narrowedNames)
	}
	if !strings.HasPrefix(fmt.Sprint(m["direct"]), "fs.read:") ||
		!strings.HasPrefix(fmt.Sprint(m["via_tools"]), "fs.read:") {
		t.Fatalf("tool results = %v / %v", m["direct"], m["via_tools"])
	}
	// operation.intent/outcome journaled for workflow tool calls (§6.3).
	ops, err := workflow.ListRecovery(context.Background(), sink, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 0 {
		t.Fatalf("clean run left recovery ops: %+v", ops)
	}
}

// §6.3 crash window: a mutating tool whose result was never committed
// blocks resume with recovery_required until resolved; a read-only one
// retries.
type toolCrashSink struct {
	trace.Sink
	disabled atomic.Bool
}

func (c *toolCrashSink) Append(ctx context.Context, events ...trace.Event) error {
	if !c.disabled.Load() {
		for _, ev := range events {
			if ev.Type == "wf.call" && strings.Contains(string(ev.Data), `"phase":"result"`) &&
				strings.Contains(string(ev.Data), `"builtin":"call"`) {
				return fmt.Errorf("simulated crash before tool result commit")
			}
		}
	}
	return c.Sink.Append(ctx, events...)
}

const toolSrc = `
WORKFLOW = workflow(description = "tool", input = {"message": schema.string(required = True)})
def run(input):
    r = call("fs.write", {"path": "x", "body": input.message})
    return {"text": r.text, "is_error": r.is_error}
`

func TestRecoveryRequiredAndResolve(t *testing.T) {
	var ran atomic.Int32
	prov := &taskProvider{}
	sink := &toolCrashSink{Sink: tracemem.New()}
	s := openTest(t, prov, sink, testView(&ran))
	runID := "wf-recovery-test"
	if _, err := workflow.RunEx(context.Background(), s, toolSrc,
		map[string]any{"message": "data"}, workflow.Options{RunID: runID}); err == nil {
		t.Fatal("expected simulated crash")
	}
	if ran.Load() != 1 {
		t.Fatalf("tool ran %d times, want 1", ran.Load())
	}
	sink.disabled.Store(true)
	ops, err := workflow.ListRecovery(context.Background(), sink, runID)
	if err != nil || len(ops) != 1 {
		t.Fatalf("recovery ops = %+v err=%v", ops, err)
	}
	if ops[0].Tool != "fs.write" || ops[0].Effect != "mutating" {
		t.Fatalf("op = %+v", ops[0])
	}
	// Resume must block, never repeat the uncertain mutation.
	res, err := workflow.Resume(context.Background(), s, runID)
	if err == nil || !strings.Contains(err.Error(), "recovery_required") ||
		res.Status != workflow.StatusRecoveryRequired {
		t.Fatalf("resume = %+v err=%v", res, err)
	}
	if ran.Load() != 1 {
		t.Fatalf("uncertain mutation repeated: ran=%d", ran.Load())
	}
	// Authorized resolution with evidence unblocks without re-running.
	if err := workflow.ResolveRecovery(context.Background(), s.Recorder(), runID,
		ops[0].Key, "success", "checked the file exists", "fs.write:done", "user:test"); err != nil {
		t.Fatal(err)
	}
	res, err = workflow.Resume(context.Background(), s, runID)
	if err != nil || res.Status != "done" {
		t.Fatalf("resume after resolve: %+v err=%v", res, err)
	}
	if ran.Load() != 1 {
		t.Fatalf("resolved op re-ran the tool: ran=%d", ran.Load())
	}
	m := res.Value.(map[string]any)
	if m["text"] != "fs.write:done" {
		t.Fatalf("value = %v", m)
	}
	if ops, _ := workflow.ListRecovery(context.Background(), sink, runID); len(ops) != 0 {
		t.Fatalf("resolved op still listed: %+v", ops)
	}
}

// §6.4: an agent with workflows:true calls the workflow built-in; the
// nested workflow's agents run and the result returns as a tool result.
func TestAgentWrittenWorkflow(t *testing.T) {
	var ran atomic.Int32
	// Parent assistant first calls the workflow tool with inline source,
	// then answers. A tiny scripted provider drives this.
	src := "WORKFLOW = workflow(description=\"inner\")\n" +
		"def run(input):\n" +
		"    r = agent(\"reviewer\", task = \"inner review\")\n" +
		"    return {\"inner\": r.text}\n"
	args, _ := json.Marshal(map[string]any{"source": src, "args": map[string]any{}})
	prov := &seqProvider{steps: []any{
		model.Response{Item: model.Item{Kind: model.KindAssistant, Finish: model.FinishToolCalls, ServedBy: "t",
			Calls: []model.ToolCall{{ID: "c1", Name: "workflow", Args: args, ArgsOK: true}}}, ServedModel: "t"},
		"inner review text", // the inner reviewer's sample
		"parent final",      // the parent's next step
	}}
	s := openTest(t, prov, tracemem.New(), testView(&ran))
	res, err := s.RunAgent(context.Background(), "assistant", "do it via workflow")
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("status %q text %q", res.Status, res.Text)
	}
	items := s.ThreadItems("")
	found := false
	for _, it := range items {
		if it.Kind == model.KindToolResult && strings.Contains(it.Text, "inner") {
			found = true
		}
	}
	if !found {
		t.Fatalf("workflow tool result missing from thread: %+v", items)
	}
}

// seqProvider serves queued items: strings become text responses.
type seqProvider struct {
	mu    sync.Mutex
	steps []any
	n     int
}

func (p *seqProvider) Sample(_ context.Context, _ model.Request, _ func(model.Delta)) (model.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n >= len(p.steps) {
		return model.Response{}, fmt.Errorf("seqProvider exhausted")
	}
	step := p.steps[p.n]
	p.n++
	switch s := step.(type) {
	case string:
		return model.Response{Item: model.Item{Kind: model.KindAssistant, Text: s,
			Finish: model.FinishStop, ServedBy: "t"}, ServedModel: "t"}, nil
	case model.Response:
		return s, nil
	}
	return model.Response{}, fmt.Errorf("bad step %T", step)
}

func (p *seqProvider) Model(_ context.Context, id string) (model.ModelInfo, error) {
	return model.ModelInfo{ID: id, ContextWindow: 128000}, nil
}

// res/: the shipped workflows catalogue and run end-to-end. The
// reviewer is structured, so this provider completes via complete().
type resProvider struct{ inner *taskProvider }

func (p *resProvider) Sample(ctx context.Context, req model.Request, d func(model.Delta)) (model.Response, error) {
	for _, t := range req.Tools {
		if t.Name == "complete" {
			task := ""
			for _, it := range req.Items {
				if it.Kind == model.KindUser {
					task = it.Text
				}
			}
			summary := "reviewed"
			if strings.Contains(task, "correctness") {
				summary = "rev-correct"
			} else if strings.Contains(task, "missing") {
				summary = "rev-missing"
			}
			args, _ := json.Marshal(map[string]any{"summary": summary, "output": map[string]any{
				"verdict": "approve", "findings": []any{}}})
			return model.Response{Item: model.Item{Kind: model.KindAssistant,
				Finish: model.FinishToolCalls, ServedBy: "test",
				Calls: []model.ToolCall{{ID: "cmp", Name: "complete", Args: args, ArgsOK: true}}},
				ServedModel: "test"}, nil
		}
	}
	return p.inner.Sample(ctx, req, d)
}

func (p *resProvider) Model(ctx context.Context, id string) (model.ModelInfo, error) {
	return p.inner.Model(ctx, id)
}

func TestShippedResWorkflows(t *testing.T) {
	loader := defs.Builtin()
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}
	names := loader.Workflows()
	if !contains(names, "agent-turn") || !contains(names, "layered") {
		t.Fatalf("catalogued workflows = %v", names)
	}
	var ran atomic.Int32
	prov := &resProvider{inner: &taskProvider{}}
	s, err := host.Open(context.Background(), host.Options{
		Config: config.Defaults(), View: testView(&ran), Provider: prov,
		Defs: loader, Trace: tracemem.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	turn, err := workflow.RunEx(context.Background(), s, "agent-turn",
		map[string]any{"message": "hello res"}, workflow.Options{})
	if err != nil {
		t.Fatalf("agent-turn: %v", err)
	}
	if m, _ := turn.Value.(map[string]any); m["status"] != "done" {
		t.Fatalf("agent-turn value = %v", turn.Value)
	}
	lay, err := workflow.RunEx(context.Background(), s, "layered",
		map[string]any{"message": "ship it"}, workflow.Options{})
	if err != nil {
		t.Fatalf("layered: %v", err)
	}
	m, _ := lay.Value.(map[string]any)
	if lay.Status != "done" || m["status"] != "done" {
		t.Fatalf("layered = %+v value=%v", lay, lay.Value)
	}
	if !strings.Contains(fmt.Sprint(m["text"]), "rev-correct") ||
		!strings.Contains(fmt.Sprint(m["text"]), "rev-missing") {
		t.Fatalf("layered synthesis missing reviews: %v", m["text"])
	}
}

// §6.2: start()/wait() over workflow handles and nested workflow_run.
func TestStartWaitAndNestedWorkflowRun(t *testing.T) {
	var ran atomic.Int32
	prov := &taskProvider{}
	s := openTest(t, prov, tracemem.New(), testView(&ran))
	inner := "WORKFLOW = workflow(description=\"inner\", input={\"message\": schema.string(required=True)})\n" +
		"def run(input):\n    r = agent(\"assistant\", task = \"INNER \" + input.message)\n    return {\"inner_text\": r.text}\n"
	src := `
WORKFLOW = workflow(description = "outer", input = {"message": schema.string(required = True), "inner": schema.string(required = True)})
def run(input):
    h1 = start(spec("assistant", task = "STARTED-A " + input.message))
    h2 = start(spec("assistant", task = "STARTED-B " + input.message))
    out = wait([h1, h2])
    nested = workflow_run(input.inner, {"message": input.message})
    return {"a": out.results[0].text, "b": out.results[1].text,
            "nested": nested.inner_text, "timeout": out.timeout}
`
	res, err := workflow.RunEx(context.Background(), s, src,
		map[string]any{"message": "m", "inner": inner}, workflow.Options{})
	if err != nil {
		t.Fatalf("RunEx: %v", err)
	}
	m := res.Value.(map[string]any)
	if !strings.Contains(fmt.Sprint(m["a"]), "STARTED-A") ||
		!strings.Contains(fmt.Sprint(m["b"]), "STARTED-B") ||
		!strings.Contains(fmt.Sprint(m["nested"]), "INNER") || m["timeout"] != false {
		t.Fatalf("value = %v", m)
	}
	if prov.count("INNER") != 1 {
		t.Fatalf("nested workflow agent calls = %v", prov.calls)
	}
}

// §6.5 adapters: spawn/send/status/wait through Starlark.
func TestSpawnSendStatusWait(t *testing.T) {
	var ran atomic.Int32
	prov := &taskProvider{}
	s := openTest(t, prov, tracemem.New(), testView(&ran))
	src := `
WORKFLOW = workflow(description = "spawn", input = {"message": schema.string(required = True)})
def run(input):
    h = spawn("assistant", task = "spawned ONE")
    st = status(h)
    out = wait(h)
    info = list_agents()
    return {"handle": h.id, "state_seen": st.state, "n_agents": len(info),
            "status": out.results[0].status, "text": out.results[0].text}
`
	res, err := workflow.RunEx(context.Background(), s, src,
		map[string]any{"message": "x"}, workflow.Options{})
	if err != nil {
		t.Fatalf("RunEx: %v", err)
	}
	m := res.Value.(map[string]any)
	if m["handle"] == "" || m["status"] != "done" ||
		!strings.Contains(fmt.Sprint(m["text"]), "spawned ONE") {
		t.Fatalf("value = %v", m)
	}
}
