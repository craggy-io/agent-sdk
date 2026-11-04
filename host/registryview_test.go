package host_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/capability-services/registry"
)

type echoIn struct {
	Text string `json:"text"`
}
type echoOut struct {
	Text string `json:"text"`
	By   string `json:"by"`
}

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	r := registry.New()
	t.Cleanup(func() { r.Close() })
	err := r.Add(
		registry.Func("demo.echo", "Echo", func(ctx context.Context, in echoIn) (echoOut, error) {
			by := ""
			if c, ok := registry.CallerFrom(ctx); ok {
				by = c.ID
			}
			return echoOut{Text: in.Text, By: by}, nil
		}, registry.WithEffects(registry.ReadOnly)),
		registry.Func("demo.slow", "Slow", func(ctx context.Context, in echoIn) (echoOut, error) {
			return echoOut{Text: "slow:" + in.Text}, nil
		}, registry.AsLong(0)),
		registry.Func("demo.mutate", "Mutate", func(_ context.Context, in echoIn) (echoOut, error) {
			return echoOut{Text: "mutated"}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegistryViewToolsAndRun(t *testing.T) {
	r := testRegistry(t)
	v := host.NewRegistryView(r.View(registry.All))
	var names []string
	effects := map[string]string{}
	for _, tool := range v.Tools() {
		names = append(names, tool.Name)
		effects[tool.Name] = tool.Effect
		if len(tool.Schema) == 0 || !json.Valid(tool.Schema) {
			t.Errorf("tool %s schema=%s", tool.Name, tool.Schema)
		}
	}
	if len(names) != 3 || effects["demo_echo"] != "read-only" || effects["demo_slow"] != "long" || effects["demo_mutate"] != "mutating" {
		t.Fatalf("tools=%v effects=%v", names, effects)
	}
	res, err := v.Run(context.Background(), "demo_echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || res.IsError || !strings.Contains(res.Text, `"hi"`) {
		t.Fatalf("run=%+v err=%v", res, err)
	}
	res, _ = v.Run(context.Background(), "nope", json.RawMessage(`{}`))
	if !res.IsError {
		t.Fatalf("unknown tool run=%+v", res)
	}
}

func TestRegistryViewNarrowAndWaitJob(t *testing.T) {
	r := testRegistry(t)
	v := host.NewRegistryView(r.View(registry.All))
	ro := v.Narrow(model.FilterSpec{MaxEffect: "read-only"})
	if got := ro.Tools(); len(got) != 1 || got[0].Name != "demo_echo" {
		t.Fatalf("narrowed tools=%v", got)
	}
	if res, _ := ro.Run(context.Background(), "demo_mutate", json.RawMessage(`{}`)); !res.IsError {
		t.Fatalf("narrowed run of mutating tool=%+v", res)
	}

	res, err := v.Run(context.Background(), "demo_slow", json.RawMessage(`{"text":"x"}`))
	if err != nil || res.JobID == "" {
		t.Fatalf("long run=%+v err=%v", res, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done, ok := v.WaitJob(ctx, res.JobID)
	if !ok || done.IsError || !strings.Contains(done.Text, "slow:x") {
		t.Fatalf("wait=%+v ok=%v", done, ok)
	}
	if _, ok := v.WaitJob(ctx, "job-9999"); ok {
		t.Fatal("unknown job reported done")
	}
}
