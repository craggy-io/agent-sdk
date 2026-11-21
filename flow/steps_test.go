package flow_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/flow"
)

const triage = `load("@craggy/flow.star", "flow")

pipeline(
    title = "Triage",
    inputs = {"lab": flow.param("lab", label = "Lab"), "limit": flow.param("number", default = 5)},
    requires = ["artifacts.list@1", "core.workflows.run@1", "core.pipelines.approve@1"],
)

STEPS = [
    flow.read(id = "lab", input = "lab"),
    flow.call(id = "apks", capability = "artifacts.list@1", bind = {"lab": "lab"}, out = "apks"),
    flow.agent(id = "check", agent = "secrets-hunter", prompt = "check.md", input = "apks", out = "secrets"),
    flow.approve(id = "ok", prompt = "File {secrets.count} in {lab}?", show = "secrets"),
    flow.workflow(id = "file", workflow = "file-findings", input = "secrets", out = "filed"),
]

def run(ctx):
    return flow.run(ctx, STEPS)
`

func triageBundle() flow.Bundle {
	return flow.Bundle{flow.FileName: triage, "check.md": "Check them."}
}

// flow.param inputs read and write back as written, and Values fills
// defaults and refuses what is missing or unknown.
func TestValueInputs(t *testing.T) {
	h, err := flow.ReadHeader([]byte(triage))
	if err != nil {
		t.Fatal(err)
	}
	if lab := h.Inputs["lab"]; !lab.IsValue() || lab.Type != "lab" || lab.Label != "Lab" {
		t.Fatalf("lab = %+v", lab)
	}
	if got := flow.RenderHeader(h); !strings.Contains(got, `"lab": flow.param("lab", label = "Lab"), "limit": flow.param("number", default = 5)`) {
		t.Fatalf("rendered:\n%s", got)
	}
	vals, err := flow.Values(h, map[string]any{"lab": "banking"})
	if err != nil || !reflect.DeepEqual(vals, map[string]any{"lab": "banking", "limit": int64(5)}) {
		t.Fatalf("values = %v, %v", vals, err)
	}
	if _, err := flow.Values(h, nil); err == nil || !strings.Contains(err.Error(), "needs a value for lab") {
		t.Fatalf("missing lab: %v", err)
	}
	if _, err := flow.Values(h, map[string]any{"lab": "x", "nope": 1}); err == nil {
		t.Fatal("an unknown value was taken")
	}
	if _, err := flow.ReadHeader([]byte(`pipeline(title = "x", inputs = {"a": flow.param("colour")})`)); err == nil {
		t.Fatal("an unknown value type was taken")
	}
}

// The new steps check what the host has: value inputs, installed agents
// and workflows, and an approve outside any For each.
func TestNewStepsAreChecked(t *testing.T) {
	served := func(string) bool { return true }
	if probs := flow.Validate(triageBundle(), flow.Env{Served: served, Values: true,
		Agent: func(id string) bool { return id == "secrets-hunter" }, Workflow: func(n string) bool { return n == "file-findings" }}); len(probs) > 0 {
		t.Fatalf("problems: %v", probs)
	}
	studio := flow.Validate(triageBundle(), flow.Env{Host: "Craggy Studio", Served: func(r string) bool { return r != flow.ApproveRef }})
	want := []string{
		"requires core.pipelines.approve@1, which Craggy Studio doesn't offer",
		"input lab is a value (flow.param), which Craggy Studio doesn't give a run",
		"step check: Craggy Studio runs only agents made for the step, not installed ones",
	}
	got := problems(studio)
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("no %q in:\n%s", w, got)
		}
	}
	harness := problems(flow.Validate(triageBundle(), flow.Env{Values: true, Agent: func(string) bool { return false }, Workflow: func(string) bool { return false }}))
	for _, w := range []string{"step check: no installed agent secrets-hunter", "step file: no installed workflow file-findings"} {
		if !strings.Contains(harness, w) {
			t.Errorf("no %q in:\n%s", w, harness)
		}
	}
	inLoop := strings.Replace(flow.Template(flow.Header{Title: "x", Requires: []string{flow.ApproveRef}}), "STEPS = []", `STEPS = [
    flow.for_each(id = "each", over = "x", item = "y", steps = [flow.approve(id = "ok", prompt = "?")]),
]`, 1)
	if got := problems(flow.Validate(flow.Bundle{flow.FileName: inLoop}, flow.Env{})); !strings.Contains(got, "step ok: ask a person outside a For each") {
		t.Errorf("approve in a loop: %s", got)
	}
}

func problems(ps []flow.Problem) string {
	var msgs []string
	for _, p := range ps {
		msgs = append(msgs, p.Error())
	}
	return strings.Join(msgs, "\n")
}

// An agent step names its installed agent to studio.agent; an approve
// asks with its question filled in, and No ends the run there; a workflow
// step runs the workflow it names on its input.
func TestApproveAgentAndWorkflowSteps(t *testing.T) {
	inputs := map[string]any{"lab": "banking", "limit": 5}
	answer := func(yes bool) func(called) any {
		return func(c called) any {
			switch c.Ref {
			case "artifacts.list@1":
				return []any{"a.apk"}
			case "core.pipelines.approve@1":
				return map[string]any{"yes": yes}
			}
			return map[string]any{"status": "done", "value": map[string]any{"count": 2}}
		}
	}
	calls, events, _ := run(t, triageBundle(), inputs, answer(false))
	refs := []string{}
	for _, c := range calls {
		refs = append(refs, c.Ref)
	}
	if strings.Join(refs, " ") != "artifacts.list@1 core.workflows.run@1 core.pipelines.approve@1" {
		t.Fatalf("No still ran on: %v", refs)
	}
	if a := calls[1].Args["input"].(map[string]any); a["agent"] != "secrets-hunter" {
		t.Fatalf("agent input = %v", a)
	}
	if q := calls[2].Args; q["prompt"] != "File 2 in banking?" || q["step"] != "ok" {
		t.Fatalf("approve asked %v", q)
	}
	if last := events[len(events)-1]; last.Stopped != "ok" {
		t.Fatalf("last event = %+v", last)
	}

	calls, _, _ = run(t, triageBundle(), inputs, answer(true))
	wf := calls[len(calls)-1]
	if wf.Args["workflow"] != "file-findings" || !reflect.DeepEqual(wf.Args["input"], map[string]any{"count": float64(2)}) {
		t.Fatalf("workflow call = %+v", wf)
	}
}

// A host offers only the kinds it runs: none whose capability it doesn't
// serve, no workflow step without installed workflows, and no installed
// agent choice without installed agents.
func TestEnvKinds(t *testing.T) {
	studio := flow.Env{Served: func(r string) bool { return r != flow.ApproveRef }}
	names := map[string]flow.Kind{}
	for _, k := range studio.Kinds() {
		names[k.Name] = k
	}
	if _, ok := names[flow.Approve]; ok {
		t.Error("approve offered where it isn't served")
	}
	if _, ok := names["workflow"]; ok {
		t.Error("workflow offered without installed workflows")
	}
	for _, p := range names["agent"].Params {
		if p.Type == "agent" {
			t.Error("installed agent offered without installed agents")
		}
	}
	harness := flow.Env{Agent: func(string) bool { return true }, Workflow: func(string) bool { return true }}
	if got := len(harness.Kinds()); got != len(flow.Kinds) {
		t.Errorf("harness offers %d of %d kinds", got, len(flow.Kinds))
	}
	if flow.Kinds[2].Params[2].Type != "agent" {
		t.Error("Env.Kinds changed the shared kinds")
	}
}

// The same call in another key order has the same key; another input or
// capability doesn't.
func TestCallKey(t *testing.T) {
	a := flow.CallKey("x.y@1", json.RawMessage(`{"a":1,"b":[2,3]}`))
	if a != flow.CallKey("x.y@1", json.RawMessage(`{"b":[2,3],"a":1}`)) {
		t.Fatal("key order changed the key")
	}
	if a == flow.CallKey("x.y@1", json.RawMessage(`{"a":2,"b":[2,3]}`)) || a == flow.CallKey("x.z@1", json.RawMessage(`{"a":1,"b":[2,3]}`)) {
		t.Fatal("different calls share a key")
	}
}
