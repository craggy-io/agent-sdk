package agentcap_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agentcap"
	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/decide"
	"github.com/craggy-io/agent-sdk/defs"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/capability-services/registry"
)

func TestLLMDecide(t *testing.T) {
	loader, err := agentdef.Open(context.Background(), agentdef.Options{Shipped: defs.Sources()})
	if err != nil {
		t.Fatal(err)
	}
	r := registry.New()
	t.Cleanup(func() { r.Close() })
	prov := agenttest.NewScripted(agenttest.CallsResponse(model.ToolCall{ID: "c", Name: decide.ToolName,
		Args: json.RawMessage(`{"value":"support","confidence":0.8}`), ArgsOK: true}))
	cfg := config.Defaults()
	h, err := host.Open(context.Background(), host.Options{Config: cfg, Provider: prov,
		View: host.NewRegistryView(r.View(registry.All)), Defs: loader, Trace: tracemem.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if err := agentcap.Register(r, h, agentcap.Options{}); err != nil {
		t.Fatal(err)
	}
	out := invoke(t, r, "llm.decide", map[string]any{"kind": "choice", "question": "Which team?",
		"options": []map[string]string{{"value": "billing"}, {"value": "support"}}})
	if !strings.Contains(string(out), `"value":"support"`) || !strings.Contains(string(out), `"confidence":0.8`) {
		t.Fatalf("llm.decide=%s", out)
	}
	if want := config.Resolve(cfg, nil, "").Profile.Model; prov.Requests[0].Model != want {
		t.Errorf("asked %q, want the default profile's %q", prov.Requests[0].Model, want)
	}
	raw, _ := json.Marshal(map[string]any{"kind": "choice", "question": "?"})
	_, err = r.View(registry.All).Invoke(context.Background(), "llm.decide", raw)
	if registry.CodeOf(err) != registry.Invalid {
		t.Errorf("no options: %v", err)
	}
}
