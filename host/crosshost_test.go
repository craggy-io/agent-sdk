package host_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
)

// An agent's tool may open a host of its own and run an agent there, as a
// Craggy Studio pipeline step does when the assistant starts a run. That
// agent is not the calling agent's child: it starts a run of the inner
// host, with no delegate allowlist from the outer agent, which names none.
func TestAnotherHostsNodeIsNotTheCaller(t *testing.T) {
	root := testRoot(t)
	var inner host.Result
	var innerErr error
	tools := []model.Tool{{Name: "open", Description: "open", Effect: "read-only", Schema: json.RawMessage(`{"type":"object"}`)}}
	view := host.NewMapView(tools, map[string]host.ToolFunc{
		"open": func(ctx context.Context, _ json.RawMessage) (model.ToolResult, error) {
			h, err := host.Open(ctx, host.Options{
				Config: config.Defaults(), View: echoView(), Provider: agenttest.NewScripted(agenttest.TextResponse("grand done")),
				Defs: openDefs(t, root), Trace: tracemem.New(),
			})
			if err != nil {
				return model.ToolResult{}, err
			}
			defer h.Close()
			inner, innerErr = h.RunAgent(ctx, "grand", "hi")
			return model.ToolResult{Text: inner.Text}, nil
		},
	})
	outer, err := host.Open(context.Background(), host.Options{
		Config: config.Defaults(), View: view,
		Provider: agenttest.NewScripted(
			agenttest.CallsResponse(model.ToolCall{ID: "o1", Name: "open", Args: json.RawMessage(`{}`)}),
			agenttest.TextResponse("solo done"),
		),
		Defs: openDefs(t, root), Trace: tracemem.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	res, err := outer.RunAgent(context.Background(), "solo", "go")
	if err != nil || res.Status != "done" {
		t.Fatalf("outer: res=%+v err=%v", res, err)
	}
	if innerErr != nil || inner.Status != "done" || inner.Text != "grand done" {
		t.Fatalf("inner: res=%+v err=%v", inner, innerErr)
	}
	if inner.RunID == "" || inner.RunID == res.RunID {
		t.Errorf("the inner agent ran in run %q, the outer in %q: it should be a run of its own", inner.RunID, res.RunID)
	}
}
