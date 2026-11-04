package configyaml_test

import (
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/adapters/configyaml"
	"github.com/craggy-io/agent-sdk/config"
)

// The design example parses to the design's values, incl. 32KiB and 30m.
func TestParseDesignExample(t *testing.T) {
	cfg, err := configyaml.Parse([]byte(`
profiles:
  default:
    model: anthropic/claude-sonnet-5.5
    fallbacks: [openai/gpt-5.2]
    reasoning: {effort: medium}
    max_output_tokens: 16000
    parallel_tool_calls: true
    provider: {require_parameters: true, data_collection: deny}
    cache: auto
    idle_timeout: 45s
limits:
  usd: 5.00
  wall_clock: 30m
  steps: 400
loop:
  tool_result_bytes: 32KiB
  compact_at: 0.8
events:
  ring_bytes: 8MiB
`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiles["default"]
	if p.IdleTimeout != 45*time.Second || p.Reasoning.Effort != "medium" {
		t.Errorf("profile=%+v", p)
	}
	if cfg.Limits.WallClock != 30*time.Minute || cfg.Loop.ToolResultBytes != 32768 {
		t.Errorf("limits/loop=%+v %+v", cfg.Limits, cfg.Loop)
	}
	if cfg.Limits.Depth != 6 || cfg.Input.MaxPending != 128 || cfg.Events.RingBytes != 8*1024*1024 {
		t.Errorf("overlay lost defaults: %+v", cfg)
	}
	if cfg.Profiles["fast"].Model != "openai/gpt-5.2-mini" {
		t.Errorf("fast profile lost: %+v", cfg.Profiles)
	}
	if _, err := configyaml.Parse([]byte("limits: {wall_clock: soon}")); err == nil {
		t.Errorf("bad duration accepted")
	}
	if _, err := configyaml.Parse([]byte("bogus_section: {}")); err == nil {
		t.Errorf("unknown field accepted (KnownFields)")
	}
}

// loop.budget_notice_at: default, list, scalar 0 / [] to disable, and
// rejection of values outside (0, 1).
func TestParseBudgetNoticeAt(t *testing.T) {
	if got := config.Defaults().Loop.BudgetNoticeAt; len(got) != 2 || got[0] != 0.75 || got[1] != 0.9 {
		t.Fatalf("default = %v", got)
	}
	for _, c := range []struct {
		yaml string
		want []float64
	}{
		{"loop: {max_steps: 10}", []float64{0.75, 0.9}},
		{"loop: {budget_notice_at: [0.9, 0.5, 0.9]}", []float64{0.5, 0.9}},
		{"loop: {budget_notice_at: 0.8}", []float64{0.8}},
		{"loop: {budget_notice_at: 0}", []float64{}},
		{"loop: {budget_notice_at: []}", []float64{}},
		{"loop: {budget_notice_at: [0]}", []float64{}},
	} {
		cfg, err := configyaml.Parse([]byte(c.yaml))
		if err != nil {
			t.Fatalf("%s: %v", c.yaml, err)
		}
		got := cfg.Loop.BudgetNoticeAt
		if got == nil || len(got) != len(c.want) {
			t.Fatalf("%s: got %#v, want %v", c.yaml, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v, want %v", c.yaml, got, c.want)
			}
		}
	}
	for _, bad := range []string{
		"loop: {budget_notice_at: [1.0]}", "loop: {budget_notice_at: [-0.1]}",
		"loop: {budget_notice_at: [0, 0.5]}", "loop: {budget_notice_at: soon}",
	} {
		if _, err := configyaml.Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}
