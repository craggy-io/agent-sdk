package host_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
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

func TestYAMLProgressiveToolsLoadRuntimeGroup(t *testing.T) {
	defs := echoView().Tools()
	for i := range defs {
		defs[i].Group = "util"
	}
	view := host.NewMapView(defs, map[string]host.ToolFunc{
		"echo": func(context.Context, json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "echo"}, nil
		},
		"secret": func(context.Context, json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "secret"}, nil
		},
	}).WithGroups(model.ToolGroup{Name: "util", Description: "Test tools."})
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(model.ToolCall{ID: "load", Name: "tools_load", ArgsOK: true, Args: json.RawMessage(`{"groups":["util"]}`)}),
		agenttest.TextResponse("done"),
	)
	s, err := host.Open(context.Background(), host.Options{
		Config: config.Defaults(), View: view, Provider: prov,
		Defs: openDefs(t, testRoot(t)), Trace: tracemem.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.RunAgent(context.Background(), "custom-progressive", "load tools"); err != nil {
		t.Fatal(err)
	}
	names := func(req model.Request) string {
		var out []string
		for _, tool := range req.Tools {
			out = append(out, tool.Name)
		}
		return strings.Join(out, ",")
	}
	first, loaded := names(prov.Requests[0]), names(prov.Requests[1])
	if !strings.Contains(first, "echo") || !strings.Contains(first, "tools_load") || strings.Contains(first, "secret") {
		t.Fatalf("initial tools = %s", first)
	}
	if !strings.Contains(loaded, "echo") || !strings.Contains(loaded, "secret") {
		t.Fatalf("loaded tools = %s", loaded)
	}
}
