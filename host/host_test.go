package host_test

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
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
)

func echoView() *host.MapView {
	tools := []model.Tool{
		{Name: "echo", Description: "echo", Effect: "read-only", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "secret", Description: "secret", Effect: "mutating", Schema: json.RawMessage(`{"type":"object"}`)},
	}
	return host.NewMapView(tools, map[string]host.ToolFunc{
		"echo": func(_ context.Context, args json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "echo:" + string(args)}, nil
		},
		"secret": func(_ context.Context, _ json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "secret!"}, nil
		},
	})
}

func openTest(t *testing.T, prov model.Provider, sink trace.Sink, cfgMut func(*config.SessionConfig)) *host.Host {
	t.Helper()
	cfg := config.Defaults()
	if cfgMut != nil {
		cfgMut(&cfg)
	}
	s, err := host.Open(context.Background(), host.Options{
		Config: cfg, View: echoView(), Provider: prov,
		Defs: openDefs(t, testRoot(t)), Trace: sink,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func readAll(t *testing.T, sink trace.Sink) []trace.Event {
	t.Helper()
	var out []trace.Event
	var after int64
	for {
		evs, next, err := sink.ReadEvents(context.Background(), after, 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 0 {
			return out
		}
		out = append(out, evs...)
		after = next
	}
}

func eventTypes(evs []trace.Event) map[string]int {
	m := map[string]int{}
	for _, e := range evs {
		m[e.Type]++
	}
	return m
}

// §2/§7.1: RunAgent happy path produces the run/node/step trace tree.
func TestRunAgentHappyPathTraceTree(t *testing.T) {
	sink := tracemem.New()
	prov := agenttest.NewScripted(agenttest.TextResponse("hello back"))
	s := openTest(t, prov, sink, nil)

	res, err := s.RunAgent(context.Background(), "solo", "hi")
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if res.Status != "done" || res.Text != "hello back" || res.RunID == "" || res.NodeID == "" {
		t.Fatalf("res = %+v", res)
	}
	evs := readAll(t, sink)
	types := eventTypes(evs)
	for _, want := range []string{"run.start", "run.end", "node.start", "node.end", "agentdef.resolved", "step.request", "step.response", "host.start"} {
		if types[want] == 0 {
			t.Errorf("missing event %s; got %v", want, types)
		}
	}
	// Ordering: run.start < node.start < step.request < node.end < run.end.
	idx := map[string]int{}
	for i, e := range evs {
		if _, ok := idx[e.Type]; !ok {
			idx[e.Type] = i
		}
	}
	if !(idx["run.start"] < idx["node.start"] && idx["node.start"] < idx["step.request"]) {
		t.Errorf("bad tree order: %v", idx)
	}
	var nodeEnd, runEnd int
	for i, e := range evs {
		if e.Type == "node.end" {
			nodeEnd = i
		}
		if e.Type == "run.end" {
			runEnd = i
		}
	}
	if nodeEnd > runEnd {
		t.Errorf("node.end after run.end")
	}
	if got := s.ThreadItems(""); len(got) < 3 || got[1].Text != "hi" || got[2].Text != "hello back" {
		t.Errorf("primary thread = %+v", got)
	}
	agenttest.CheckTranscript(t, s.ThreadItems(""))
}

// §3.2: unknown profile falls back to default + config.fallback, never fails.
func TestProfileFallbackEvent(t *testing.T) {
	sink := tracemem.New()
	prov := agenttest.NewScripted(agenttest.TextResponse("ok"))
	s := openTest(t, prov, sink, nil)
	res, err := s.RunAgent(context.Background(), "badprof", "hi")
	if err != nil || res.Status != "done" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	found := false
	for _, e := range readAll(t, sink) {
		if e.Type == "config.fallback" && strings.Contains(string(e.Data), `"want":"nope"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("no config.fallback event with want=nope")
	}
	if len(prov.Requests) != 1 || prov.Requests[0].Model != config.Defaults().Profiles["default"].Model {
		t.Fatalf("request model = %+v", prov.Requests)
	}
}

// §2 chat continuity: primary thread carries across runs.
func TestThreadContinuity(t *testing.T) {
	prov := agenttest.NewScripted(agenttest.TextResponse("first answer"), agenttest.TextResponse("second answer"))
	s := openTest(t, prov, tracemem.New(), nil)
	if _, err := s.RunAgent(context.Background(), "solo", "first question"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunAgent(context.Background(), "solo", "second question"); err != nil {
		t.Fatal(err)
	}
	if len(prov.Requests) != 2 {
		t.Fatalf("requests = %d", len(prov.Requests))
	}
	var texts []string
	for _, it := range prov.Requests[1].Items {
		texts = append(texts, it.Text)
	}
	joined := strings.Join(texts, "|")
	for _, want := range []string{"first question", "first answer", "second question"} {
		if !strings.Contains(joined, want) {
			t.Errorf("second request missing %q: %s", want, joined)
		}
	}
}

// §5.7/§3: run-tree USD budget stops mid-run.
func TestBudgetUSDStops(t *testing.T) {
	call := model.ToolCall{ID: "c1", Name: "echo", Args: json.RawMessage(`{}`), ArgsOK: true}
	r1 := agenttest.CallsResponse(call)
	r1.Usage.CostUSD = 0.6
	call.ID = "c2"
	r2 := agenttest.CallsResponse(call)
	r2.Usage.CostUSD = 0.6
	prov := agenttest.NewScripted(r1, r2, agenttest.TextResponse("never"))
	s := openTest(t, prov, tracemem.New(), func(c *config.SessionConfig) { c.Limits.USD = 1.0 })
	res, err := s.RunAgent(context.Background(), "solo", "go")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != "stopped: budget(usd)" {
		t.Fatalf("status = %q (steps %d)", res.Status, res.Steps)
	}
}

// §5.7: run-tree step budget charged before work, shared with children.
func TestBudgetStepsStops(t *testing.T) {
	call := model.ToolCall{ID: "c1", Name: "echo", Args: json.RawMessage(`{}`), ArgsOK: true}
	prov := agenttest.NewScripted(agenttest.CallsResponse(call), agenttest.CallsResponse(call), agenttest.TextResponse("never"))
	s := openTest(t, prov, tracemem.New(), func(c *config.SessionConfig) { c.Limits.Steps = 2 })
	res, _ := s.RunAgent(context.Background(), "solo", "go")
	if res.Status != "stopped: budget(steps)" {
		t.Fatalf("status = %q", res.Status)
	}
}

func agentCall(id, name, task string) model.ToolCall {
	args, _ := json.Marshal(map[string]string{"name": name, "task": task})
	return model.ToolCall{ID: id, Name: "agent", Args: args, ArgsOK: true}
}

// §4.3/§11: delegation with narrowed tools at concurrency=1 must complete;
// the child must not see (or run) a tool outside its narrowed view.
func TestDelegationConcurrencyOneAndNarrowing(t *testing.T) {
	secretCall := model.ToolCall{ID: "s1", Name: "secret", Args: json.RawMessage(`{}`), ArgsOK: true}
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(agentCall("a1", "child", "do child work")), // parent step 1
		agenttest.CallsResponse(secretCall),                                // child step 1: tries secret
		agenttest.TextResponse("child done"),                               // child step 2
		agenttest.TextResponse("parent done"),                              // parent step 2
	)
	sink := tracemem.New()
	s := openTest(t, prov, sink, func(c *config.SessionConfig) { c.Limits.Concurrency = 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := s.RunAgent(ctx, "parent", "delegate please")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != "done" || res.Text != "parent done" {
		t.Fatalf("res = %+v", res)
	}
	// Child request (2nd overall) must not advertise secret.
	if len(prov.Requests) < 2 {
		t.Fatalf("requests = %d", len(prov.Requests))
	}
	for _, tool := range prov.Requests[1].Tools {
		if tool.Name == "secret" {
			t.Fatal("child was advertised the secret tool outside its view")
		}
	}
	// The secret call settles unknown_tool (it never ran).
	settled := false
	for _, e := range readAll(t, sink) {
		if e.Type == "call.end" && strings.Contains(string(e.Data), "unknown_tool") {
			settled = true
		}
		if strings.Contains(string(e.Data), "secret!") {
			t.Fatal("secret tool actually ran for the child")
		}
	}
	if !settled {
		t.Error("secret call was not settled unknown_tool")
	}
	// Caller chain: parent thread's agent result mentions child text.
	joined := ""
	for _, it := range s.ThreadItems("") {
		joined += it.Text
	}
	if !strings.Contains(joined, "child done") {
		t.Errorf("parent thread missing child result: %s", joined)
	}
}

// §3.1 depth cap across the node tree.
func TestDepthCap(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(agentCall("a1", "child", "go deeper")), // parent
		agenttest.CallsResponse(agentCall("a2", "grand", "too deep")),  // child tries grand
		agenttest.TextResponse("child done"),
		agenttest.TextResponse("parent done"),
	)
	s := openTest(t, prov, tracemem.New(), func(c *config.SessionConfig) { c.Limits.Depth = 1 })
	res, err := s.RunAgent(context.Background(), "parent", "go")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("status = %q", res.Status)
	}
	joined := ""
	for _, it := range s.ThreadItems("") {
		joined += it.Text
	}
	if !strings.Contains(joined, "depth") && !strings.Contains(joined, "child done") {
		t.Errorf("expected depth-cap error to reach the child as a tool result; thread: %s", joined)
	}
	if len(prov.Requests) != 4 {
		t.Errorf("grand should never sample; requests = %d", len(prov.Requests))
	}
}

// Non-delegate must be refused by the agent built-in (§6.5 spawn contract).
func TestDelegateNotAllowed(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(agentCall("a1", "grand", "sneak")),
		agenttest.TextResponse("parent done"),
	)
	s := openTest(t, prov, tracemem.New(), nil)
	res, err := s.RunAgent(context.Background(), "parent", "go")
	if err != nil || res.Status != "done" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	joined := ""
	for _, it := range s.ThreadItems("") {
		joined += it.Text
	}
	if !strings.Contains(joined, "not an allowed delegate") {
		t.Errorf("delegate refusal missing: %s", joined)
	}
}

// blockingProvider blocks the first Sample until released/cancelled.
type blockingProvider struct {
	inner   *agenttest.ScriptedProvider
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingProvider) Sample(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	blocked := false
	b.once.Do(func() { blocked = true; close(b.entered) })
	if blocked {
		select {
		case <-b.release:
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
	}
	return b.inner.Sample(ctx, req, onDelta)
}

func (b *blockingProvider) Model(ctx context.Context, id string) (model.ModelInfo, error) {
	return b.inner.Model(ctx, id)
}

func newBlocking(inner *agenttest.ScriptedProvider) *blockingProvider {
	return &blockingProvider{inner: inner, entered: make(chan struct{}), release: make(chan struct{})}
}

// §6.6: steer delivered at the safe boundary after tool results; stale
// activation -> target_changed; duplicate request_id -> same receipt.
func TestMailboxSteerAndDedupe(t *testing.T) {
	call := model.ToolCall{ID: "c1", Name: "echo", Args: json.RawMessage(`{}`), ArgsOK: true}
	// Third response: the queued "note" message continues the loop at the
	// would-be stop (one input per continuation, §6.6).
	inner := agenttest.NewScripted(agenttest.CallsResponse(call), agenttest.TextResponse("steered done"), agenttest.TextResponse("after note"))
	prov := newBlocking(inner)
	s := openTest(t, prov, tracemem.New(), nil)

	inf, err := s.Spawn(context.Background(), host.SpawnSpec{ExecSpec: host.ExecSpec{Name: "solo", Task: "start"}})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-prov.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("provider never entered")
	}
	st, err := s.Status(context.Background(), inf.ID)
	if err != nil || st.Activation == "" {
		t.Fatalf("status = %+v err=%v", st, err)
	}
	// Stale activation.
	stale, _ := s.SendInput(context.Background(), host.SendRequest{
		Target: inf.ID, Mode: host.ModeSteer, Text: "wrong turn", ExpectedActivation: "bogus-node"})
	if stale.State != host.ReceiptRejected || stale.Code != "target_changed" {
		t.Errorf("stale steer receipt = %+v", stale)
	}
	// Live steer.
	live, err := s.SendInput(context.Background(), host.SendRequest{
		Target: inf.ID, Mode: host.ModeSteer, Text: "steer now", ExpectedActivation: st.Activation})
	if err != nil || live.State != host.ReceiptAccepted {
		t.Fatalf("live steer = %+v err=%v", live, err)
	}
	// Duplicate request IDs dedupe; conflicting body rejects.
	r1, _ := s.SendInput(context.Background(), host.SendRequest{Target: inf.ID, Mode: host.ModeMessage, Text: "note", RequestID: "req-1"})
	r2, _ := s.SendInput(context.Background(), host.SendRequest{Target: inf.ID, Mode: host.ModeMessage, Text: "note", RequestID: "req-1"})
	if r1.InputID == "" || r1.InputID != r2.InputID {
		t.Errorf("dedupe receipts differ: %+v vs %+v", r1, r2)
	}
	r3, _ := s.SendInput(context.Background(), host.SendRequest{Target: inf.ID, Mode: host.ModeMessage, Text: "different", RequestID: "req-1"})
	if r3.State != host.ReceiptRejected || r3.Code != "duplicate_conflict" {
		t.Errorf("conflict receipt = %+v", r3)
	}
	close(prov.release)
	res, err := s.Wait(context.Background(), []string{inf.ID}, 15*time.Second)
	if err != nil || len(res) != 1 || res[0].Timeout {
		t.Fatalf("Wait = %+v err=%v", res, err)
	}
	if res[0].Info.State != host.HandleIdle || res[0].Info.LastResult == nil || res[0].Info.LastResult.Status != "done" {
		t.Fatalf("final info = %+v", res[0].Info)
	}
	// Second request: steer item lands after the tool result (§6.6).
	if len(inner.Requests) < 2 {
		t.Fatalf("requests = %d", len(inner.Requests))
	}
	items := inner.Requests[1].Items
	toolResultAt, steerAt := -1, -1
	for i, it := range items {
		if it.Kind == model.KindToolResult {
			toolResultAt = i
		}
		if it.Text == "steer now" {
			steerAt = i
		}
	}
	if toolResultAt < 0 || steerAt <= toolResultAt {
		t.Errorf("steer not after tool results: toolResult=%d steer=%d items=%+v", toolResultAt, steerAt, items)
	}
	if rc, ok := s.InputReceipt(live.InputID); !ok || rc.State != host.ReceiptDelivered {
		t.Errorf("steer receipt after run = %+v ok=%v", rc, ok)
	}
}

// §6.5/§6.6: message never wakes an idle handle; follow_up does.
func TestMessageNoWakeFollowUpWakes(t *testing.T) {
	inner := agenttest.NewScripted(agenttest.TextResponse("first"))
	s := openTest(t, inner, tracemem.New(), nil)
	inf, err := s.Spawn(context.Background(), host.SpawnSpec{ExecSpec: host.ExecSpec{Name: "solo", Task: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Wait(context.Background(), []string{inf.ID}, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	before := len(inner.Requests)
	if _, err := s.SendInput(context.Background(), host.SendRequest{Target: inf.ID, Mode: host.ModeMessage, Text: "quiet note"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	st, _ := s.Status(context.Background(), inf.ID)
	if st.State != host.HandleIdle || len(inner.Requests) != before {
		t.Fatalf("message woke the handle: state=%s requests=%d (was %d)", st.State, len(inner.Requests), before)
	}
	// Two samples: the queued message is delivered first, then the
	// follow_up at the would-be stop (one input per continuation).
	inner.Push(agenttest.TextResponse("woken1"))
	inner.Push(agenttest.TextResponse("woken2"))
	if _, err := s.SendInput(context.Background(), host.SendRequest{Target: inf.ID, Mode: host.ModeFollowUp, Text: "next task"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Wait(context.Background(), []string{inf.ID}, 15*time.Second)
	if err != nil || res[0].Timeout {
		t.Fatalf("Wait after follow_up = %+v err=%v", res, err)
	}
	if len(inner.Requests) != before+2 {
		t.Fatalf("follow_up did not wake: requests=%d want %d", len(inner.Requests), before+2)
	}
	joined := ""
	for _, it := range s.ThreadItems("handle:" + inf.ID) {
		joined += it.Text + "|"
	}
	// ThreadItems takes a thread name, not a key; check via last request instead.
	last := inner.Requests[len(inner.Requests)-1]
	joined = ""
	for _, it := range last.Items {
		joined += it.Text + "|"
	}
	if !strings.Contains(joined, "next task") {
		t.Errorf("follow_up text not in woken request: %s", joined)
	}
	if res[0].Info.LastResult == nil || res[0].Info.LastResult.Text != "woken2" {
		t.Errorf("last result = %+v", res[0].Info.LastResult)
	}
}

// §6.5: interrupt settles the activation, then the handle is idle.
func TestInterruptSettlesToIdle(t *testing.T) {
	inner := agenttest.NewScripted(agenttest.TextResponse("never seen"))
	prov := newBlocking(inner)
	s := openTest(t, prov, tracemem.New(), nil)
	inf, err := s.Spawn(context.Background(), host.SpawnSpec{ExecSpec: host.ExecSpec{Name: "solo", Task: "hang"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-prov.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("never entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got, err := s.Interrupt(ctx, inf.ID)
	if err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if got.State != host.HandleIdle {
		t.Fatalf("state = %s", got.State)
	}
	if got.LastResult == nil || got.LastResult.Status != "cancelled" {
		t.Fatalf("last result = %+v", got.LastResult)
	}
}

// §7.2: slow subscriber is disconnected; replay from cursor has no duplicates.
func TestEventsOverflowAndReplay(t *testing.T) {
	sink := tracemem.New()
	prov := agenttest.NewScripted(agenttest.TextResponse("e1"))
	s := openTest(t, prov, sink, func(c *config.SessionConfig) { c.Events.SubscriberEvents = 2 })

	slow := s.Events(context.Background(), host.EventOptions{})
	if _, err := s.RunAgent(context.Background(), "solo", "hi"); err != nil {
		t.Fatal(err)
	}
	// Give the poller time to overflow the never-read queue, then drain.
	time.Sleep(300 * time.Millisecond)
	// The slow (never-read) subscriber must be disconnected: its channel closes.
	closed := make(chan []host.Envelope, 1)
	go func() {
		var got []host.Envelope
		for env := range slow {
			got = append(got, env)
		}
		closed <- got
	}()
	select {
	case got := <-closed:
		if len(got) > 2 {
			t.Errorf("slow subscriber got %d envelopes before disconnect (queue=2)", len(got))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("slow subscriber was not disconnected")
	}

	// Replay with a normal-sized queue (config changes apply to later
	// subscriptions, §3) from 0, then resume after a mid cursor.
	cfg2 := s.Config()
	cfg2.Events.SubscriberEvents = 256
	s.SetConfig(cfg2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := s.Events(ctx, host.EventOptions{After: 0})
	var cursors []int64
	deadline := time.After(5 * time.Second)
loop:
	for {
		select {
		case env, ok := <-first:
			if !ok {
				break loop
			}
			if env.Cursor > 0 {
				cursors = append(cursors, env.Cursor)
			}
			if env.Type == "run.end" {
				break loop
			}
		case <-deadline:
			t.Fatal("replay never reached run.end")
		}
	}
	if len(cursors) < 4 {
		t.Fatalf("cursors = %v", cursors)
	}
	mid := cursors[2]
	resumed := s.Events(ctx, host.EventOptions{After: mid})
	select {
	case env := <-resumed:
		if env.Cursor <= mid {
			t.Errorf("resumed from %d replayed cursor %d (duplicate)", mid, env.Cursor)
		}
		if env.Cursor != cursors[3] {
			t.Errorf("resumed first cursor = %d, want %d", env.Cursor, cursors[3])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no resumed event")
	}
}

// §7.2: projections rebuild from the trace in a fresh host.
func TestRebuildProjections(t *testing.T) {
	sink := tracemem.New()
	prov := agenttest.NewScripted(agenttest.TextResponse("kept answer"))
	root := testRoot(t)
	cfg := config.Defaults()
	opts := host.Options{ID: "resume-session", Config: cfg, View: echoView(),
		Provider: prov, Defs: openDefs(t, root), Trace: sink}
	s1, err := host.Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RunAgent(context.Background(), "solo", "kept question"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.SendInput(context.Background(), host.SendRequest{Target: "", Mode: host.ModeMessage, Text: "pending note", RequestID: "p-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := host.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if err := s2.Rebuild(context.Background(), ""); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	joined := ""
	for _, it := range s2.ThreadItems("") {
		joined += it.Text + "|"
	}
	if !strings.Contains(joined, "kept question") || !strings.Contains(joined, "kept answer") {
		t.Errorf("rebuilt thread = %s", joined)
	}
	pending := s2.Inputs("")
	if len(pending) != 1 || pending[0].RequestID != "p-1" || pending[0].State != host.ReceiptAccepted {
		t.Errorf("rebuilt pending inputs = %+v", pending)
	}
}

// WithOutput imposes a structured contract on a text-completion
// definition for one run: a plain-text reply no longer finishes
// the run, the schema-validated output lands in Result.Output,
// and the next run without the option is text again.
func TestWithOutputForcesStructuredCompletion(t *testing.T) {
	sink := tracemem.New()
	schema := json.RawMessage(`{"type":"object","required":["verdict"],"properties":{"verdict":{"type":"string"}},"additionalProperties":false}`)
	prov := agenttest.NewScripted(
		// Bad output first: the loop rejects it back to the model.
		agenttest.CallsResponse(model.ToolCall{
			ID: "c1", Name: "complete", ArgsOK: true,
			Args: json.RawMessage(`{"summary":"trying","output":{"nope":1}}`),
		}),
		agenttest.CallsResponse(model.ToolCall{
			ID: "c2", Name: "complete", ArgsOK: true,
			Args: json.RawMessage(`{"summary":"all done","output":{"verdict":"ok"}}`),
		}),
		agenttest.TextResponse("plain again"),
	)
	s := openTest(t, prov, sink, nil)

	res, err := s.RunAgent(context.Background(), "solo", "judge it", host.WithOutput(schema))
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if res.Status != "done" || res.Text != "all done" || string(res.Output) != `{"verdict":"ok"}` {
		t.Fatalf("res = %+v output=%s", res, res.Output)
	}
	if res.DefName != "solo" {
		t.Errorf("DefName = %q, want the definition's own name", res.DefName)
	}

	res, err = s.RunAgent(context.Background(), "solo", "and now?")
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if res.Status != "done" || res.Text != "plain again" || len(res.Output) != 0 {
		t.Fatalf("text run after contract = %+v output=%s", res, res.Output)
	}

	if _, err := s.RunAgent(context.Background(), "solo", "bad", host.WithOutput(json.RawMessage(`{`))); err == nil {
		t.Errorf("invalid schema JSON accepted")
	}
}

// loop.progressive_tools reaches the loop through the resolved
// definition, and a narrowed view keeps its groups for the catalogue
// (§4.3).
func TestProgressiveToolsFromDefinition(t *testing.T) {
	root := testRoot(t)
	view := echoView()
	view.WithGroups(model.ToolGroup{Name: "util", Description: "Echo things back."})
	defs := view.Tools()
	for i := range defs {
		defs[i].Group = "util"
	}
	grouped := host.NewMapView(defs, map[string]host.ToolFunc{
		"echo": func(_ context.Context, args json.RawMessage) (model.ToolResult, error) {
			return model.ToolResult{Text: "echo"}, nil
		},
	}).WithGroups(view.Groups()...)
	prov := agenttest.NewScripted(agenttest.TextResponse("lazy"), agenttest.TextResponse("solo"))
	s, err := host.Open(context.Background(), host.Options{
		Config: config.Defaults(), View: grouped, Provider: prov, Defs: openDefs(t, root), Trace: tracemem.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, agent := range []string{"lazy", "solo"} {
		if _, err := s.RunAgent(context.Background(), agent, "go"); err != nil {
			t.Fatal(err)
		}
	}
	names := func(req model.Request) string {
		var out []string
		for _, tl := range req.Tools {
			out = append(out, tl.Name)
		}
		return strings.Join(out, ",")
	}
	lazy, solo := prov.Requests[0], prov.Requests[1]
	if got := names(lazy); strings.Contains(got, "echo") || !strings.Contains(got, "tools_load") {
		t.Errorf("lazy tools = %s", got)
	}
	if !strings.Contains(lazy.Items[0].Text, "- `util` (1 tool, on demand): Echo things back.") {
		t.Errorf("lazy system prompt:\n%s", lazy.Items[0].Text)
	}
	if got := names(solo); !strings.Contains(got, "echo") || strings.Contains(got, "tools_load") {
		t.Errorf("solo tools = %s", got)
	}
	if !strings.Contains(solo.Items[0].Text, "- `util` (2 tools): Echo things back.") {
		t.Errorf("solo system prompt:\n%s", solo.Items[0].Text)
	}
}

// §5.7 runway notices: a child sees the shared run-tree budget (the
// parent's steps count), each node gets each notice once, the notice is
// traced, and an empty loop.budget_notice_at disables them.
func TestRunwayNoticeSharedBudget(t *testing.T) {
	script := func() *agenttest.ScriptedProvider {
		prov := agenttest.NewScripted()
		for i := range 5 { // parent steps 1-5
			prov.Push(agenttest.CallsResponse(model.ToolCall{ID: fmt.Sprintf("e%d", i), Name: "echo",
				Args: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)), ArgsOK: true}))
		}
		prov.Push(agenttest.CallsResponse(agentCall("a1", "child", "finish up"))) // parent step 6
		prov.Push(agenttest.TextResponse("child done"))                           // child step 1 (run step 7)
		prov.Push(agenttest.TextResponse("parent done"))                          // parent step 7 (run step 8)
		return prov
	}
	budgetTexts := func(req model.Request) []string {
		var out []string
		for _, it := range req.Items {
			if strings.Contains(it.Text, `kind="budget"`) {
				out = append(out, it.Text)
			}
		}
		return out
	}

	prov := script()
	sink := tracemem.New()
	s := openTest(t, prov, sink, func(c *config.SessionConfig) { c.Limits.Steps = 8 })
	res, err := s.RunAgent(context.Background(), "parent", "go")
	if err != nil || res.Status != "done" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(prov.Requests) != 8 {
		t.Fatalf("requests = %d", len(prov.Requests))
	}
	// Request 7 is the child's first: 6/8 run steps used, by the parent.
	child := budgetTexts(prov.Requests[6])
	if len(child) != 1 || !strings.Contains(child[0], "6/8 steps across the run") ||
		!strings.Contains(child[0], "0/80 of your own steps") {
		t.Errorf("child notices = %q", child)
	}
	// Request 8 is the parent's last: its own notice at 7/8, given once.
	parent := budgetTexts(prov.Requests[7])
	if len(parent) != 1 || !strings.Contains(parent[0], "Budget check: 87% of the run step limit") {
		t.Errorf("parent notices = %q", parent)
	}
	traced := 0
	for _, e := range readAll(t, sink) {
		if e.Type == "budget.notice" {
			traced++
		}
	}
	if traced != 2 {
		t.Errorf("budget.notice events = %d, want 2", traced)
	}

	prov = script()
	s = openTest(t, prov, tracemem.New(), func(c *config.SessionConfig) {
		c.Limits.Steps = 8
		c.Loop.BudgetNoticeAt = []float64{}
	})
	if res, err := s.RunAgent(context.Background(), "parent", "go"); err != nil || res.Status != "done" {
		t.Fatalf("disabled: res=%+v err=%v", res, err)
	}
	for i, req := range prov.Requests {
		if n := budgetTexts(req); len(n) != 0 {
			t.Errorf("disabled, but request %d has notices %q", i, n)
		}
	}
}
