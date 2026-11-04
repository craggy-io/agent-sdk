package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// CallerInfo mirrors registry.Caller 1:1 (registry deferred, see task brief).
type CallerInfo struct {
	ID      string            `json:"id"`
	Kind    string            `json:"kind,omitempty"`
	Session string            `json:"session,omitempty"`
	Parent  *CallerInfo       `json:"parent,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// ReasoningCfg is per-profile reasoning configuration (§3.1).
type ReasoningCfg struct {
	Effort    string `json:"effort,omitempty" yaml:"effort,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// ProviderPrefs is OpenRouter provider routing preferences (§3.1, §8).
type ProviderPrefs struct {
	RequireParameters bool   `json:"require_parameters,omitempty" yaml:"require_parameters,omitempty"`
	DataCollection    string `json:"data_collection,omitempty" yaml:"data_collection,omitempty"`
	Sort              string `json:"sort,omitempty" yaml:"sort,omitempty"`
}

// Request is one sample request (§8).
type Request struct {
	Model             string        `json:"model"`
	Fallbacks         []string      `json:"fallbacks,omitempty"`
	Items             []Item        `json:"items"`
	Tools             []Tool        `json:"tools,omitempty"`
	Reasoning         ReasoningCfg  `json:"reasoning"`
	MaxOutputTokens   int           `json:"max_output_tokens,omitempty"`
	ParallelToolCalls bool          `json:"parallel_tool_calls,omitempty"`
	ToolChoice        string        `json:"tool_choice,omitempty"`
	Provider          ProviderPrefs `json:"provider"`
	Cache             string        `json:"cache,omitempty"`
	SessionID         string        `json:"session_id,omitempty"`
	IdleTimeout       time.Duration `json:"idle_timeout,omitempty"`
}

// Usage is provider accounting for one sample (§8).
//
// The counters are omitempty, so a reported zero and an absent field
// encode the same; Reported says which fields the provider actually
// returned (by their JSON names, in UsageFields order). A nil Reported
// means presence is unknown: a trace written before the field existed,
// or a provider that does not say.
type Usage struct {
	PromptTokens     int      `json:"prompt_tokens,omitempty"`
	CachedTokens     int      `json:"cached_tokens,omitempty"`
	CompletionTokens int      `json:"completion_tokens,omitempty"`
	ReasoningTokens  int      `json:"reasoning_tokens,omitempty"`
	CostUSD          float64  `json:"cost_usd,omitempty"`
	Reported         []string `json:"reported,omitempty"`
}

// Usage field names, as they appear in Usage's JSON and in Reported.
const (
	UsagePromptTokens     = "prompt_tokens"
	UsageCachedTokens     = "cached_tokens"
	UsageCompletionTokens = "completion_tokens"
	UsageReasoningTokens  = "reasoning_tokens"
	UsageCostUSD          = "cost_usd"
)

// UsageFields is the canonical order of Reported entries.
var UsageFields = []string{UsagePromptTokens, UsageCachedTokens,
	UsageCompletionTokens, UsageReasoningTokens, UsageCostUSD}

// Add sums o into u. Reported becomes the union of both lists in
// UsageFields order: a summed total lists a field when at least one
// summed sample reported it, so a total whose Reported lacks cost_usd
// had no reported cost at all (not "cost zero"). Per-sample presence
// stays on the leaves (step.response, step.attempt_failed).
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CachedTokens += o.CachedTokens
	u.CompletionTokens += o.CompletionTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.CostUSD += o.CostUSD
	u.Reported = unionFields(u.Reported, o.Reported)
}

// unionFields merges two Reported lists: UsageFields order first, then
// any unrecognised names in first-seen order. Returns a unchanged when b
// is empty, so two nil lists stay nil.
func unionFields(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	for _, f := range a {
		seen[f] = true
	}
	for _, f := range b {
		seen[f] = true
	}
	out := make([]string, 0, len(seen))
	for _, f := range UsageFields {
		if seen[f] {
			out = append(out, f)
			delete(seen, f)
		}
	}
	for _, f := range append(append([]string(nil), a...), b...) {
		if seen[f] {
			out = append(out, f)
			delete(seen, f)
		}
	}
	return out
}

// Response is one completed sample (§8).
type Response struct {
	Item         Item   `json:"item"`
	ServedModel  string `json:"served_model,omitempty"`
	GenerationID string `json:"generation_id,omitempty"`
	// Provider is the upstream provider a router (OpenRouter) sent the
	// request to, when reported; prompt caching is per provider, so it
	// explains cache misses that ServedModel cannot.
	Provider string `json:"provider,omitempty"`
	Usage    Usage  `json:"usage"`
}

// Delta is one streaming fragment. Kind: text|reasoning|tool_args.
type Delta struct {
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	CallIndex int    `json:"call_index,omitempty"`
}

// ModelInfo describes a model (§8: context window, pricing).
type ModelInfo struct {
	ID                  string  `json:"id"`
	ContextWindow       int     `json:"context_window,omitempty"`
	InputPricePerToken  float64 `json:"input_price_per_token,omitempty"`
	OutputPricePerToken float64 `json:"output_price_per_token,omitempty"`
}

// Error is a provider error mapped per §8.
type Error struct {
	Status          int           `json:"status,omitempty"`
	Code            string        `json:"code,omitempty"`
	Message         string        `json:"message,omitempty"`
	Provider        string        `json:"provider,omitempty"`
	Raw             string        `json:"raw,omitempty"`
	Transient       bool          `json:"transient,omitempty"`
	ContextExceeded bool          `json:"context_exceeded,omitempty"`
	RetryAfter      time.Duration `json:"retry_after,omitempty"`
	// Usage is what the provider reported for the failed attempt, when
	// it reported anything (an error body or stream that carried usage);
	// nil means none was reported, which does not mean the attempt was
	// free.
	Usage *Usage `json:"usage,omitempty"`
}

// TransientStatus reports whether an HTTP status is in the §5.3
// retryable set (408, 429, 502, 503).
func TransientStatus(status int) bool {
	switch status {
	case 408, 429, 502, 503:
		return true
	}
	return false
}

// Retryable reports whether the attempt may be retried as is (§5.3):
// the provider marked it transient or its status is retryable.
func (e *Error) Retryable() bool {
	return e != nil && (e.Transient || TransientStatus(e.Status))
}

// Failure codes: the stable class of a failed sample attempt, recorded
// as "code" on step.attempt_failed (see FailureCode).
const (
	FailRateLimited     = "rate_limited"     // HTTP 429
	FailTransient       = "transient"        // other retryable failures
	FailContextExceeded = "context_exceeded" // prompt over the context window
	FailCancelled       = "cancelled"        // context cancelled or deadline passed
	FailPermanent       = "permanent"        // any other *Error
	FailProviderError   = "provider_error"   // an error that is not an *Error
)

// FailureCode classifies a Sample error into one of the Fail* codes. It
// never reads message text: only Error's status and flags and context
// errors. Order: 429, context exceeded, retryable, cancellation, then
// permanent / provider_error.
func FailureCode(err error) string {
	var merr *Error
	if errors.As(err, &merr) && merr != nil {
		switch {
		case merr.Status == 429:
			return FailRateLimited
		case merr.ContextExceeded:
			return FailContextExceeded
		case merr.Retryable():
			return FailTransient
		}
		return FailPermanent
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return FailCancelled
	}
	return FailProviderError
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := e.Message
	if s == "" {
		s = e.Code
	}
	if e.Status != 0 {
		return fmt.Sprintf("model error %d %s: %s", e.Status, e.Code, s)
	}
	if e.Code != "" {
		return fmt.Sprintf("model error %s: %s", e.Code, s)
	}
	return "model error: " + s
}

// Provider is the only interface the loop sees (§8).
type Provider interface {
	Sample(ctx context.Context, req Request, onDelta func(Delta)) (Response, error)
	Model(ctx context.Context, id string) (ModelInfo, error)
}

// NewID returns a UUIDv7 string (§2).
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
