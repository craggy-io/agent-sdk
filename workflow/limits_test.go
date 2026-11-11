package workflow

import (
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/script-services/script"
)

func TestWorkflowLimitsFollowSessionBudget(t *testing.T) {
	s := &host.Host{}
	s.SetConfig(config.SessionConfig{Limits: config.Limits{WallClock: time.Hour}})
	got := (&exec{s: s}).limits()
	if got.Timeout != time.Hour || got.MaxSteps != script.DefaultLimits.MaxSteps {
		t.Fatalf("session limits = %+v", got)
	}

	override := script.Limits{MaxSteps: 17, Timeout: time.Second}
	if got := (&exec{s: s, lim: override}).limits(); got != override {
		t.Fatalf("explicit limits = %+v, want %+v", got, override)
	}

	s.SetConfig(config.SessionConfig{})
	if got := (&exec{s: s}).limits(); got != script.DefaultLimits {
		t.Fatalf("default limits = %+v, want %+v", got, script.DefaultLimits)
	}
}
