package flow_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/flow"
)

const triageReviews = `load("@craggy/flow.star", "flow")

pipeline(
    title = "Reviews",
    inputs = {"reviews": flow.input("shop.reviews", "id")},
    requires = ["core.models.decide@1"],
)

STEPS = [
    flow.read(id = "reviews", input = "reviews"),
    flow.for_each(id = "each", over = "reviews", item = "review", steps = [
        flow.decide(id = "team", question = "Which team should answer {review.id}?", input = "review", out = "team", decision = "choice", options = "billing, support", explain = True),
        flow.decide(id = "stars", question = "How happy is the customer?", input = "review", out = "stars", decision = "score", min = 1, max = 5),
    ]),
]

def run(ctx):
    return flow.run(ctx, STEPS)
`

// A decide step asks core.models.decide about its input with its question
// filled in, and outputs the decision, its confidence and its reason.
func TestDecideStep(t *testing.T) {
	b := flow.Bundle{flow.FileName: triageReviews}
	if probs := flow.Validate(b, flow.Env{SourceOnly: true}); len(probs) > 0 {
		t.Fatalf("problems: %v", probs)
	}
	inputs := map[string]any{"reviews": []any{map[string]any{"id": "r1", "text": "I was charged twice", "_keys": []any{"r1"}}}}
	calls, _, _ := run(t, b, inputs, func(c called) any {
		if c.Args["kind"] == "score" {
			return map[string]any{"value": 2, "confidence": 0.6}
		}
		return map[string]any{"value": "billing", "confidence": 0.9, "reason": "a charge"}
	})
	if len(calls) != 2 || calls[0].Ref != "core.models.decide@1" {
		t.Fatalf("calls %+v", calls)
	}
	team := calls[0].Args
	if team["question"] != "Which team should answer r1?" || team["explain"] != true ||
		!reflect.DeepEqual(team["options"], []any{map[string]any{"value": "billing"}, map[string]any{"value": "support"}}) ||
		!strings.Contains(team["context"].(string), "charged twice") {
		t.Fatalf("team asked %v", team)
	}
	if stars := calls[1].Args; stars["min"] != float64(1) || stars["max"] != float64(5) || stars["options"] != nil {
		t.Fatalf("stars asked %v", stars)
	}
}

func TestDecideStepProblems(t *testing.T) {
	src := strings.Replace(flow.Template(flow.Header{Title: "Bad", Requires: []string{"core.models.decide@1"}}), "STEPS = []",
		`STEPS = [
    flow.code(id = "x", fn = f, out = "doc"),
    flow.decide(id = "a", question = "Which?", input = "doc", out = "a", decision = "choice", options = "one"),
    flow.decide(id = "b", question = "Spam?", input = "doc", out = "b", options = "x, y"),
    flow.decide(id = "c", question = "How much?", input = "doc", out = "c", decision = "score", min = 5, max = 1),
    flow.decide(id = "d", question = "Which?", input = "doc", out = "d", decision = "guess"),
]

def f(env):
    return "text"
`, 1)
	got := problems(flow.Validate(flow.Bundle{flow.FileName: src}, flow.Env{SourceOnly: true}))
	for _, want := range []string{"step a: a choice decision needs at least two options", "step b: a bool decision takes no options",
		"step c: the lowest score 5 isn't below the highest 1", `step d: decision is "guess"`} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
}
