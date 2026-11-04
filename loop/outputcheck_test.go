package loop_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
)

var verdictSchema = json.RawMessage(`{"type":"object","required":["verdict"],"properties":{"verdict":{"type":"string"}}}`)

func completeCall(id, verdict string) model.Response {
	return agenttest.CallsResponse(call(id, "complete", `{"summary":"s","output":{"verdict":"`+verdict+`"}}`))
}

func checkPolicy(check loop.OutputCheck, retries int) loop.Policy {
	return loop.Policy{Completion: "structured", Output: verdictSchema,
		OutputCheck: check, OutputCheckRetries: retries}
}

// A rejected completion goes back to the model as feedback and the
// run continues; the next, acceptable completion ends it.
func TestOutputCheckRejectsThenAccepts(t *testing.T) {
	prov := agenttest.NewScripted(completeCall("c1", "bad"), completeCall("c2", "good"))
	seen := 0
	check := func(_ context.Context, output json.RawMessage) (string, error) {
		seen++
		if strings.Contains(string(output), "bad") {
			return "verdict must not be bad", nil
		}
		return "", nil
	}
	h, res := runLoop(t, prov, newFakeTools(), checkPolicy(check, 2), nil)
	if res.Status != loop.StatusDone {
		t.Fatalf("status=%q", res.Status)
	}
	if seen != 2 {
		t.Fatalf("check ran %d times, want 2", seen)
	}
	if !strings.Contains(string(res.Output), "good") {
		t.Fatalf("output=%s", res.Output)
	}
	found := false
	for _, it := range h.items() {
		if it.Kind == model.KindNotice && strings.Contains(it.Text, "verdict must not be bad") {
			found = true
		}
	}
	if !found {
		t.Fatalf("feedback notice missing from transcript: %+v", h.items())
	}
}

// After maxRetries rejections the next completion is accepted as is,
// without consulting the check again.
func TestOutputCheckRetriesExhaustedAccepts(t *testing.T) {
	prov := agenttest.NewScripted(completeCall("c1", "bad"), completeCall("c2", "bad"), completeCall("c3", "bad"))
	seen := 0
	check := func(_ context.Context, _ json.RawMessage) (string, error) {
		seen++
		return "still bad", nil
	}
	_, res := runLoop(t, prov, newFakeTools(), checkPolicy(check, 2), nil)
	if res.Status != loop.StatusDone {
		t.Fatalf("status=%q", res.Status)
	}
	if seen != 2 {
		t.Fatalf("check ran %d times, want 2 (third completion accepted unchecked)", seen)
	}
	if !strings.Contains(string(res.Output), "bad") {
		t.Fatalf("output=%s", res.Output)
	}
}

// Zero retries disables the check entirely.
func TestOutputCheckZeroRetriesDisables(t *testing.T) {
	prov := agenttest.NewScripted(completeCall("c1", "bad"))
	seen := 0
	check := func(_ context.Context, _ json.RawMessage) (string, error) {
		seen++
		return "nope", nil
	}
	_, res := runLoop(t, prov, newFakeTools(), checkPolicy(check, 0), nil)
	if res.Status != loop.StatusDone {
		t.Fatalf("status=%q", res.Status)
	}
	if seen != 0 {
		t.Fatalf("check ran %d times with retries=0", seen)
	}
}

// A check error fails the run, like a BeforeStop hook error.
func TestOutputCheckErrorFailsRun(t *testing.T) {
	prov := agenttest.NewScripted(completeCall("c1", "good"))
	check := func(_ context.Context, _ json.RawMessage) (string, error) {
		return "", errors.New("store unreachable")
	}
	_, res := runLoop(t, prov, newFakeTools(), checkPolicy(check, 2), nil)
	if res.Status != "failed: hook" {
		t.Fatalf("status=%q, want failed: hook", res.Status)
	}
}
