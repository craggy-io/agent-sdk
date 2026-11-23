package host_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
)

// growingView is a tool view a tool can be installed into while an agent
// waits, as a host's registry gains a plugin's capabilities.
type growingView struct {
	mu   sync.Mutex
	view *host.MapView
}

func (g *growingView) current() *host.MapView {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.view
}

func (g *growingView) install(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	defs := append(g.view.Tools(), model.Tool{Name: name, Description: name, Effect: "read-only", Schema: json.RawMessage(`{"type":"object"}`)})
	funcs := map[string]host.ToolFunc{}
	for _, d := range defs {
		funcs[d.Name] = func(context.Context, json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: d.Name}, nil
		}
	}
	g.view = host.NewMapView(defs, funcs)
}

func (g *growingView) Tools() []model.Tool       { return g.current().Tools() }
func (g *growingView) Groups() []model.ToolGroup { return g.current().Groups() }
func (g *growingView) Run(ctx context.Context, name string, args json.RawMessage) (model.ToolResult, error) {
	return g.current().Run(ctx, name, args)
}
func (g *growingView) WaitJob(ctx context.Context, id string) (model.ToolResult, bool) {
	return g.current().WaitJob(ctx, id)
}
func (g *growingView) Narrow(spec model.FilterSpec) host.ToolView { return g.current().Narrow(spec) }

func openNeedy(t *testing.T, view host.ToolView, needs host.NeedsFunc) (*host.Host, *agenttest.ScriptedProvider, trace.Sink) {
	t.Helper()
	prov := agenttest.NewScripted(agenttest.TextResponse("done"))
	sink := tracemem.New()
	h, err := host.Open(context.Background(), host.Options{
		Config: config.Defaults(), View: view, Provider: prov,
		Defs: openDefs(t, testRoot(t)), Trace: sink, Needs: needs,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, prov, sink
}

func requestTools(req model.Request) []string {
	var out []string
	for _, tool := range req.Tools {
		out = append(out, tool.Name)
	}
	return out
}

// A tool the definition names exactly that isn't installed is asked for
// before the agent starts (globs and what it excludes are not), and the
// agent is told it doesn't have it when nothing was installed.
func TestNeedsMissingToolsWarnsWhenNotInstalled(t *testing.T) {
	var asked []host.Needs
	h, prov, sink := openNeedy(t, echoView(), func(_ context.Context, n host.Needs) host.NeedsAnswer {
		asked = append(asked, n)
		return host.NeedsAnswer{Note: "There is no lab here."}
	})
	if _, err := h.RunAgent(context.Background(), "needy", "hi"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0].Agent != "needy" || !slices.Equal(asked[0].Missing, []string{"web_fetch"}) || !slices.Equal(asked[0].Tools, []string{"echo"}) {
		t.Fatalf("asked = %+v", asked)
	}
	system := prov.Requests[0].Items[0].Text
	if !strings.Contains(system, "There is no lab here.") || !strings.Contains(system, "not installed here, and the person did not install them: web_fetch") {
		t.Fatalf("system prompt = %q", system)
	}
	if eventTypes(readAll(t, sink))["node.tools_missing"] != 1 {
		t.Fatal("no node.tools_missing event")
	}
}

// A tool installed while the agent waited is given to it, and it is not
// warned.
func TestNeedsInstalledWhileWaitingIsGiven(t *testing.T) {
	view := &growingView{view: echoView()}
	h, prov, sink := openNeedy(t, view, func(_ context.Context, n host.Needs) host.NeedsAnswer {
		view.install("web_fetch")
		return host.NeedsAnswer{Again: true}
	})
	if _, err := h.RunAgent(context.Background(), "needy", "hi"); err != nil {
		t.Fatal(err)
	}
	if got := requestTools(prov.Requests[0]); !slices.Contains(got, "web_fetch") {
		t.Fatalf("tools = %v", got)
	}
	if strings.Contains(prov.Requests[0].Items[0].Text, "Tools you don't have") {
		t.Fatal("warned of a tool that was installed")
	}
	if eventTypes(readAll(t, sink))["node.tools_missing"] != 0 {
		t.Fatal("node.tools_missing after the install")
	}
}

// With no Needs, the agent is still told what it is missing.
func TestNeedsWarnsWithoutHook(t *testing.T) {
	h, prov, _ := openNeedy(t, echoView(), nil)
	if _, err := h.RunAgent(context.Background(), "needy", "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prov.Requests[0].Items[0].Text, "web_fetch") {
		t.Fatalf("system prompt = %q", prov.Requests[0].Items[0].Text)
	}
}
