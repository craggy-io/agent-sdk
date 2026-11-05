package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/transcript"
)

// crashSink stands in for a process that dies right after committing a
// call.start: every later append is lost until revive.
type crashSink struct {
	*tracemem.Store
	dead atomic.Bool
}

func (c *crashSink) Append(ctx context.Context, events ...trace.Event) error {
	if c.dead.Load() {
		return errors.New("crashed")
	}
	if err := c.Store.Append(ctx, events...); err != nil {
		return err
	}
	for _, e := range events {
		if e.Type == "call.start" {
			c.dead.Store(true)
		}
	}
	return nil
}

type callEnd struct {
	CallID    string `json:"call_id"`
	Settled   string `json:"settled"`
	IsError   bool   `json:"is_error"`
	Recovered bool   `json:"recovered"`
}

// callEvents returns call.start node IDs and call.end payloads (with
// their node IDs) by call ID.
func callEvents(t *testing.T, sink trace.Sink) (map[string]string, map[string][]callEnd, map[string]string) {
	t.Helper()
	starts, ends, endNodes := map[string]string{}, map[string][]callEnd{}, map[string]string{}
	for _, e := range readAll(t, sink) {
		var d callEnd
		_ = json.Unmarshal(e.Data, &d)
		switch e.Type {
		case "call.start":
			starts[d.CallID] = e.NodeID
		case "call.end":
			ends[d.CallID] = append(ends[d.CallID], d)
			endNodes[d.CallID] = e.NodeID
		}
	}
	return starts, ends, endNodes
}

// §6.3 / §7.2: a call a crashed process left open (call.start, no
// call.end) is settled outcome_unknown when the session is resumed,
// in the trace and in the rebuilt thread, exactly once.
func TestRebuildReconcilesCallsOpenAtCrash(t *testing.T) {
	ctx := context.Background()
	sink := &crashSink{Store: tracemem.New()}
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(model.ToolCall{ID: "c1", Name: "echo", Args: json.RawMessage(`{}`), ArgsOK: true}),
		agenttest.TextResponse("never reached"),
	)
	opts := host.Options{ID: "crash-session", Config: config.Defaults(), View: echoView(),
		Provider: prov, Defs: openDefs(t, testRoot(t)), Trace: sink}
	s1, err := host.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s1.RunAgent(ctx, "solo", "call a tool") // fails: the trace died
	_ = s1.Close()

	starts, ends, _ := callEvents(t, sink.Store)
	if starts["c1"] == "" || len(ends["c1"]) != 0 {
		t.Fatalf("crash setup: starts=%v ends=%v, want c1 started and not ended", starts, ends)
	}

	sink.dead.Store(false) // the next process
	reopen := func() *host.Host {
		s, err := host.Open(ctx, opts)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if err := s.Rebuild(ctx, ""); err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		return s
	}
	checkThread := func(s *host.Host) {
		t.Helper()
		items := s.ThreadItems("")
		var res *model.Item
		for i := range items {
			if items[i].Kind == model.KindToolResult && items[i].CallID == "c1" {
				res = &items[i]
			}
		}
		if res == nil || res.Settled != model.SettleOutcomeUnknown || !res.IsError || !strings.Contains(res.Text, "outcome unknown") {
			t.Fatalf("rebuilt thread result for c1 = %+v", res)
		}
		proj, repairs := transcript.Normalise(items, "n", "scripted")
		if err := transcript.Check(proj); err != nil {
			t.Fatalf("rebuilt thread projection: %v", err)
		}
		for _, r := range repairs {
			if strings.Contains(r, "c1") {
				t.Errorf("projection still synthesises c1: %s", r)
			}
		}
	}

	s2 := reopen()
	_, ends, endNodes := callEvents(t, sink.Store)
	if got := ends["c1"]; len(got) != 1 || got[0].Settled != "outcome_unknown" || !got[0].IsError || !got[0].Recovered {
		t.Fatalf("reconciled call.end = %+v, want one outcome_unknown recovered error", got)
	}
	if endNodes["c1"] != starts["c1"] {
		t.Errorf("call.end node %q != call.start node %q", endNodes["c1"], starts["c1"])
	}
	checkThread(s2)
	_ = s2.Close()

	// Resuming again settles nothing twice but restores the result.
	s3 := reopen()
	defer func() { _ = s3.Close() }()
	if _, ends, _ := callEvents(t, sink.Store); len(ends["c1"]) != 1 {
		t.Fatalf("second Rebuild: call.end count = %d, want 1", len(ends["c1"]))
	}
	checkThread(s3)
}

// A run that closed every call needs no reconciliation.
func TestRebuildLeavesSettledCallsAlone(t *testing.T) {
	ctx := context.Background()
	sink := tracemem.New()
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(model.ToolCall{ID: "c1", Name: "echo", Args: json.RawMessage(`{}`), ArgsOK: true}),
		agenttest.TextResponse("done"),
	)
	opts := host.Options{ID: "clean-session", Config: config.Defaults(), View: echoView(),
		Provider: prov, Defs: openDefs(t, testRoot(t)), Trace: sink}
	s1, err := host.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := s1.RunAgent(ctx, "solo", "call a tool"); err != nil || res.Status != "done" {
		t.Fatalf("run: %+v %v", res, err)
	}
	_ = s1.Close()
	s2, err := host.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if err := s2.Rebuild(ctx, ""); err != nil {
		t.Fatal(err)
	}
	_, ends, _ := callEvents(t, sink)
	if got := ends["c1"]; len(got) != 1 || got[0].Recovered || got[0].Settled != "ran" {
		t.Fatalf("call.end for a settled call = %+v, want the original only", got)
	}
}
