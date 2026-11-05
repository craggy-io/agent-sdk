package workflow

import (
	"slices"
	"testing"

	"github.com/craggy-io/agent-sdk/agentdef"
)

// The names agentdef checks workflow files against are exactly the
// built-ins a workflow run provides.
func TestWorkflowBuiltinsMatchAgentdef(t *testing.T) {
	var got []string
	for name := range predeclared(&exec{}) {
		got = append(got, name)
	}
	slices.Sort(got)
	want := agentdef.WorkflowBuiltins()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("workflow built-ins %v\nagentdef.WorkflowBuiltins %v", got, want)
	}
}
