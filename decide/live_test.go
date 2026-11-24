package decide_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/craggy-io/agent-sdk/decide"
	"github.com/craggy-io/agent-sdk/openrouter"
	"github.com/craggy-io/agent-sdk/pricesheet"
)

// TestLive asks a real model for one decision of every kind. It runs only
// with a key and explicit opt-in:
//
//	OPENROUTER_API_KEY=... AGENT_SDK_LIVE=1 go test ./decide/
//
// AGENT_SDK_LIVE_MODEL names the model; the price sheet's low tier is
// asked otherwise.
func TestLive(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" || os.Getenv("AGENT_SDK_LIVE") != "1" {
		t.Skip("live test: set OPENROUTER_API_KEY and AGENT_SDK_LIVE=1 to run")
	}
	name := os.Getenv("AGENT_SDK_LIVE_MODEL")
	if name == "" {
		name = pricesheet.Builtin().Tier(pricesheet.Low).ID
	}
	d := decide.Decider{Provider: openrouter.New(key), Model: name}
	review := "Great blender, crushed ice in seconds. Buy cheap watches at www.example.com!!!"
	for _, c := range []struct {
		req   decide.Request
		check func(decide.Decision) bool
	}{
		{decide.Request{Kind: decide.Bool, Question: "Is 17 a prime number?"}, func(d decide.Decision) bool { return d.Bool() }},
		{decide.Request{Kind: decide.Choice, Question: "What does the review mainly advertise besides the blender?", Context: review,
			Options: decide.Values("watches", "shoes", "nothing")}, func(d decide.Decision) bool { return d.Choice() == "watches" }},
		{decide.Request{Kind: decide.Choices, Question: "Which are fruits?", Options: decide.Values("apple", "carrot", "banana")},
			func(d decide.Decision) bool { return len(d.Choices()) == 2 }},
		{decide.Request{Kind: decide.Score, Question: "How likely is this review spam?", Context: review, Explain: true},
			func(d decide.Decision) bool { return d.Score() >= 0 && d.Score() <= 1 && d.Reason != "" }},
		{decide.Request{Kind: decide.Rank, Question: "Order by size, largest first.", Options: decide.Values("mouse", "elephant", "dog")},
			func(d decide.Decision) bool { return d.Choices()[0] == "elephant" }},
		{decide.Request{Kind: decide.Extract, Question: "Who wrote it and when?", Context: "Written by Ada Lovelace in 1843.",
			Schema: json.RawMessage(`{"type":"object","properties":{"author":{"type":"string"},"year":{"type":"integer"}},"required":["author","year"]}`)},
			func(d decide.Decision) bool {
				var v struct{ Year int }
				return d.Into(&v) == nil && v.Year == 1843
			}},
	} {
		got, err := d.Decide(context.Background(), c.req)
		if err != nil {
			t.Errorf("%s: %v", c.req.Kind, err)
			continue
		}
		t.Logf("%s: %s (confidence %.2f, %d attempts, %s, $%.5f) %s", c.req.Kind, got.Value, got.Confidence, got.Attempts, got.Model, got.Usage.CostUSD, got.Reason)
		if !c.check(got) {
			t.Errorf("%s: unexpected decision %s", c.req.Kind, got.Value)
		}
	}
}
