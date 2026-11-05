package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/adapters/tracesqlite"
	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
)

// makeTraceDB runs one scripted agent (tool call, then text) into a
// temp SQLite trace and returns db path + a step id + run id + node id.
func makeTraceDB(t *testing.T) (db, runID, nodeID, stepID string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: craggy.io/bundle/v1\nmetadata: {id: agentsdk.project, version: 1.0.0}\ncontributions:\n  - {id: base, kind: agentsdk.agent@1, path: agents/base.star}\n"
	if err := os.WriteFile(filepath.Join(root, "bundle.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `agent(name = "base", profile = "default", prompt = [{"p": "You are base. The SYSTEM-ALPHA prompt."}], loop = loop_spec(completion = "text"))`
	if err := os.WriteFile(filepath.Join(root, "agents", "base.star"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	defs, err := agentdef.Open(context.Background(), agentdef.Options{Project: root})
	if err != nil {
		t.Fatal(err)
	}
	db = filepath.Join(t.TempDir(), "trace.db")
	store, err := tracesqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	view := host.NewMapView(
		[]model.Tool{{Name: "echo", Description: "echo back", Effect: "read-only",
			Schema: json.RawMessage(`{"type":"object"}`)}},
		map[string]host.ToolFunc{"echo": func(_ context.Context, args json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "echoed:" + string(args)}, nil
		}})
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(model.ToolCall{ID: "c1", Name: "echo",
			Args: json.RawMessage(`{"msg":"TOOLCALL-BETA"}`), ArgsOK: true}),
		agenttest.TextResponse("all done"),
	)
	s, err := host.Open(context.Background(), host.Options{
		Config: config.Defaults(), View: view, Provider: prov,
		Defs: defs, Trace: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.RunAgent(context.Background(), "base", "please echo something")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Fatalf("status %q", res.Status)
	}
	runID, nodeID = res.RunID, res.NodeID
	evs, _, err := store.ReadEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Type == "step.request" && e.StepID != "" {
			stepID = e.StepID // last step also carries the tool call in history
		}
	}
	_ = s.Close()
	store.Close()
	if stepID == "" {
		t.Fatal("no step.request recorded")
	}
	return db, runID, nodeID, stepID
}

func cli(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	if code != 0 {
		t.Logf("stderr: %s", errb.String())
	}
	return out.String(), code
}

// §7.3: runs/tree/step --context against a real temp trace DB.
func TestQueryCommands(t *testing.T) {
	db, runID, nodeID, stepID := makeTraceDB(t)

	out, code := cli(t, "--db", db, "runs")
	if code != 0 || !strings.Contains(out, runID) {
		t.Fatalf("runs code=%d out=%q", code, out)
	}
	out, code = cli(t, "--db", db, "tree", runID[:12]) // prefix form
	if code != 0 || !strings.Contains(out, nodeID) || !strings.Contains(out, "steps=") {
		t.Fatalf("tree code=%d out=%q", code, out)
	}
	out, code = cli(t, "--db", db, "step", stepID, "--context")
	if code != 0 {
		t.Fatalf("step --context code=%d", code)
	}
	// The brief's smoke assertion: system prompt + a tool call present.
	if !strings.Contains(out, "SYSTEM-ALPHA") {
		t.Fatalf("step --context missing system prompt:\n%s", out)
	}
	if !strings.Contains(out, "echo(") || !strings.Contains(out, "TOOLCALL-BETA") {
		t.Fatalf("step --context missing tool call:\n%s", out)
	}
	out, code = cli(t, "--db", db, "step", stepID, "--diff")
	if code != 0 || !strings.Contains(out, "new items") {
		t.Fatalf("step --diff code=%d out=%q", code, out)
	}
	out, code = cli(t, "--db", db, "tools", nodeID)
	if code != 0 || !strings.Contains(out, "echo") {
		t.Fatalf("tools code=%d out=%q", code, out)
	}
	// Tool metadata comes from step.request (effect + schema hash),
	// not only the node summary.
	if !strings.Contains(out, "#") || !strings.Contains(out, "[read-only]") && !strings.Contains(out, "[mutating]") && !strings.Contains(out, "[") {
		t.Fatalf("tools missing effect/schema-hash metadata:\n%s", out)
	}
	out, code = cli(t, "--db", db, "usage", runID)
	if code != 0 || !strings.Contains(out, "total: steps=") || !strings.Contains(out, "cost=$") || !strings.Contains(out, "by model:") {
		t.Fatalf("usage code=%d out=%q", code, out)
	}
	out, code = cli(t, "--db", db, "usage", "--node", nodeID, "--json")
	if code != 0 || !strings.Contains(out, `"by_tool"`) || !strings.Contains(out, "echo") {
		t.Fatalf("usage --json code=%d out=%.300q", code, out)
	}
	out, code = cli(t, "--db", db, "grep", runID)
	if code != 0 {
		t.Fatalf("grep code=%d out=%q", code, out)
	}
	out, code = cli(t, "--db", db, "recovery", runID)
	if code != 0 || !strings.Contains(out, "no unresolved") {
		t.Fatalf("recovery code=%d out=%q", code, out)
	}
	out, code = cli(t, "--db", db, "tail", "--once")
	if code != 0 || !strings.Contains(out, "run.start") {
		t.Fatalf("tail code=%d out=%.200q", code, out)
	}
	out, code = cli(t, "--db", db, "replay", stepID, "--print")
	if code != 0 || !strings.Contains(out, "SYSTEM-ALPHA") {
		t.Fatalf("replay --print code=%d out=%.200q", code, out)
	}
	out, code = cli(t, "--db", db, "inputs", "primary")
	if code != 0 {
		t.Fatalf("inputs code=%d out=%q", code, out)
	}
	if _, code := cli(t, "--db", db, "step", "nonexistent-id"); code == 0 {
		t.Fatal("unknown step must fail")
	}
}

// run without a key fails with the handoff message, not a panic.
func TestRunNeedsKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	var out, errb bytes.Buffer
	code := run([]string{"--mem", "run", "--agent", "assistant", "hello"}, &out, &errb)
	if code == 0 || !strings.Contains(errb.String(), "OPENROUTER_API_KEY") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}
