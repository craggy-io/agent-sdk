package workflow_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/workflow"
)

// A catalogued workflow load()s a support file of its bundle and runs an
// agent from another layer.
func TestCataloguedWorkflowLoadsSupportFile(t *testing.T) {
	ctx := context.Background()
	shipped := fstest.MapFS{
		"bundle.yaml": {Data: []byte(`apiVersion: craggy.io/bundle/v1
metadata: {id: agentsdk.shipped, version: 1.0.0}
contributions:
  - {id: greet, kind: agentsdk.workflow@1, path: workflows/greet.star}
`)},
		// A support file: undeclared, only load()ed.
		"workflows/lib/text.star": {Data: []byte("def shout(s):\n    return s.upper() + \"!\"\n")},
		"workflows/greet.star": {Data: []byte(`load("./lib/text.star", "shout")

WORKFLOW = workflow(description = "Greet loudly.", input = {"message": schema.string(required = True)})

def run(input):
    r = agent("base", task = shout(input.message), phase = "greet")
    return {"text": r.text}
`)},
	}
	defs, err := agentdef.Open(ctx, agentdef.Options{Shipped: bundle.Embedded("shipped", shipped), Project: testRoot(t)})
	if err != nil {
		t.Fatal(err)
	}
	var ran atomic.Int32
	s, err := host.Open(ctx, host.Options{
		Config: config.Defaults(), View: testView(&ran), Provider: &taskProvider{},
		Defs: defs, Trace: tracemem.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := workflow.RunEx(ctx, s, "greet", map[string]any{"message": "hello"}, workflow.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := res.Value.(map[string]any)
	if res.Status != workflow.StatusDone || !strings.Contains(m["text"].(string), "HELLO!") {
		t.Fatalf("result=%+v", res)
	}
}

// A workflow file that uses a name workflows do not have fails when the
// bundle loads, not when it runs.
func TestCataloguedWorkflowChecked(t *testing.T) {
	_, err := agentdef.Open(context.Background(), agentdef.Options{Shipped: bundle.Embedded("shipped", fstest.MapFS{
		"bundle.yaml": {Data: []byte("apiVersion: craggy.io/bundle/v1\nmetadata: {id: agentsdk.shipped, version: 1.0.0}\ncontributions:\n  - {id: bad, kind: agentsdk.workflow@1, path: bad.star}\n")},
		"bad.star":    {Data: []byte("WORKFLOW = workflow(description = \"x\")\n\ndef run(input):\n    return nope()\n")},
	})})
	var le *bundle.LoadError
	if !errors.As(err, &le) || le.Diagnostics[0].Path != "bad.star" || le.Diagnostics[0].Line != 4 || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err=%v", err)
	}
}
