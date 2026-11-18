package host_test

import (
	"context"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/model"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
)

func TestAuthoredLoopsInheritAndNarrowRuntimeTools(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.TextResponse("all"),
		agenttest.TextResponse("read"),
		agenttest.TextResponse("filtered"),
	)
	s := openTest(t, prov, tracemem.New(), nil)
	for _, name := range []string{"custom-all", "custom-read", "custom-filter"} {
		if _, err := s.RunAgent(context.Background(), name, "inspect tools"); err != nil {
			t.Fatalf("RunAgent(%s): %v", name, err)
		}
	}
	has := func(req model.Request, name string) bool {
		for _, tool := range req.Tools {
			if tool.Name == name {
				return true
			}
		}
		return false
	}
	if !has(prov.Requests[0], "echo") || !has(prov.Requests[0], "secret") {
		t.Fatalf("unrestricted authored loop tools = %+v", prov.Requests[0].Tools)
	}
	if !has(prov.Requests[1], "echo") || has(prov.Requests[1], "secret") {
		t.Fatalf("read-only authored loop tools = %+v", prov.Requests[1].Tools)
	}
	if !has(prov.Requests[2], "echo") || has(prov.Requests[2], "secret") || has(prov.Requests[2], "missing") {
		t.Fatalf("include/exclude authored loop tools = %+v", prov.Requests[2].Tools)
	}
}
