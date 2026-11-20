package agentdef_test

import (
	"slices"
	"testing"
	"testing/fstest"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/foundation-kit/bundle"
)

func TestEveryAgentLeavesOutWorkflowHelpers(t *testing.T) {
	team := fstest.MapFS{
		"bundle.yaml": file(`apiVersion: craggy.io/bundle/v1
metadata: {id: local.team, version: 1.0.0}
contributions:
  - {id: analyst, kind: agentsdk.agent@1, path: analyst.star}
`),
		"analyst.star": file(`agent(name = "analyst")`),
	}
	// A chat workflow with a merger of its own, and a member role it casts
	// from everyone else.
	huddle := fstest.MapFS{
		"bundle.yaml": file(`apiVersion: craggy.io/bundle/v1
metadata: {id: local.huddle, version: 1.0.0}
contributions:
  - {id: huddle, kind: agentsdk.workflow@1, path: workflow.star}
  - {id: merger, kind: agentsdk.agent@1, path: merger.star}
`),
		"merger.star": file(`agent(name = "merger")`),
		"workflow.star": file(`WORKFLOW = workflow(chat = True, roles = [role("member", seats = "all"), role("merger", agent = "merger")])

def run(input):
    return agent("merger", task = input.message)
`),
	}
	// A chat workflow that declares no roles: its own agent is a member
	// anywhere else.
	solo := fstest.MapFS{
		"bundle.yaml": file(`apiVersion: craggy.io/bundle/v1
metadata: {id: local.solo, version: 1.0.0}
contributions:
  - {id: solo, kind: agentsdk.workflow@1, path: workflow.star}
  - {id: soloist, kind: agentsdk.agent@1, path: soloist.star}
`),
		"soloist.star": file(`agent(name = "soloist")`),
		"workflow.star": file(`WORKFLOW = workflow(chat = True)

def run(input):
    return agent("soloist", task = input.message)
`),
	}
	l, err := agentdef.Open(t.Context(), agentdef.Options{Shipped: []bundle.Source{
		bundle.Embedded("team", team), bundle.Embedded("huddle", huddle), bundle.Embedded("solo", solo),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := agentdef.EveryAgent(l.Catalog(), "local.huddle"); !slices.Equal(got, []string{"analyst", "soloist"}) {
		t.Errorf("huddle's members %v", got)
	}
	if got := agentdef.EveryAgent(l.Catalog(), "local.other"); !slices.Equal(got, []string{"analyst", "soloist"}) {
		t.Errorf("another workflow's members %v: the merger is there only for the huddle", got)
	}
}
