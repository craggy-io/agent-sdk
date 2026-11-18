package loop_test

import (
	"context"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/transcript"
	"go.starlark.net/starlark"
)

func TestAuthoredBatchSettlement(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		settled          bool
	}{
		{"abandoned", "status, act = step()", "unsettled calls", false},
		{"double use", "status, act = step()\n    act()\n    act()", "already been settled", true},
		{"resample", "step()\n    step()", "before sampling again", false},
		{"script failure", "step()\n    fail('test stop')", "test stop", false},
		{"no sample", "pass", "before a final response", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			globals, err := starlark.ExecFile(&starlark.Thread{Name: "test"}, "loop.star", "def run(step, max_steps):\n    "+tc.body, nil)
			if err != nil {
				t.Fatal(err)
			}
			globals.Freeze()
			provider := agenttest.NewScripted(agenttest.CallsResponse(call("skill", "skill", `{"name":"format"}`)))
			history := transcript.New()
			in := loop.Input{Behavior: globals["run"].(*starlark.Function), Thread: history, Skills: map[string]string{"format": "Use metric units."}, Task: "Format a quantity."}
			_, err = loop.Run(context.Background(), loop.Deps{Provider: provider}, in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			agenttest.CheckTranscript(t, history.Items())
			if tc.name == "no sample" {
				if len(provider.Requests) != 0 {
					t.Fatal("model ran after script returned without sampling")
				}
				return
			}
			results := resultsOf(&harness{thread: history})
			if len(results) != 1 || results[0].IsError == tc.settled {
				t.Errorf("settlements: %+v", results)
			}
		})
	}
}

func TestAuthoredLoopCannotDispatchRegistryCalls(t *testing.T) {
	globals, err := starlark.ExecFile(&starlark.Thread{Name: "test"}, "loop.star", "def run(step, max_steps):\n    status, act = step()\n    act()\n    step()", nil)
	if err != nil {
		t.Fatal(err)
	}
	globals.Freeze()
	registry := newFakeTools().echoTools("echo")
	provider := agenttest.NewScripted(agenttest.CallsResponse(call("external", "echo", "{}")), agenttest.TextResponse("finished"))
	h, res := runLoop(t, provider, registry, loop.Policy{}, func(in *loop.Input, _ *loop.Deps) { in.Behavior = globals["run"].(*starlark.Function) })
	if res.Status != loop.StatusDone || len(registry.called()) > 0 {
		t.Fatalf("status=%s calls=%v", res.Status, registry.called())
	}
	results := resultsOf(h)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("unexpected results: %v", results)
	}
	for _, req := range provider.Requests {
		for _, tool := range req.Tools {
			if tool.Name == "echo" {
				t.Fatal("registry tool offered to authored loop")
			}
		}
	}
}
