package openrouter_test

import (
	"context"
	"os"
	"testing"

	"github.com/craggy-io/agent-sdk/openrouter"
)

// TestLiveSmoke is the env-gated live check (§8): it runs only with a real
// key and explicit opt-in, so CI and offline runs skip cleanly.
//
//	OPENROUTER_API_KEY=... AGENT_SDK_LIVE=1 go test ./src/openrouter/
func TestLiveSmoke(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" || os.Getenv("AGENT_SDK_LIVE") != "1" {
		t.Skip("live test: set OPENROUTER_API_KEY and AGENT_SDK_LIVE=1 to run")
	}
	c := openrouter.New(key)
	req := baseRequest()
	if m := os.Getenv("AGENT_SDK_LIVE_MODEL"); m != "" {
		req.Model = m
	}
	if dc := os.Getenv("AGENT_SDK_LIVE_DATA_COLLECTION"); dc != "" {
		req.Provider.DataCollection = dc
	}
	// Some models do not list parallel_tool_calls as supported; sending it
	// with require_parameters 404s. Opt in explicitly for those that do.
	req.ParallelToolCalls = os.Getenv("AGENT_SDK_LIVE_PARALLEL") == "1"
	req.Fallbacks = nil
	req.Tools = nil
	req.ToolChoice = "none"
	resp, err := c.Sample(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Item.Text == "" || resp.ServedModel == "" {
		t.Errorf("live response empty: %+v", resp)
	}
	if _, err := c.Model(context.Background(), resp.ServedModel); err != nil {
		t.Errorf("Model(%q): %v", resp.ServedModel, err)
	}
}
