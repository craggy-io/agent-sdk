package loop_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

func eventsOf(t *testing.T, h *harness, typ string) []trace.Event {
	t.Helper()
	evs, _, err := h.rec.Sink.ReadEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []trace.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// step.attempt_failed carries the failure class, status, provider,
// requested model and any usage reported for the failed attempt; that
// usage is charged like a sample's. step.response keeps usage presence.
func TestAttemptFailedFieldsAndUsage(t *testing.T) {
	failedUsage := model.Usage{PromptTokens: 5, CostUSD: 0.01,
		Reported: []string{model.UsagePromptTokens, model.UsageCostUSD}}
	prov := agenttest.NewScripted()
	prov.PushError(&model.Error{Status: 429, Code: "429", Message: "slow down", Transient: true,
		Provider: "SomeProvider", RetryAfter: time.Millisecond, Usage: &failedUsage})
	prov.PushError(&model.Error{Code: "idle_timeout", Transient: true})
	ok := agenttest.TextResponse("done")
	ok.Usage = model.Usage{PromptTokens: 0, CompletionTokens: 2,
		Reported: []string{model.UsagePromptTokens, model.UsageCompletionTokens}}
	prov.Push(ok)
	h, res := runLoop(t, prov, newFakeTools(), loop.Policy{}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}

	failed := eventsOf(t, h, "step.attempt_failed")
	if len(failed) != 2 {
		t.Fatalf("step.attempt_failed events = %d, want 2", len(failed))
	}
	var first map[string]any
	if err := json.Unmarshal(failed[0].Data, &first); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"error": "model error 429 429: slow down", "retries": float64(0),
		"code": model.FailRateLimited, "status": float64(429), "error_code": "429",
		"provider": "SomeProvider", "model": "scripted",
		"usage": map[string]any{"prompt_tokens": float64(5), "cost_usd": 0.01,
			"reported": []any{"prompt_tokens", "cost_usd"}},
	}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("first attempt_failed = %v\nwant %v", first, want)
	}
	var second map[string]any
	_ = json.Unmarshal(failed[1].Data, &second)
	if second["code"] != model.FailTransient || second["retries"] != float64(1) || second["model"] != "scripted" {
		t.Errorf("second attempt_failed = %v", second)
	}
	for _, k := range []string{"status", "provider", "usage"} {
		if _, has := second[k]; has {
			t.Errorf("second attempt_failed has %q though the error did not say: %v", k, second)
		}
	}

	resps := eventsOf(t, h, "step.response")
	if len(resps) != 1 {
		t.Fatalf("step.response events = %d", len(resps))
	}
	var rd struct {
		Usage model.Usage `json:"usage"`
	}
	_ = json.Unmarshal(resps[0].Data, &rd)
	if !reflect.DeepEqual(rd.Usage.Reported, []string{"prompt_tokens", "completion_tokens"}) {
		t.Errorf("step.response usage = %+v", rd.Usage)
	}
	// The node's usage includes the failed attempt's reported usage.
	wantUsage := model.Usage{PromptTokens: 5, CompletionTokens: 2, CostUSD: 0.01,
		Reported: []string{"prompt_tokens", "completion_tokens", "cost_usd"}}
	if !reflect.DeepEqual(res.Usage, wantUsage) {
		t.Errorf("result usage = %+v, want %+v", res.Usage, wantUsage)
	}
}

// A permanent failure stops the step with the provider's code and is
// recorded as code "permanent".
func TestAttemptFailedPermanent(t *testing.T) {
	prov := agenttest.NewScripted()
	prov.PushError(&model.Error{Status: 400, Code: "bad_request", Message: "nope"})
	h, res := runLoop(t, prov, newFakeTools(), loop.Policy{}, nil)
	if res.Status != "failed: bad_request" {
		t.Fatalf("status=%q", res.Status)
	}
	failed := eventsOf(t, h, "step.attempt_failed")
	var d map[string]any
	if len(failed) != 1 || json.Unmarshal(failed[0].Data, &d) != nil {
		t.Fatalf("attempt_failed = %v", failed)
	}
	if d["code"] != model.FailPermanent || d["status"] != float64(400) || d["error_code"] != "bad_request" {
		t.Errorf("attempt_failed = %v", d)
	}
}
