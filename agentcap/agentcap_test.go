package agentcap_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agentcap"
	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/defs"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/memory"
	"github.com/craggy-io/agent-sdk/model"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
	_ "github.com/craggy-io/agent-sdk/workflow"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

func openStack(t *testing.T, opts agentcap.Options) (*registry.Registry, *host.Host) {
	t.Helper()
	r := registry.New()
	t.Cleanup(func() { r.Close() })
	prov := agenttest.NewScripted(
		agenttest.TextResponse("agent reply"),
		agenttest.TextResponse("llm reply"),
		agenttest.TextResponse("second reply"),
		agenttest.TextResponse("third reply"),
	)
	h, err := host.Open(context.Background(), host.Options{
		Config: config.Defaults(), Provider: prov,
		View:  host.NewRegistryView(r.View(registry.All)),
		Defs:  agentdef.Roots(defs.FS()),
		Trace: tracemem.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if err := agentcap.Register(r, h, opts); err != nil {
		t.Fatal(err)
	}
	return r, h
}

func invoke(t *testing.T, r *registry.Registry, ref string, in any) json.RawMessage {
	t.Helper()
	raw, _ := json.Marshal(in)
	out, err := r.View(registry.All).Invoke(context.Background(), ref, raw)
	if err != nil {
		t.Fatalf("%s: %v", ref, err)
	}
	return out
}

func TestRegisterAndRunAgent(t *testing.T) {
	r, _ := openStack(t, agentcap.Options{})
	out := invoke(t, r, "agents.list", map[string]any{})
	if !strings.Contains(string(out), `"assistant"`) {
		t.Fatalf("agents.list=%s", out)
	}
	out = invoke(t, r, "agents.run", map[string]any{"name": "assistant", "task": "hi"})
	var res struct {
		Status string `json:"status"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(out, &res); err != nil || res.Status != "done" || res.Text != "agent reply" {
		t.Fatalf("agents.run=%s", out)
	}
	out = invoke(t, r, "llm.sample", map[string]any{"prompt": "say hi"})
	if !strings.Contains(string(out), "llm reply") {
		t.Fatalf("llm.sample=%s", out)
	}
}

func TestStarlarkScriptUsesMemoryAndAgents(t *testing.T) {
	r, _ := openStack(t, agentcap.Options{Memory: memory.NewMemStore()})
	ctx := registry.WithView(context.Background(), r.View(registry.All))
	res, err := starlarkcap.Exec(ctx, starlarkcap.Program{
		Filename: "test.star",
		Source: []byte(`
def run(input):
    memory.put(namespace = "demo", key = "greeting", value = {"text": "hello"})
    got = memory.get(namespace = "demo", key = "greeting")
    profs = llm.profiles()
    return {"value": got["value"]["text"], "profiles": profs}
`),
	}, starlarkcap.DefaultLimits, starlarkcap.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Value), `"hello"`) || !strings.Contains(string(res.Value), "default") {
		t.Fatalf("script result=%s", res.Value)
	}
}

func TestDefinePromoteRoundTrip(t *testing.T) {
	root := t.TempDir()
	approved := false
	r, _ := openStack(t, agentcap.Options{
		Root: root,
		Approve: func(_ context.Context, kind, name string, payload []byte) bool {
			approved = kind == "agent" && name == "quick" && strings.Contains(string(payload), "quick")
			return approved
		},
	})
	src := `agent(name = "quick", extends = "base", profile = "fast", prompt = [{"q": "Be quick."}])`
	invoke(t, r, "agents.define", map[string]any{"source": src})
	out := invoke(t, r, "agents.promote", map[string]any{"name": "quick"})
	if !approved || !strings.Contains(string(out), "quick.star") {
		t.Fatalf("promote=%s approved=%v", out, approved)
	}
	written, err := os.ReadFile(filepath.Join(root, "agents", "quick.star"))
	if err != nil || string(written) != src {
		t.Fatalf("written=%q err=%v", written, err)
	}
	// The promoted file catalogues standing alone with the built-ins.
	loader := agentdef.Roots(defs.FS(), os.DirFS(root))
	res, err := loader.Resolve("quick", nil)
	if err != nil || res.Profile != "fast" || !strings.Contains(res.SystemPrompt, "Be quick.") {
		t.Fatalf("resolve promoted=%+v err=%v", res, err)
	}
}

func TestPromoteRefusedWithoutApprove(t *testing.T) {
	root := t.TempDir()
	r, _ := openStack(t, agentcap.Options{Root: root})
	invoke(t, r, "agents.define", map[string]any{
		"source": `agent(name = "quick", extends = "base")`,
	})
	raw, _ := json.Marshal(map[string]any{"name": "quick"})
	if _, err := r.View(registry.All).Invoke(context.Background(), "agents.promote", raw); err == nil {
		t.Fatal("promote without Approve succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "quick.star")); !os.IsNotExist(err) {
		t.Fatalf("file written despite refusal: %v", err)
	}
}

var _ = model.Usage{}
