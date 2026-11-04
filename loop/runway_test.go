package loop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
)

// budgetNotices returns the budget notices in the thread, each with the
// number of assistant items before it (the steps taken when it arrived).
func budgetNotices(h *harness) (texts []string, atStep []int) {
	steps := 0
	for _, it := range h.items() {
		switch {
		case it.Kind == model.KindAssistant:
			steps++
		case it.Kind == model.KindNotice && strings.Contains(it.Text, `kind="budget"`):
			texts = append(texts, it.Text)
			atStep = append(atStep, steps)
		}
	}
	return texts, atStep
}

// callSteps scripts n distinct t1 calls (distinct args, so no loop
// notice), each costing usd, then final (a text final when omitted).
func callSteps(n int, usd float64, final ...model.Response) *agenttest.ScriptedProvider {
	prov := agenttest.NewScripted()
	for i := range n {
		r := agenttest.CallsResponse(call(fmt.Sprintf("c%d", i), "t1", fmt.Sprintf(`{"i":%d}`, i)))
		r.Usage.CostUSD = usd
		prov.Push(r)
	}
	if len(final) == 0 {
		final = []model.Response{agenttest.TextResponse("done")}
	}
	for _, r := range final {
		prov.Push(r)
	}
	return prov
}

// Run-tree steps: one wrap-up notice at 75%, one urgent notice at 90%,
// each once, at the step boundary after the crossing.
func TestRunwayNoticeSteps(t *testing.T) {
	h, res := runLoop(t, callSteps(18, 0), newFakeTools().echoTools("t1"), loop.Policy{},
		func(_ *loop.Input, d *loop.Deps) { d.Budget = loop.NewBudget(config.Limits{Steps: 20}) })
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	texts, at := budgetNotices(h)
	if len(texts) != 2 || at[0] != 15 || at[1] != 18 {
		t.Fatalf("budget notices at steps %v, want [15 18]: %q", at, texts)
	}
	if !strings.Contains(texts[0], "Budget check: 75% of the run step limit is used.") ||
		!strings.Contains(texts[0], "15/20 steps across the run, 15/80 of your own steps.") ||
		!strings.Contains(texts[0], "record durable notes or a handoff") ||
		!strings.Contains(texts[0], "then reply with your best partial result as your final answer rather than being cut off") {
		t.Errorf("wrap-up notice:\n%s", texts[0])
	}
	if !strings.Contains(texts[1], "Budget almost exhausted: 90% of the run step limit is used.") ||
		!strings.Contains(texts[1], "18/20 steps across the run") ||
		!strings.Contains(texts[1], "Start no new work") {
		t.Errorf("urgent notice:\n%s", texts[1])
	}
}

// Wall clock: measured on the budget's clock.
func TestRunwayNoticeWallClock(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	ft := newFakeTools().add("t1", "read-only", `{"type":"object"}`, func(context.Context, json.RawMessage) (model.ToolResult, error) {
		mu.Lock()
		now = now.Add(5 * time.Minute) // each call takes five minutes
		mu.Unlock()
		return model.ToolResult{Text: "ok"}, nil
	})
	b := loop.NewBudget(config.Limits{WallClock: time.Hour})
	b.SetClock(clock)
	h, res := runLoop(t, callSteps(11, 0), ft, loop.Policy{},
		func(_ *loop.Input, d *loop.Deps) { d.Budget = b })
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	texts, at := budgetNotices(h)
	if len(texts) != 2 || at[0] != 9 || at[1] != 11 {
		t.Fatalf("budget notices at steps %v, want [9 11]: %q", at, texts)
	}
	if !strings.Contains(texts[0], "75% of the wall-clock limit") || !strings.Contains(texts[0], "45m of 60m elapsed") {
		t.Errorf("wrap-up notice:\n%s", texts[0])
	}
	if !strings.Contains(texts[1], "91% of the wall-clock limit") || !strings.Contains(texts[1], "55m of 60m elapsed") {
		t.Errorf("urgent notice:\n%s", texts[1])
	}
}

// USD: actual charges count; the structured-completion advice names complete.
func TestRunwayNoticeUSD(t *testing.T) {
	prov := callSteps(4, 0.20, agenttest.CallsResponse(call("done", "complete", `{"summary":"s","output":{}}`)))
	h, res := runLoop(t, prov, newFakeTools().echoTools("t1"), loop.Policy{Completion: "structured"},
		func(_ *loop.Input, d *loop.Deps) { d.Budget = loop.NewBudget(config.Limits{USD: 1.00}) })
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	texts, at := budgetNotices(h)
	if len(texts) != 1 || at[0] != 4 {
		t.Fatalf("budget notices at steps %v, want [4]: %q", at, texts)
	}
	if !strings.Contains(texts[0], "80% of the USD limit") || !strings.Contains(texts[0], "$0.80 of $1.00 spent") ||
		!strings.Contains(texts[0], "then call complete(summary, output) with your best partial result rather than") {
		t.Errorf("notice:\n%s", texts[0])
	}
}

// The agent's own max_steps is a dimension even with no run limits.
func TestRunwayNoticeAgentMaxSteps(t *testing.T) {
	h, _ := runLoop(t, callSteps(12, 0), newFakeTools().echoTools("t1"), loop.Policy{MaxSteps: 10}, nil)
	texts, at := budgetNotices(h)
	if len(texts) != 2 || at[0] != 8 || at[1] != 9 {
		t.Fatalf("budget notices at steps %v, want [8 9]: %q", at, texts)
	}
	if !strings.Contains(texts[0], "80% of the agent step limit") || !strings.Contains(texts[0], "8/10 of your own steps") {
		t.Errorf("notice:\n%s", texts[0])
	}
}

// A jump past both thresholds gives only the urgent notice; an empty
// list disables notices.
func TestRunwayNoticeJumpAndDisabled(t *testing.T) {
	b := loop.NewBudget(config.Limits{USD: 1.00})
	b.ChargeUSD(0.95)
	h, _ := runLoop(t, callSteps(2, 0), newFakeTools().echoTools("t1"), loop.Policy{},
		func(_ *loop.Input, d *loop.Deps) { d.Budget = b })
	if texts, at := budgetNotices(h); len(texts) != 1 || at[0] != 0 || !strings.Contains(texts[0], "Budget almost exhausted") {
		t.Fatalf("jump: notices at %v: %q", at, texts)
	}

	h, _ = runLoop(t, callSteps(18, 0), newFakeTools().echoTools("t1"), loop.Policy{BudgetNoticeAt: []float64{}},
		func(_ *loop.Input, d *loop.Deps) { d.Budget = loop.NewBudget(config.Limits{Steps: 20}) })
	if texts, _ := budgetNotices(h); len(texts) != 0 {
		t.Fatalf("disabled, but got notices: %q", texts)
	}
}
