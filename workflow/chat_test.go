package workflow_test

import (
	"context"
	"github.com/craggy-io/agent-sdk/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/workflow"
)

func TestInstalledChatReplay(t *testing.T) {
	root := writeChatBundle(t, "def run(step, max_steps):\n    step()\n")
	ctx := context.Background()
	provider := agenttest.NewScripted(agenttest.TextResponse("20 mL"), agenttest.TextResponse("20 mL"))
	h, err := host.Open(ctx, host.Options{Config: config.Defaults(), Defs: openDefs(t, root), Provider: provider, Trace: memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	result, err := workflow.RunEx(ctx, h, "test-chat", nil, workflow.Options{})
	if err != nil || result.Status != "done" {
		t.Fatalf("run=%+v err=%v", result, err)
	}
	replay, err := workflow.Resume(ctx, h, result.RunID)
	if err != nil || replay.Status != "done" || len(provider.Requests) != 1 {
		t.Fatalf("replay=%+v err=%v requests=%d", replay, err, len(provider.Requests))
	}
	if _, err := workflow.RunEx(ctx, h, writerWorkflow, nil, workflow.Options{}); err == nil {
		t.Fatal("accepted ad hoc chat")
	}
	checkInteractiveReplay(t, h)
	writeChatFile(t, root, "loop.star", "def run(step, max_steps):\n    step('Use one decimal place.')\n")
	if replay, err := workflow.Resume(ctx, h, result.RunID); err == nil || !strings.Contains(err.Error(), "changed") || replay.Status != workflow.StatusResumeIncompatible {
		t.Fatalf("changed role replay=%+v err=%v", replay, err)
	}
	if len(provider.Requests) != 2 {
		t.Errorf("unexpected model calls: %d", len(provider.Requests))
	}
}

func writeChatFile(t *testing.T, root, name, source string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
}

func checkInteractiveReplay(t *testing.T, h *host.Host) {
	t.Helper()
	ctx := context.Background()
	interactive, err := h.RunChat(ctx, "test-chat", "Round the amount.")
	if err != nil || interactive.Status != "done" {
		t.Fatalf("interactive=%+v err=%v", interactive, err)
	}
	if replay, err := workflow.Resume(ctx, h, interactive.RunID); err == nil || replay.Status != workflow.StatusResumeIncompatible {
		t.Fatalf("interactive replay=%+v err=%v", replay, err)
	}
}

const writerWorkflow = `WORKFLOW = workflow(chat=True)
def run(input):
    return agent("writer", task="Round the amount.")
`

func writeChatBundle(t *testing.T, loop string) string {
	t.Helper()
	root := t.TempDir()
	writeChatFile(t, root, "bundle.yaml", `apiVersion: craggy.io/bundle/v1
metadata: {id: local.chat, version: 1.0.0}
contributions:
  - {id: writer, kind: agentsdk.agent@1, path: agent.yaml}
  - {id: test-chat, kind: agentsdk.workflow@1, path: workflow.star}
`)
	writeChatFile(t, root, "agent.yaml", "name: writer\nloop: loop.star\n")
	writeChatFile(t, root, "workflow.star", writerWorkflow)
	writeChatFile(t, root, "loop.star", loop)
	return root
}

func TestChatTurnLimitsAndErrors(t *testing.T) {
	root := writeChatBundle(t, "def run(step, max_steps):\n    status, _ = step(intermediate=True)\n    if status == 'answer':\n        step()")
	provider := agenttest.NewScripted(agenttest.TextResponse("one step"))
	cfg := config.Defaults()
	cfg.Limits.Steps = 1
	h, err := host.Open(t.Context(), host.Options{Config: cfg, Defs: openDefs(t, root), Provider: provider, Trace: memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	result, err := h.RunChat(t.Context(), "test-chat", "Round the amount.")
	if err != nil || result.Status != "stopped: budget(steps)" || len(provider.Requests) != 1 {
		t.Fatalf("budget=%+v err=%v", result, err)
	}
	provider.PushError(&model.Error{Code: "invalid_input", Status: 400})
	result, err = h.RunChat(t.Context(), "test-chat", "Round the amount.")
	if err != nil || result.Status != "failed: invalid_input" {
		t.Fatalf("provider failure=%+v err=%v", result, err)
	}
}

type waitingProvider struct {
	*agenttest.ScriptedProvider
	started chan struct{}
}

func (p waitingProvider) Sample(ctx context.Context, _ model.Request, _ func(model.Delta)) (model.Response, error) {
	close(p.started)
	<-ctx.Done()
	return model.Response{}, ctx.Err()
}

func TestCancelInstalledChat(t *testing.T) {
	root := writeChatBundle(t, "def run(step, max_steps):\n    step()")
	provider := waitingProvider{agenttest.NewScripted(), make(chan struct{})}
	h, err := host.Open(t.Context(), host.Options{Config: config.Defaults(), Defs: openDefs(t, root), Provider: provider, Trace: memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	go func() {
		select {
		case <-provider.started:
			cancel()
		case <-ctx.Done():
		}
	}()
	result, err := h.RunChat(ctx, "test-chat", "Round the amount.")
	if ctx.Err() != context.Canceled || result.Status != "cancelled" {
		t.Fatalf("cancel=%+v ctx=%v err=%v", result, ctx.Err(), err)
	}
}
