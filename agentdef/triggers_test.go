package agentdef_test

import (
	"github.com/craggy-io/agent-sdk/agentdef"
	"testing"
)

func TestWorkflowTriggerDeclarationAllowsRuntimeNamesInModules(t *testing.T) {
	root := t.TempDir()
	writeBundle(t, root, map[string]string{
		"bundle.yaml": "apiVersion: craggy.io/bundle/v1\nmetadata: {id: test.trigger, version: 1.0.0}\napplication: {id: triggered, kind: agentsdk.workflow@1, path: workflow.star}\n",
		"workflow.star": `load("helper.star", "later")
WORKFLOW = workflow(input={"x":schema.string(required=True)}, triggers=[on_event("test.created@1",input={"x":event_field("data.x")})])
def run(input):
    return later(input.x)
`,
		"helper.star": `def later(x):
    return json.decode(call("example_read", {"x":x}).text)
`,
	})
	loader, err := agentdef.Open(t.Context(), agentdef.Options{Project: root})
	if err != nil {
		t.Fatal(err)
	}
	wf, ok := loader.Workflow("triggered")
	if !ok || len(wf.Triggers) != 1 {
		t.Fatal("trigger declaration missing")
	}
}
