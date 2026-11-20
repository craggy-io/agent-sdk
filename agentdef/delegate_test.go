package agentdef_test

import (
	"testing"
	"testing/fstest"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/foundation-kit/bundle"
)

func TestWorkflowDelegate(t *testing.T) {
	const message = `input = {"message": schema.string(required = True)}`
	cases := map[string]struct {
		workflow string
		want     string
	}{
		"hands the message on": {`WORKFLOW = workflow(chat = True, ` + message + `)

def run(input):
    return agent("react", task = input.message)
`, "react"},
		"positionally, as its one seat": {`WORKFLOW = workflow(chat = True, roles = [role("helper", agent = "react")], ` + message + `)

def run(req):
    return agent("react", req.message)
`, "react"},
		"not a chat workflow": {`WORKFLOW = workflow(` + message + `)

def run(input):
    return agent("react", task = input.message)
`, ""},
		"does more": {`WORKFLOW = workflow(chat = True, ` + message + `)

def run(input):
    first = agent("react", task = input.message)
    return agent("react", task = first.text)
`, ""},
		"changes the task": {`WORKFLOW = workflow(chat = True, ` + message + `)

def run(input):
    return agent("react", task = render("Plan: {m}", m = input.message))
`, ""},
		"passes more": {`WORKFLOW = workflow(chat = True, ` + message + `)

def run(input):
    return agent("react", task = input.message, phase = "answer")
`, ""},
		"has settings": {`WORKFLOW = workflow(chat = True, input = {"message": schema.string(required = True), "rounds": schema.integer()})

def run(input):
    return agent("react", task = input.message)
`, ""},
		"has many seats": {`WORKFLOW = workflow(chat = True, roles = [role("member", seats = "all")], ` + message + `)

def run(input):
    return agent("react", task = input.message)
`, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			l := open(t, fstest.MapFS{
				"bundle.yaml": file(`apiVersion: craggy.io/bundle/v1
metadata: {id: local.chat, version: 1.0.0}
contributions:
  - {id: chat, kind: agentsdk.workflow@1, path: workflow.star}
`),
				"workflow.star": file(c.workflow),
			})
			def, ok := bundle.Lookup(l.Catalog(), agentdef.WorkflowKind, "chat")
			if !ok {
				t.Fatal("workflow not loaded")
			}
			got, ok := def.Value.Delegate()
			if got != c.want || ok != (c.want != "") {
				t.Fatalf("Delegate() = %q, %v; want %q", got, ok, c.want)
			}
		})
	}
}
