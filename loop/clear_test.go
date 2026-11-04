package loop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
)

// bigTools serves "big" (a distinct 2 KB result per call) and "tiny"
// (too short to clear).
func bigTools() *fakeTools {
	n := 0
	return newFakeTools().
		add("big", "read-only", `{"type":"object"}`, func(context.Context, json.RawMessage) (model.ToolResult, error) {
			n++
			return model.ToolResult{Text: fmt.Sprintf("%02d", n) + strings.Repeat("x", 2000)}, nil
		}).
		add("tiny", "read-only", `{"type":"object"}`, func(context.Context, json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "ok"}, nil
		})
}

// clearedIn lists the call IDs whose results req sends as a cleared marker.
func clearedIn(req model.Request) []string {
	var out []string
	for _, it := range req.Items {
		if it.Kind == model.KindToolResult && strings.HasPrefix(it.Text, "[result cleared from context") {
			out = append(out, it.CallID)
		}
	}
	return out
}

func TestClearResultsInBatchesAndReadBack(t *testing.T) {
	script := []model.Response{agenttest.CallsResponse(call("t0", "tiny", `{}`))}
	for i := 1; i <= 8; i++ {
		script = append(script, agenttest.CallsResponse(call(fmt.Sprintf("b%d", i), "big", `{}`)))
	}
	script = append(script,
		agenttest.CallsResponse(call("rr", "result_read", `{"call_id":"b1"}`)),
		agenttest.TextResponse("done"))
	prov := agenttest.NewScripted(script...)
	h, res := runLoop(t, prov, bigTools(), loop.Policy{
		ClearResultsAt: 1e-9, KeepRecentResults: 3, CompactAt: 0.99, ToolResultBytes: 1 << 20,
	}, func(in *loop.Input, _ *loop.Deps) { in.ContextWindow = 10_000_000 })
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}

	// Request k carries t0 (never clearable) and k-1 clearable results:
	// b1.., then rr. The boundary moves three results at a time, so
	// three to five of the newest stay intact.
	b3 := []string{"b1", "b2", "b3"}
	want := [][]string{nil, nil, nil, nil, nil, nil, nil, b3, b3, b3,
		{"b1", "b2", "b3", "b4", "b5", "b6"}}
	if len(prov.Requests) != len(want) {
		t.Fatalf("requests=%d, want %d", len(prov.Requests), len(want))
	}
	for k, req := range prov.Requests {
		if got := clearedIn(req); strings.Join(got, ",") != strings.Join(want[k], ",") {
			t.Errorf("request %d cleared %v, want %v", k, got, want[k])
		}
	}

	// The stored transcript keeps every result, and result_read(call_id)
	// returns the cleared one in full.
	byCall := map[string]string{}
	for _, it := range resultsOf(h) {
		byCall[it.CallID] = it.Text
	}
	if !strings.HasPrefix(byCall["b1"], "01xxx") || len(byCall["b1"]) != 2002 {
		t.Fatalf("stored b1 = %.20q (%d bytes), want the full result", byCall["b1"], len(byCall["b1"]))
	}
	if byCall["rr"] != byCall["b1"] {
		t.Fatalf("result_read(call_id=b1) = %.20q, want the stored b1 result", byCall["rr"])
	}
}

func TestClearResultsBelowTriggerSendsEverything(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("b1", "big", `{}`)),
		agenttest.CallsResponse(call("b2", "big", `{}`)),
		agenttest.CallsResponse(call("b3", "big", `{}`)),
		agenttest.TextResponse("done"))
	_, res := runLoop(t, prov, bigTools(), loop.Policy{KeepRecentResults: 1},
		func(in *loop.Input, _ *loop.Deps) { in.ContextWindow = 10_000_000 })
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	for k, req := range prov.Requests {
		if got := clearedIn(req); len(got) > 0 {
			t.Errorf("request %d cleared %v below the default trigger", k, got)
		}
	}
}

func TestResultReadNeedsExactlyOneKey(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("r1", "result_read", `{}`)),
		agenttest.CallsResponse(call("r2", "result_read", `{"handle":"r_x","call_id":"c"}`)),
		agenttest.CallsResponse(call("r3", "result_read", `{"call_id":"nope"}`)),
		agenttest.TextResponse("done"))
	h, _ := runLoop(t, prov, bigTools(), loop.Policy{}, nil)
	want := map[string]string{
		"r1": "pass exactly one of handle and call_id",
		"r2": "pass exactly one of handle and call_id",
		"r3": "unknown call_id: nope",
	}
	for _, it := range resultsOf(h) {
		if w, ok := want[it.CallID]; ok && (!it.IsError || !strings.Contains(it.Text, w)) {
			t.Errorf("%s: error=%v text=%q, want error containing %q", it.CallID, it.IsError, it.Text, w)
		}
	}
}
