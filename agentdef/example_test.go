package agentdef_test

import (
	"fmt"
	"testing/fstest"

	"github.com/craggy-io/agent-sdk/agentdef"
)

// ExampleLoader_Resolve loads a tiny catalogue from memory and
// resolves an agent through its extends chain.
func ExampleLoader_Resolve() {
	l := agentdef.Roots(fstest.MapFS{
		"agents/team.star": {Data: []byte(`
agent(
    name = "base",
    profile = "default",
    prompt = [{"role": "You are {{.Agent.Name}}."}, {"conduct": "Be careful."}],
    tools = tools_filter(max_effect = "any"),
)
agent(
    name = "reviewer",
    extends = "base",
    profile = "deep",
    prompt = [{"replace": "conduct", "text": "Be strict."}],
    tools = tools_filter(max_effect = "read-only"),
)
`)},
	})
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
