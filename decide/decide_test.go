package decide_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/decide"
	"github.com/craggy-io/agent-sdk/model"
)

func call(args string) model.Response {
	r := agenttest.CallsResponse(model.ToolCall{ID: "c1", Name: decide.ToolName, Args: json.RawMessage(args), ArgsOK: true})
	r.Usage = model.Usage{PromptTokens: 10, CompletionTokens: 2}
	return r
}

func f(v float64) *float64 { return &v }

func TestKinds(t *testing.T) {
	cases := []struct {
		name string
		req  decide.Request
		args string
		want string
	}{
		{"bool", decide.Request{Kind: decide.Bool, Question: "Spam?"}, `{"value":true,"confidence":0.9}`, `true`},
		{"bool as words", decide.Request{Kind: decide.Bool, Question: "Spam?"}, `{"value":"No","confidence":0.9}`, `false`},
		{"choice folds case", decide.Request{Kind: "Choice", Question: "Team?", Options: decide.Values("Billing", "Support")}, `{"value":"billing","confidence":1}`, `"Billing"`},
		{"choices in option order", decide.Request{Kind: decide.Choices, Question: "Tags?", Options: decide.Values("a", "b", "c")}, `{"value":["c","a"],"confidence":0.5}`, `["a","c"]`},
		{"choices double-encoded", decide.Request{Kind: decide.Choices, Question: "Tags?", Options: decide.Values("a", "b")}, `{"value":"[\"b\"]","confidence":0.5}`, `["b"]`},
		{"score default range", decide.Request{Kind: decide.Score, Question: "Relevant?"}, `{"value":0.25,"confidence":0.7}`, `0.25`},
		{"score as string", decide.Request{Kind: decide.Score, Question: "Stars?", Min: f(1), Max: f(5), Integer: true}, `{"value":"4","confidence":0.7}`, `4`},
		{"rank", decide.Request{Kind: decide.Rank, Question: "Best?", Options: decide.Values("x", "y")}, `{"value":["y","x"],"confidence":0.6}`, `["y","x"]`},
		{"extract", decide.Request{Kind: decide.Extract, Question: "Who?", Schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)}, `{"value":{"name":"Ada"},"confidence":0.8}`, `{"name":"Ada"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := agenttest.NewScripted(call(c.args))
			d, err := decide.Decider{Provider: p, Model: "m"}.Decide(context.Background(), c.req)
			if err != nil {
				t.Fatal(err)
			}
			if string(d.Value) != c.want {
				t.Errorf("value %s, want %s", d.Value, c.want)
			}
			req := p.Requests[0]
			if req.ToolChoice != "" || len(req.Tools) != 1 || req.Model != "m" {
				t.Errorf("request %+v", req)
			}
			if !json.Valid(req.Tools[0].Schema) {
				t.Errorf("tool schema isn't JSON: %s", req.Tools[0].Schema)
			}
		})
	}
}

func TestRetriesAnAnswerThatDoesntFit(t *testing.T) {
	p := agenttest.NewScripted(call(`{"value":"maybe","confidence":0.9}`), call(`{"reason":"it says so","value":"yes","confidence":1.5}`))
	d, err := decide.Decider{Provider: p, Model: "m"}.Decide(context.Background(),
		decide.Request{Kind: decide.Choice, Question: "Which?", Options: decide.Values("yes", "no"), Explain: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Choice() != "yes" || d.Attempts != 2 || d.Confidence != 1 || d.Reason != "it says so" || d.Usage.PromptTokens != 20 {
		t.Errorf("decision %+v", d)
	}
	if got := p.Requests[1].Items; len(got) != 3 || !strings.Contains(got[2].Text, `"maybe" isn't one of the options`) {
		t.Errorf("retry items %+v", got)
	}
	if !strings.Contains(string(p.Requests[0].Tools[0].Schema), `"required":["reason","value","confidence"]`) {
		t.Errorf("schema %s", p.Requests[0].Tools[0].Schema)
	}
}

func TestUndecided(t *testing.T) {
	p := agenttest.NewScripted(agenttest.TextResponse("I think so"), call(`{"value":7,"confidence":1}`))
	_, err := decide.Decider{Provider: p, Model: "m"}.Decide(context.Background(), decide.Request{Kind: decide.Score, Question: "?", Min: f(0), Max: f(5)})
	if !errors.Is(err, decide.ErrUndecided) || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err %v", err)
	}
}

func TestReadsArgumentsWrittenAsText(t *testing.T) {
	p := agenttest.NewScripted(agenttest.TextResponse("Here: {\"value\": false, \"confidence\": 0.4}"))
	d, err := decide.Decider{Provider: p, Model: "m"}.Decide(context.Background(), decide.Request{Kind: decide.Bool, Question: "?"})
	if err != nil || d.Bool() || d.Confidence != 0.4 {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestInvalid(t *testing.T) {
	for _, req := range []decide.Request{
		{Kind: decide.Bool},
		{Kind: "guess", Question: "?"},
		{Kind: decide.Choice, Question: "?", Options: decide.Values("one")},
		{Kind: decide.Choice, Question: "?", Options: decide.Values("a", "A")},
		{Kind: decide.Bool, Question: "?", Options: decide.Values("a", "b")},
		{Kind: decide.Score, Question: "?", Min: f(1)},
		{Kind: decide.Score, Question: "?", Min: f(2), Max: f(1)},
		{Kind: decide.Choices, Question: "?", Options: decide.Values("a"), AtLeast: 2},
		{Kind: decide.Extract, Question: "?", Schema: json.RawMessage(`{"type":"string"}`)},
	} {
		_, err := decide.Decider{Provider: agenttest.NewScripted(), Model: "m"}.Decide(context.Background(), req)
		if !errors.Is(err, decide.ErrInvalid) {
			t.Errorf("%+v: %v", req, err)
		}
	}
}

func TestProviderErrorPassesThrough(t *testing.T) {
	p := agenttest.NewScripted()
	want := &model.Error{Status: 429, Transient: true}
	p.PushError(want)
	_, err := decide.Decider{Provider: p, Model: "m"}.Decide(context.Background(), decide.Request{Kind: decide.Bool, Question: "?"})
	if !errors.Is(err, want) {
		t.Fatalf("err %v", err)
	}
}
