package agentdef_test

import (
	"context"
	"fmt"
	"testing/fstest"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/agent-sdk/agentdef"
)

// ExampleLoader_Resolve loads a tiny catalogue from memory and
// resolves an agent through its extends chain. The bundle.yaml lists one
// contribution per agent; its id is the agent's name.
func ExampleLoader_Resolve() {
	shipped := fstest.MapFS{
		"bundle.yaml": {Data: []byte(`apiVersion: craggy.io/bundle/v1
metadata: {id: agentsdk.shipped, version: 1.0.0}
contributions:
  - {id: base, kind: agentsdk.agent@1, path: agents/base.star}
  - {id: reviewer, kind: agentsdk.agent@1, path: agents/reviewer.star}
`)},
		"agents/base.star": {Data: []byte(`
agent(
    name = "base",
    profile = "default",
    prompt = [{"role": "You are {{.Agent.Name}}."}, {"conduct": "Be careful."}],
    tools = tools_filter(max_effect = "any"),
)
`)},
		"agents/reviewer.star": {Data: []byte(`
agent(
    name = "reviewer",
    extends = "base",
    profile = "deep",
    prompt = [{"replace": "conduct", "text": "Be strict."}],
    tools = tools_filter(max_effect = "read-only"),
)
`)},
	}
	l, err := agentdef.Open(context.Background(), agentdef.Options{Shipped: []bundle.Source{bundle.Embedded("shipped", shipped)}})
	if err != nil {
		panic(err)
	}
	r, err := l.Resolve("reviewer", nil)
	if err != nil {
		panic(err)
	}
	fmt.Println(r.Profile, r.Tools.MaxEffect, r.Chain)
	fmt.Println(r.SystemPrompt)
	// Output:
	// deep read-only [base reviewer]
	// You are reviewer.
	//
	// Be strict.
}
