package config_test

import (
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/model"
)

func ptr[T any](v T) *T { return &v }

// §3.1 verbatim values.
func TestDefaults(t *testing.T) {
	d := config.Defaults()
	def := d.Profiles["default"]
	if def.Model != "anthropic/claude-sonnet-5.5" || def.MaxOutputTokens != 16000 ||
		!def.ParallelToolCalls || def.Cache != "auto" || def.Reasoning.Effort != "medium" ||
		!def.Provider.RequireParameters || def.Provider.DataCollection != "deny" ||
		len(def.Fallbacks) != 1 || def.Fallbacks[0] != "openai/gpt-5.2" {
		t.Errorf("default profile=%+v", def)
	}
	if d.Profiles["fast"].Model != "openai/gpt-5.2-mini" || d.Profiles["deep"].Reasoning.Effort != "high" {
		t.Errorf("fast/deep=%+v %+v", d.Profiles["fast"], d.Profiles["deep"])
	}
	if d.Limits != (config.Limits{USD: 5, WallClock: 30 * time.Minute, Steps: 400, ToolCalls: 1000, Depth: 6, Concurrency: 8}) {
		t.Errorf("limits=%+v", d.Limits)
	}
	if d.Loop.MaxSteps != 80 || d.Loop.ToolResultBytes != 32768 || d.Loop.CompactAt != 0.8 ||
		d.Loop.KeepRecentTokens != 20000 || d.Loop.SummaryTokens != 4000 {
		t.Errorf("loop=%+v", d.Loop)
	}
	if d.Input.QueueMode != "one" || d.Input.MaxPending != 128 || d.Input.MaxBytes != 65536 {
		t.Errorf("input=%+v", d.Input)
	}
	if d.Events.PersistDeltas || d.Events.RingBytes != 8388608 || d.Events.SubscriberEvents != 256 {
		t.Errorf("events=%+v", d.Events)
	}
}

func TestParseSize(t *testing.T) {
	for s, want := range map[string]int{
		"32768": 32768, "32KiB": 32768, "32kib": 32768, "1MiB": 1048576,
		"8KB": 8000, "2G": 2 << 30, "100B": 100, "1.5KiB": 1536,
	} {
		got, err := config.ParseSize(s)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q)=%d err=%v, want %d", s, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "12XB", "-5KiB"} {
		if _, err := config.ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q): want error", bad)
		}
	}
}

// §3.2: limits take the minimum of all layers; a layer may only tighten.
func TestResolveLimitsMin(t *testing.T) {
	base := config.Defaults()
	eff := config.Resolve(base, []config.Layer{
		{Name: "workflow:w", Limits: &config.LimitsPatch{USD: ptr(2.0), Steps: ptr(900)}},
		{Name: "call", Limits: &config.LimitsPatch{USD: ptr(10.0), Steps: ptr(50)}},
	}, "")
	if eff.Limits.USD != 2.0 {
		t.Errorf("USD=%v, want min 2 (raise attempt ignored)", eff.Limits.USD)
	}
	if eff.Limits.Steps != 50 {
		t.Errorf("Steps=%v, want 50", eff.Limits.Steps)
	}
	if eff.Sources["limits.usd"] != "workflow:w" || eff.Sources["limits.steps"] != "call" {
		t.Errorf("sources=%v", eff.Sources)
	}
	if eff.Sources["limits.depth"] != "session" {
		t.Errorf("untouched limit source=%q", eff.Sources["limits.depth"])
	}
}

// §3.2: remap deep->fast at workflow layer; names resolve last.
func TestResolveProfileRemap(t *testing.T) {
	base := config.Defaults()
	eff := config.Resolve(base, []config.Layer{
		{Name: "workflow:verified-task", ProfileRemap: map[string]string{"deep": "fast"}},
	}, "deep")
	if eff.ProfileName != "fast" || eff.Profile.Model != "openai/gpt-5.2-mini" || eff.FellBack {
		t.Errorf("eff=%+v", eff)
	}
	if eff.Sources["profile_remap"] != "workflow:verified-task" {
		t.Errorf("sources=%v", eff.Sources)
	}
}

// §3.2 settled default: unknown profile -> default + FellBack, never fail.
func TestResolveUnknownProfileFallback(t *testing.T) {
	base := config.Defaults()
	eff := config.Resolve(base, nil, "no-such-profile")
	if !eff.FellBack || eff.ProfileName != "default" || eff.Profile.Model != "anthropic/claude-sonnet-5.5" {
		t.Errorf("eff=%+v", eff)
	}
	// Even with no profiles at all, Resolve returns rather than failing.
	eff2 := config.Resolve(config.SessionConfig{}, nil, "x")
	if !eff2.FellBack || eff2.ProfileName != "default" {
		t.Errorf("eff2=%+v", eff2)
	}
}

// §3.2: a workflow may redefine a profile for its subtree; the nearest
// table wins. Loop fields: last set wins (not min).
func TestResolveNearestProfileAndLoop(t *testing.T) {
	base := config.Defaults()
	cheap := config.Profile{Model: "openai/gpt-5.2-mini", Reasoning: model.ReasoningCfg{Effort: "low"}}
	eff := config.Resolve(base, []config.Layer{
		{Name: "workflow:w", Profiles: map[string]config.Profile{"deep": cheap},
			Loop: &config.LoopPatch{MaxSteps: ptr(10), CompactAt: ptr(0.5)}},
		{Name: "def:reviewer", Loop: &config.LoopPatch{MaxSteps: ptr(25)}},
	}, "deep")
	if eff.Profile.Model != "openai/gpt-5.2-mini" || eff.Sources["profile"] != "workflow:w" {
		t.Errorf("profile=%+v sources=%v", eff.Profile, eff.Sources)
	}
	if eff.Loop.MaxSteps != 25 || eff.Sources["loop.max_steps"] != "def:reviewer" {
		t.Errorf("loop=%+v (last set must win)", eff.Loop)
	}
	if eff.Loop.CompactAt != 0.5 {
		t.Errorf("compact_at=%v", eff.Loop.CompactAt)
	}
	// wantProfile beats a layer's ProfileName; layer name used when no want.
	eff2 := config.Resolve(base, []config.Layer{{Name: "workflow:w", ProfileName: "fast"}}, "deep")
	if eff2.ProfileName != "deep" {
		t.Errorf("wantProfile lost: %q", eff2.ProfileName)
	}
	eff3 := config.Resolve(base, []config.Layer{{Name: "workflow:w", ProfileName: "fast"}}, "")
	if eff3.ProfileName != "fast" || eff3.Sources["profile_name"] != "workflow:w" {
		t.Errorf("eff3=%+v", eff3)
	}
}
