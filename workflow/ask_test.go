package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/workflow"
)

const gateSrc = `
WORKFLOW = workflow(description = "gate", input = {"message": schema.string(required = True)})
ANSWER = {"type": "object", "required": ["decision"], "properties": {"decision": {"type": "string"}, "text": {"type": "string"}}, "additionalProperties": False}
def run(input):
    before = tools.echo(x = input.message)
    a = ask("approve", payload = {"q": input.message}, schema = ANSWER)
    after = tools.echo(x = a.decision)
    return {"before": before.text, "after": after.text, "decision": a.decision}
`

func runToGate(t *testing.T, s *host.Host, src string) workflow.Result {
	t.Helper()
	res, err := workflow.RunEx(context.Background(), s, src, map[string]any{"message": "hi"}, workflow.Options{})
	if err != nil {
		t.Fatalf("RunEx: %v", err)
	}
	if res.Status != workflow.StatusWaiting || res.Pending == nil {
		t.Fatalf("status=%q pending=%v, want waiting with pending", res.Status, res.Pending)
	}
	if res.Pending.Kind != "approve" || res.Pending.Key == "" || res.Pending.RunID != res.RunID {
		t.Fatalf("pending=%+v", res.Pending)
	}
	return res
}

// ask suspends with waiting + pending; Answer validates; Resume
// returns the answer and continues, reusing the call recorded
// before the gate instead of re-running it.
func TestAskSuspendAnswerResume(t *testing.T) {
	var ran atomic.Int32
	s := openTest(t, &taskProvider{}, tracemem.New(), testView(&ran))
	res := runToGate(t, s, gateSrc)
	if got := ran.Load(); got != 1 {
		t.Fatalf("tools ran %d times before gate, want 1", got)
	}
	// Wrong-key and schema-invalid answers are rejected.
	if err := workflow.Answer(context.Background(), s.Recorder(), res.RunID, "nope", json.RawMessage(`{"decision":"go"}`)); err == nil {
		t.Fatal("answer to unknown key accepted")
	}
	if err := workflow.Answer(context.Background(), s.Recorder(), res.RunID, res.Pending.Key, json.RawMessage(`{"decision":3}`)); err == nil {
		t.Fatal("schema-invalid answer accepted")
	}
	if err := workflow.Answer(context.Background(), s.Recorder(), res.RunID, res.Pending.Key, json.RawMessage(`{"decision":"go"}`)); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	// A second answer is rejected.
	if err := workflow.Answer(context.Background(), s.Recorder(), res.RunID, res.Pending.Key, json.RawMessage(`{"decision":"stop"}`)); err == nil {
		t.Fatal("second answer accepted")
	}
	out, err := workflow.Resume(context.Background(), s, res.RunID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if out.Status != workflow.StatusDone {
		t.Fatalf("status=%q value=%v", out.Status, out.Value)
	}
	v, _ := out.Value.(map[string]any)
	if v["decision"] != "go" || !strings.Contains(v["after"].(string), "go") {
		t.Fatalf("value=%v", v)
	}
	// Only the post-gate echo ran on resume; the pre-gate one replayed.
	if got := ran.Load(); got != 2 {
		t.Fatalf("tools ran %d times total, want 2 (pre-gate call must replay)", got)
	}
	// And the gate is gone afterwards.
	waiting, err := workflow.ListWaiting(context.Background(), s.Sink())
	if err != nil || len(waiting) != 0 {
		t.Fatalf("ListWaiting=%v err=%v", waiting, err)
	}
	if err := workflow.Answer(context.Background(), s.Recorder(), res.RunID, res.Pending.Key, json.RawMessage(`{"decision":"late"}`)); err == nil {
		t.Fatal("answer after completion accepted")
	}
}

// ListWaiting finds the gate from a fresh session over the same
// sink, and the fresh session can answer and resume it.
func TestAskListWaitingFreshSession(t *testing.T) {
	var ran atomic.Int32
	sink := tracemem.New()
	s1 := openTest(t, &taskProvider{}, sink, testView(&ran))
	res := runToGate(t, s1, gateSrc)
	if err := s1.Close(); err != nil {
		t.Fatalf("close s1: %v", err)
	}
	s2, err := host.Open(context.Background(), host.Options{
		Config: config.Defaults(), View: testView(&ran), Provider: &taskProvider{},
		Defs: agentdef.DirRoots(testRoot(t)), Trace: sink,
	})
	if err != nil {
		t.Fatalf("open s2: %v", err)
	}
	defer func() { _ = s2.Close() }()
	waiting, err := workflow.ListWaiting(context.Background(), s2.Sink())
	if err != nil {
		t.Fatalf("ListWaiting: %v", err)
	}
	if len(waiting) != 1 || waiting[0].Key != res.Pending.Key || waiting[0].RunID != res.RunID {
		t.Fatalf("waiting=%+v", waiting)
	}
	if err := workflow.Answer(context.Background(), s2.Recorder(), res.RunID, res.Pending.Key, json.RawMessage(`{"decision":"go"}`)); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	out, err := workflow.Resume(context.Background(), s2, res.RunID)
	if err != nil || out.Status != workflow.StatusDone {
		t.Fatalf("Resume: %v %+v", err, out)
	}
}

// An expired deadline is recorded as a timed_out result on resume:
// ask returns {"timed_out": true}, the script continues, and a late
// answer is rejected.
func TestAskTimeout(t *testing.T) {
	var ran atomic.Int32
	s := openTest(t, &taskProvider{}, tracemem.New(), testView(&ran))
	src := `
WORKFLOW = workflow(description = "gate", input = {"message": schema.string(required = True)})
def run(input):
    a = ask("approve", payload = {}, schema = {"type": "object"}, timeout = 0.05)
    return {"answer": a}
`
	res := runToGate(t, s, src)
	if res.Pending.Deadline.IsZero() {
		t.Fatal("pending has no deadline")
	}
	time.Sleep(80 * time.Millisecond)
	if err := workflow.Answer(context.Background(), s.Recorder(), res.RunID, res.Pending.Key, json.RawMessage(`{}`)); err == nil {
		t.Fatal("late answer accepted")
	}
	out, err := workflow.Resume(context.Background(), s, res.RunID)
	if err != nil || out.Status != workflow.StatusDone {
		t.Fatalf("Resume: %v %+v", err, out)
	}
	v, _ := out.Value.(map[string]any)
	ans, _ := v["answer"].(map[string]any)
	if ans["timed_out"] != true {
		t.Fatalf("value=%v, want answer.timed_out true", v)
	}
	// Replay reuses the recorded timeout.
	out2, err := workflow.Resume(context.Background(), s, res.RunID)
	if err != nil || out2.Status != workflow.StatusDone {
		t.Fatalf("second Resume: %v %+v", err, out2)
	}
}

// An ask inside a nested workflow_run suspends the whole run under
// the outer run ID.
func TestAskNestedWorkflowRunSuspends(t *testing.T) {
	var ran atomic.Int32
	s := openTest(t, &taskProvider{}, tracemem.New(), testView(&ran))
	inner := "WORKFLOW = workflow(description = 'inner', input = {'message': schema.string(required = True)})\n" +
		"def run(input):\n" +
		"    a = ask('approve', payload = {'q': input.message}, schema = {'type': 'object', 'required': ['decision'], 'properties': {'decision': {'type': 'string'}}})\n" +
		"    return {'decision': a.decision}\n"
	outer := `
WORKFLOW = workflow(description = "outer", input = {"message": schema.string(required = True), "inner": schema.string(required = True)})
def run(input):
    return workflow_run(input.inner, input = {"message": input.message})
`
	res, err := workflow.RunEx(context.Background(), s, outer,
		map[string]any{"message": "hi", "inner": inner}, workflow.Options{})
	if err != nil {
		t.Fatalf("RunEx: %v", err)
	}
	if res.Status != workflow.StatusWaiting || res.Pending == nil {
		t.Fatalf("status=%q pending=%v", res.Status, res.Pending)
	}
	if err := workflow.Answer(context.Background(), s.Recorder(), res.RunID, res.Pending.Key, json.RawMessage(`{"decision":"nested-go"}`)); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	out, err := workflow.Resume(context.Background(), s, res.RunID)
	if err != nil || out.Status != workflow.StatusDone {
		t.Fatalf("Resume: %v %+v", err, out)
	}
	v, _ := out.Value.(map[string]any)
	if v["decision"] != "nested-go" {
		t.Fatalf("value=%v", v)
	}
}

// completeProvider answers every sample with a complete call whose
// verdict flips from "bad" to "good" once it has seen the output
// check's feedback notice.
type completeProvider struct {
	mu    sync.Mutex
	calls int
	tools [][]string
}

func (p *completeProvider) Sample(_ context.Context, req model.Request, _ func(model.Delta)) (model.Response, error) {
	verdict := "bad"
	names := make([]string, 0, len(req.Tools))
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}
	for _, it := range req.Items {
		if strings.Contains(it.Text, "must be good") {
			verdict = "good"
		}
	}
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.tools = append(p.tools, names)
	p.mu.Unlock()
	args := `{"summary":"done","output":{"verdict":"` + verdict + `"}}`
	return model.Response{
		Item: model.Item{Kind: model.KindAssistant, Finish: model.FinishToolCalls, ServedBy: "test",
			Calls: []model.ToolCall{{ID: "c" + string(rune('0'+n)), Name: "complete", Args: json.RawMessage(args), ArgsOK: true}}},
		ServedModel: "test",
	}, nil
}

func (p *completeProvider) Model(_ context.Context, id string) (model.ModelInfo, error) {
	return model.ModelInfo{ID: id, ContextWindow: 128000}, nil
}

const checkSrc = `
WORKFLOW = workflow(description = "checked", input = {"message": schema.string(required = True)})
OUT = {"type": "object", "required": ["verdict"], "properties": {"verdict": {"type": "string"}}}
def run(input):
    r = agent("assistant", task = input.message, output = OUT, check = "verdict.check", check_args = {"want": "good"}, retries = 2)
    return {"status": r.status, "verdict": r.output.verdict, "retries": r.check_retries_used}
`

func checkView(checkCalls *atomic.Int32) *host.MapView {
	tools := []model.Tool{
		{Name: "verdict.check", Description: "check", Effect: "read-only", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "fs.read", Description: "read", Effect: "read-only", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "fs.write", Description: "write", Effect: "mutating", Schema: json.RawMessage(`{"type":"object"}`)},
	}
	return host.NewMapView(tools, map[string]host.ToolFunc{
		"verdict.check": func(_ context.Context, args json.RawMessage) (model.ToolResult, error) {
			checkCalls.Add(1)
			var in struct {
				Output struct {
					Verdict string `json:"verdict"`
				} `json:"output"`
				Want string `json:"want"`
			}
			_ = json.Unmarshal(args, &in)
			if in.Output.Verdict != in.Want {
				return model.ToolResult{Text: `{"feedback":"verdict must be good"}`}, nil
			}
			return model.ToolResult{Text: `{"feedback":""}`}, nil
		},
		"fs.read": func(_ context.Context, _ json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "r"}, nil
		},
		"fs.write": func(_ context.Context, _ json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "w"}, nil
		},
	})
}

// agent(check=, retries=) rejects the first completion in-run and
// accepts the fixed one; on Resume the journaled agent result
// replays and the check does not run again.
func TestAgentCheckInWorkflowAndReplay(t *testing.T) {
	var checkCalls atomic.Int32
	prov := &completeProvider{}
	s := openTest(t, prov, tracemem.New(), checkView(&checkCalls))
	res, err := workflow.RunEx(context.Background(), s, checkSrc, map[string]any{"message": "go"}, workflow.Options{})
	if err != nil || res.Status != workflow.StatusDone {
		t.Fatalf("RunEx: %v %+v", err, res)
	}
	v, _ := res.Value.(map[string]any)
	if v["verdict"] != "good" || v["status"] != "done" {
		t.Fatalf("value=%v", v)
	}
	if got := checkCalls.Load(); got != 2 {
		t.Fatalf("check ran %d times, want 2 (reject + accept)", got)
	}
	if got := fmt.Sprint(v["retries"]); got != "1" {
		t.Fatalf("check_retries_used=%v (%T), want 1", v["retries"], v["retries"])
	}
	out, err := workflow.Resume(context.Background(), s, res.RunID)
	if err != nil || out.Status != workflow.StatusDone {
		t.Fatalf("Resume: %v %+v", err, out)
	}
	if got := checkCalls.Load(); got != 2 {
		t.Fatalf("check ran again on replay: %d calls", got)
	}
	prov.mu.Lock()
	calls := prov.calls
	prov.mu.Unlock()
	if calls != 2 {
		t.Fatalf("provider sampled %d times, want 2 (no re-sample on replay)", calls)
	}
}

// agent(tools = {"max_effect": "read-only"}) narrows by effect:
// the mutating tool is neither advertised nor callable, while the
// workflow's own view still has it.
func TestAgentReadOnlyToolFilter(t *testing.T) {
	var checkCalls atomic.Int32
	prov := &completeProvider{}
	s := openTest(t, prov, tracemem.New(), checkView(&checkCalls))
	src := `
WORKFLOW = workflow(description = "ro", input = {"message": schema.string(required = True)})
OUT = {"type": "object", "required": ["verdict"], "properties": {"verdict": {"type": "string"}}}
def run(input):
    r = agent("assistant", task = input.message, output = OUT, tools = {"max_effect": "read-only"})
    w = tools.fs.write(x = 1)
    return {"status": r.status, "wrote": w.text}
`
	res, err := workflow.RunEx(context.Background(), s, src, map[string]any{"message": "go"}, workflow.Options{})
	if err != nil || res.Status != workflow.StatusDone {
		t.Fatalf("RunEx: %v %+v", err, res)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.tools) == 0 {
		t.Fatal("provider never sampled")
	}
	for _, names := range prov.tools {
		if contains(names, "fs.write") {
			t.Fatalf("mutating tool advertised to read-only agent: %v", names)
		}
		if !contains(names, "fs.read") {
			t.Fatalf("read-only tool missing: %v", names)
		}
	}
}
