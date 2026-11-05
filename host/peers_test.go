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
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
)

// routedProvider answers each agent from its own script, so concurrent
// agents get deterministic replies. An agent is known by its system
// prompt ("You are <name>."). A script step sees the request, so it can
// address agents whose IDs are only known at run time.
type routedProvider struct {
	mu       sync.Mutex
	scripts  map[string][]func(model.Request) model.Response
	requests map[string][]model.Request
}

func newRouted(scripts map[string][]func(model.Request) model.Response) *routedProvider {
	return &routedProvider{scripts: scripts, requests: map[string][]model.Request{}}
}

func agentOf(req model.Request) string {
	if len(req.Items) == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(req.Items[0].Text, "You are "), ".")
}

func (p *routedProvider) Sample(_ context.Context, req model.Request, _ func(model.Delta)) (model.Response, error) {
	p.mu.Lock()
	name := agentOf(req)
	p.requests[name] = append(p.requests[name], req)
	q := p.scripts[name]
	if len(q) == 0 {
		p.mu.Unlock()
		return model.Response{}, fmt.Errorf("routed: no script left for %q", name)
	}
	step := q[0]
	p.scripts[name] = q[1:]
	p.mu.Unlock()
	return step(req), nil
}

func (p *routedProvider) Model(_ context.Context, id string) (model.ModelInfo, error) {
	return model.ModelInfo{ID: id}, nil
}

func (p *routedProvider) sent(name string) []model.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]model.Request(nil), p.requests[name]...)
}

func text(s string) func(model.Request) model.Response {
	return func(model.Request) model.Response { return agenttest.TextResponse(s) }
}

func builtinCall(id, name string, args any) func(model.Request) model.Response {
	return func(model.Request) model.Response { return agenttest.CallsResponse(toolCall(id, name, args)) }
}

func toolCall(id, name string, args any) model.ToolCall {
	b, _ := json.Marshal(args)
	return model.ToolCall{ID: id, Name: name, Args: b, ArgsOK: true}
}

// lastToolResult is the newest tool result an agent's request carries.
func lastToolResult(req model.Request) string {
	for i := len(req.Items) - 1; i >= 0; i-- {
		if req.Items[i].Kind == model.KindToolResult {
			return req.Items[i].Text
		}
	}
	return ""
}

func joinedText(items []model.Item) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString(it.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func toolNames(req model.Request) map[string]bool {
	out := map[string]bool{}
	for _, t := range req.Tools {
		out[t.Name] = true
	}
	return out
}

// §6.7: every agent gets the messaging built-ins; only delegators get
// delegation ones.
func TestMessagingBuiltinsForEveryAgent(t *testing.T) {
	prov := newRouted(map[string][]func(model.Request) model.Response{
		"solo": {text("hi")}, "parent": {text("hi")},
	})
	s := openTest(t, prov, tracemem.New(), nil)
	for _, name := range []string{"solo", "parent"} {
		if _, err := s.RunAgent(context.Background(), name, "go", host.WithThread(name)); err != nil {
			t.Fatal(err)
		}
	}
	solo, parent := toolNames(prov.sent("solo")[0]), toolNames(prov.sent("parent")[0])
	for _, n := range []string{"agent_list", "agent_send", "agent_status", "agent_wait"} {
		if !solo[n] || !parent[n] {
			t.Errorf("%s not offered to every agent: solo=%v parent=%v", n, solo[n], parent[n])
		}
	}
	for _, n := range []string{"agent", "agent_spawn", "agent_close"} {
		if solo[n] {
			t.Errorf("%s offered to an agent with no delegates", n)
		}
		if !parent[n] {
			t.Errorf("%s not offered to a delegator", n)
		}
	}
}

// §6.7: a child discovers and messages its parent (the thread agent);
// the parent, having finished its text, waits for the running child,
// receives the message with a reply_to address, and does not end before
// the child settles.
func TestChildMessagesWaitingParent(t *testing.T) {
	var childID string
	prov := newRouted(map[string][]func(model.Request) model.Response{
		"parent": {
			builtinCall("p1", "agent_spawn", map[string]string{"name": "child", "task": "look into it"}),
			text("parent waiting"),
			text("parent final"),
			text("parent final"),
		},
		"child": {
			builtinCall("c1", "agent_list", map[string]any{}),
			func(req model.Request) model.Response {
				var peers []host.Peer
				_ = json.Unmarshal([]byte(lastToolResult(req)), &peers)
				to := ""
				for _, p := range peers {
					if p.You {
						childID = p.ID
					} else if p.Kind == "thread" {
						to = p.ID
					}
				}
				return agenttest.CallsResponse(toolCall("c2", "agent_send", map[string]string{"to": to, "text": "which build?"}))
			},
			text("child done"),
		},
	})
	s := openTest(t, prov, tracemem.New(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := s.RunAgent(ctx, "parent", "investigate")
	if err != nil || !strings.HasPrefix(res.Text, "parent final") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if childID == "" {
		t.Fatal("child did not find itself in agent_list")
	}
	thread := joinedText(s.ThreadItems(""))
	want := fmt.Sprintf(`kind="agent_message" from="agent:child/`)
	if !strings.Contains(thread, want) || !strings.Contains(thread, `reply_to="`+childID+`"`) || !strings.Contains(thread, "which build?") {
		t.Errorf("parent thread lacks the child's message with reply_to %s:\n%s", childID, thread)
	}
	// The parent never ends before the child it started.
	for _, h := range s.ListAgents(context.Background()) {
		if h.State == host.HandleRunning {
			t.Errorf("handle %s (%s) still running after the parent returned", h.ID, h.Def)
		}
	}
}

// §6.7: a delegate that finished in an earlier turn is woken by a later
// turn's message, answers in a fresh run, and goes idle again; the waking
// agent waits for it and receives its answer.
func TestMessageWakesFinishedAgent(t *testing.T) {
	prov := newRouted(map[string][]func(model.Request) model.Response{
		"parent": {
			// Turn 1: blocking delegation.
			builtinCall("p1", "agent", map[string]string{"name": "child", "task": "first job"}),
			text("turn1 done"),
			// Turn 2: find the finished child and ask it something.
			builtinCall("p2", "agent_list", map[string]any{}),
			func(req model.Request) model.Response {
				var peers []host.Peer
				_ = json.Unmarshal([]byte(lastToolResult(req)), &peers)
				to := ""
				for _, p := range peers {
					if p.Def == "child" {
						to = p.ID
					}
				}
				return agenttest.CallsResponse(toolCall("p3", "agent_send", map[string]string{"to": to, "text": "follow-up question"}))
			},
			text("turn2 waiting"),
			text("turn2 done"),
		},
		"child": {text("first answer"), text("second answer")},
	})
	sink := tracemem.New()
	s := openTest(t, prov, sink, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if res, err := s.RunAgent(ctx, "parent", "turn one"); err != nil || res.Text != "turn1 done" {
		t.Fatalf("turn 1: res=%+v err=%v", res, err)
	}
	handles := s.ListAgents(context.Background())
	if len(handles) != 1 || handles[0].State != host.HandleIdle {
		t.Fatalf("after turn 1 handles = %+v, want one idle child", handles)
	}
	res, err := s.RunAgent(ctx, "parent", "turn two")
	if err != nil || res.Text != "turn2 done" {
		t.Fatalf("turn 2: res=%+v err=%v", res, err)
	}
	childReqs := prov.sent("child")
	if len(childReqs) != 2 {
		t.Fatalf("child sampled %d times, want 2 (first job, then woken)", len(childReqs))
	}
	woken := joinedText(childReqs[1].Items)
	if !strings.Contains(woken, "first answer") || !strings.Contains(woken, "follow-up question") || !strings.Contains(woken, `reply_to="primary"`) {
		t.Errorf("woken child lacks its history or the message:\n%s", woken)
	}
	thread := joinedText(s.ThreadItems(""))
	if !strings.Contains(thread, `kind="agent_settled"`) || !strings.Contains(thread, "second answer") {
		t.Errorf("parent did not receive the woken child's answer:\n%s", thread)
	}
	if st, _ := s.Status(context.Background(), handles[0].ID); st.State != host.HandleIdle {
		t.Errorf("child state after answering = %s, want idle", st.State)
	}
	wakeRun := false
	for _, e := range readAll(t, sink) {
		if e.Type == "run.start" && strings.Contains(string(e.Data), `"agent-wake"`) {
			wakeRun = true
		}
	}
	if !wakeRun {
		t.Error("the woken child did not get a fresh run after turn 1's run ended")
	}
}

// §6.7: agents see and message only their own conversation.
func TestMessagingConfinedToConversation(t *testing.T) {
	var foreign string
	prov := newRouted(map[string][]func(model.Request) model.Response{
		"parent": {
			builtinCall("p1", "agent", map[string]string{"name": "child", "task": "job"}),
			text("a done"),
		},
		"child": {text("child answer")},
		"solo": {
			builtinCall("s1", "agent_list", map[string]any{}),
			func(model.Request) model.Response {
				return agenttest.CallsResponse(toolCall("s2", "agent_send", map[string]string{"to": foreign, "text": "hello"}))
			},
			func(model.Request) model.Response {
				return agenttest.CallsResponse(toolCall("s3", "agent_send", map[string]string{"to": "a", "text": "hello"}))
			},
			text("b done"),
		},
	})
	s := openTest(t, prov, tracemem.New(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.RunAgent(host.WithConversation(ctx, "chat-a"), "parent", "go", host.WithThread("a")); err != nil {
		t.Fatal(err)
	}
	foreign = s.ListAgents(context.Background())[0].ID
	if _, err := s.RunAgent(host.WithConversation(ctx, "chat-b"), "solo", "go", host.WithThread("b")); err != nil {
		t.Fatal(err)
	}
	reqs := prov.sent("solo")
	if got := lastToolResult(reqs[1]); strings.Contains(got, foreign) || strings.Contains(got, `"id":"a"`) {
		t.Errorf("agent_list leaked another conversation's agents: %s", got)
	}
	for i, step := range []int{2, 3} {
		if got := lastToolResult(reqs[step]); !strings.Contains(got, "no agent") {
			t.Errorf("send %d across conversations not refused: %s", i, got)
		}
	}
	if n := len(prov.sent("child")); n != 1 {
		t.Errorf("foreign child sampled %d times, want 1 (never woken)", n)
	}
}

type hostKey struct{}

// §11: a spawned delegate's tool calls carry the host values (e.g. a host's lab
// scope) of the context that started it, though its activation runs
// detached from that context's cancellation.
func TestDelegateKeepsHostContextValues(t *testing.T) {
	var mu sync.Mutex
	var seen []any
	view := host.NewMapView(
		[]model.Tool{{Name: "echo", Effect: "read-only", Schema: json.RawMessage(`{"type":"object"}`)}},
		map[string]host.ToolFunc{"echo": func(ctx context.Context, _ json.RawMessage) (model.ToolResult, error) {
			mu.Lock()
			seen = append(seen, ctx.Value(hostKey{}))
			mu.Unlock()
			return model.ToolResult{Text: "ok"}, nil
		}})
	prov := newRouted(map[string][]func(model.Request) model.Response{
		"parent": {builtinCall("p1", "agent_spawn", map[string]string{"name": "child", "task": "probe"}), text("done")},
		"child":  {builtinCall("c1", "echo", map[string]any{}), text("probed")},
	})
	s, err := host.Open(context.Background(), host.Options{
		View: view, Provider: prov, Defs: openDefs(t, testRoot(t)), Trace: tracemem.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.WithValue(context.Background(), hostKey{}, "lab-1")
	if res, err := s.RunAgent(ctx, "parent", "go"); err != nil || res.Text != "done" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(seen) != 1 || seen[0] != "lab-1" {
		t.Fatalf("delegate tool saw host values %v, want [lab-1]", seen)
	}
}
