package workflow_test

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/craggy-io/agent-sdk/model"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/workflow"
)

const threadSrc = `
WORKFLOW = workflow(
    description = "One agent, on the thread the input names, if any.",
    input = {"message": schema.string(required = True), "thread": schema.string()},
    agents = ["assistant"],
)
def run(input):
    thread = getattr(input, "thread", None) or ""
    if thread:
        return agent("assistant", task = input.message, thread = thread).text
    agent("assistant", task = input.message)
    return agent("assistant", task = "again: " + input.message).text
`

// userTexts are the user items a request carried, in order.
func userTexts(req model.Request) []string {
	var out []string
	for _, it := range req.Items {
		if it.Kind == model.KindUser {
			out = append(out, it.Text)
		}
	}
	return out
}

// Each top-level run of a workflow gets a thread of its own: a second run
// never reads the first run's conversation, while the agents of one run
// share theirs, and a thread the script names is carried on across runs.
func TestEachWorkflowRunHasAThreadOfItsOwn(t *testing.T) {
	var ran atomic.Int32
	prov := &taskProvider{}
	s := openTest(t, prov, tracemem.New(), testView(&ran))
	run := func(input map[string]any) {
		t.Helper()
		res, err := workflow.RunEx(t.Context(), s, threadSrc, input, workflow.Options{})
		if err != nil || res.Status != "done" {
			t.Fatalf("run %v: %+v %v", input, res, err)
		}
	}
	tasks := func(i int) []string {
		t.Helper()
		reqs := prov.requests()
		if i >= len(reqs) {
			t.Fatalf("only %d requests", len(reqs))
		}
		return userTexts(reqs[i])
	}

	run(map[string]any{"message": "first"})
	run(map[string]any{"message": "second"})
	// Requests: run 1 (first, again: first), run 2 (second, again: second).
	if got := strings.Join(tasks(1), "|"); got != "first|again: first" {
		t.Errorf("one run's agents share its thread: second call saw %q", got)
	}
	if got := strings.Join(tasks(2), "|"); got != "second" {
		t.Errorf("a second run read another run's history: %q", got)
	}

	run(map[string]any{"message": "named one", "thread": "kept"})
	run(map[string]any{"message": "named two", "thread": "kept"})
	if got := strings.Join(tasks(5), "|"); got != "named one|named two" {
		t.Errorf("a named thread is carried on across runs: %q", got)
	}
}
