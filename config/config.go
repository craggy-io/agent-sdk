// Package config is the layered session configuration (§3): named model
// profiles, run-tree limits, loop defaults, and operational bounds.
// Definitions name profiles, never models; Resolve applies layering with
// limits = minimum and profile lookup last (§3.2).
package config

import (
	"time"

	"github.com/craggy-io/agent-sdk/model"
)

// Profile is one named model binding (§3.1).
type Profile struct {
	Model             string              `yaml:"model" json:"model"`
	Fallbacks         []string            `yaml:"fallbacks,omitempty" json:"fallbacks,omitempty"`
	Reasoning         model.ReasoningCfg  `yaml:"reasoning,omitempty" json:"reasoning,omitempty"`
	MaxOutputTokens   int                 `yaml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	ParallelToolCalls bool                `yaml:"parallel_tool_calls,omitempty" json:"parallel_tool_calls,omitempty"`
	Provider          model.ProviderPrefs `yaml:"provider,omitempty" json:"provider,omitempty"`
	Cache             string              `yaml:"cache,omitempty" json:"cache,omitempty"`
	IdleTimeout       time.Duration       `yaml:"idle_timeout,omitempty" json:"idle_timeout,omitempty"`
}

// Limits caps the whole run tree (§3.1). Zero means no cap.
type Limits struct {
	USD         float64       `yaml:"usd,omitempty" json:"usd,omitempty"`
	WallClock   time.Duration `yaml:"wall_clock,omitempty" json:"wall_clock,omitempty"`
	Steps       int           `yaml:"steps,omitempty" json:"steps,omitempty"`
	ToolCalls   int           `yaml:"tool_calls,omitempty" json:"tool_calls,omitempty"`
	Depth       int           `yaml:"depth,omitempty" json:"depth,omitempty"`
	Concurrency int           `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
}

// LoopDefaults are defaults for every agent's Policy (§3.1).
type LoopDefaults struct {
	MaxSteps         int     `yaml:"max_steps,omitempty" json:"max_steps,omitempty"`
	ToolResultBytes  int     `yaml:"tool_result_bytes,omitempty" json:"tool_result_bytes,omitempty"`
	CompactAt        float64 `yaml:"compact_at,omitempty" json:"compact_at,omitempty"`
	KeepRecentTokens int     `yaml:"keep_recent_tokens,omitempty" json:"keep_recent_tokens,omitempty"`
	SummaryTokens    int     `yaml:"summary_tokens,omitempty" json:"summary_tokens,omitempty"`
	// BudgetNoticeAt are the fractions of a limit (run-tree limits and
	// the agent's max_steps) at which the agent is told its remaining
	// budget and to wrap up: the first a wrap-up notice, later ones
	// urgent. Empty disables the notices.
	BudgetNoticeAt []float64 `yaml:"budget_notice_at" json:"budget_notice_at"`
}

// InputCfg bounds mid-run input admission (§3.1, §6.6).
type InputCfg struct {
	QueueMode  string `yaml:"queue_mode,omitempty" json:"queue_mode,omitempty"` // one | all
	MaxPending int    `yaml:"max_pending,omitempty" json:"max_pending,omitempty"`
	MaxBytes   int    `yaml:"max_bytes,omitempty" json:"max_bytes,omitempty"`
}

// EventsCfg bounds event retention and subscribers (§3.1).
type EventsCfg struct {
	PersistDeltas    bool `yaml:"persist_deltas,omitempty" json:"persist_deltas,omitempty"`
	RingBytes        int  `yaml:"ring_bytes,omitempty" json:"ring_bytes,omitempty"`
	SubscriberEvents int  `yaml:"subscriber_events,omitempty" json:"subscriber_events,omitempty"`
}

// SessionConfig is the session-owned base layer (§3.1).
type SessionConfig struct {
	Profiles map[string]Profile `yaml:"profiles" json:"profiles"`
	Limits   Limits             `yaml:"limits" json:"limits"`
	Loop     LoopDefaults       `yaml:"loop" json:"loop"`
	Input    InputCfg           `yaml:"input" json:"input"`
	Events   EventsCfg          `yaml:"events" json:"events"`
}

// Defaults returns the §3.1 session config verbatim.
func Defaults() SessionConfig {
	return SessionConfig{
		Profiles: map[string]Profile{
			"default": {
				Model:             "anthropic/claude-sonnet-5.5",
				Fallbacks:         []string{"openai/gpt-5.2"},
				Reasoning:         model.ReasoningCfg{Effort: "medium"},
				MaxOutputTokens:   16000,
				ParallelToolCalls: true,
				Provider:          model.ProviderPrefs{RequireParameters: true, DataCollection: "deny"},
				Cache:             "auto",
			},
			"fast": {
				Model:     "openai/gpt-5.2-mini",
				Reasoning: model.ReasoningCfg{Effort: "low"},
			},
			"deep": {
				Model:     "anthropic/claude-opus-5.5",
				Reasoning: model.ReasoningCfg{Effort: "high"},
			},
		},
		Limits: Limits{
			USD: 5.00, WallClock: 30 * time.Minute,
			Steps: 400, ToolCalls: 1000, Depth: 6, Concurrency: 8,
		},
		Loop: LoopDefaults{
			MaxSteps: 80, ToolResultBytes: 32 * 1024, CompactAt: 0.8,
			KeepRecentTokens: 20000, SummaryTokens: 4000,
			BudgetNoticeAt: []float64{0.75, 0.9},
		},
		Input:  InputCfg{QueueMode: "one", MaxPending: 128, MaxBytes: 65536},
		Events: EventsCfg{PersistDeltas: false, RingBytes: 8388608, SubscriberEvents: 256},
	}
}
