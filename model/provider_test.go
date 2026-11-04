package model_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/craggy-io/agent-sdk/model"
)

// FailureCode classifies by status and flags only, never message text.
func TestFailureCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"429", &model.Error{Status: 429, Transient: true}, model.FailRateLimited},
		{"429 wrapped", fmt.Errorf("sample: %w", &model.Error{Status: 429}), model.FailRateLimited},
		{"408", &model.Error{Status: 408}, model.FailTransient},
		{"502", &model.Error{Status: 502}, model.FailTransient},
		{"503", &model.Error{Status: 503}, model.FailTransient},
		{"transient flag, no status", &model.Error{Code: "idle_timeout", Transient: true}, model.FailTransient},
		{"context exceeded", &model.Error{Status: 400, Code: "context_length_exceeded", ContextExceeded: true}, model.FailContextExceeded},
		{"400", &model.Error{Status: 400, Code: "bad_request", Message: "rate limited"}, model.FailPermanent},
		{"401", &model.Error{Status: 401}, model.FailPermanent},
		{"panic", &model.Error{Code: "provider_panic"}, model.FailPermanent},
		{"cancelled", context.Canceled, model.FailCancelled},
		{"cancelled wrapped", fmt.Errorf("read: %w", context.Canceled), model.FailCancelled},
		{"deadline", context.DeadlineExceeded, model.FailCancelled},
		{"plain error", errors.New("boom"), model.FailProviderError},
	}
	for _, tc := range cases {
		if got := model.FailureCode(tc.err); got != tc.want {
			t.Errorf("%s: FailureCode = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Add sums counters and unions Reported in canonical order; two nil
// lists stay nil (presence unknown, not "nothing reported").
func TestUsageAdd(t *testing.T) {
	var u model.Usage
	u.Add(model.Usage{PromptTokens: 10, CostUSD: 0.5})
	if u.Reported != nil || u.PromptTokens != 10 || u.CostUSD != 0.5 {
		t.Fatalf("legacy add = %+v", u)
	}
	u.Add(model.Usage{CompletionTokens: 3, CostUSD: 0.25,
		Reported: []string{model.UsageCostUSD, model.UsageCompletionTokens}})
	u.Add(model.Usage{PromptTokens: 1,
		Reported: []string{model.UsagePromptTokens, model.UsageCostUSD, "extra"}})
	want := model.Usage{PromptTokens: 11, CompletionTokens: 3, CostUSD: 0.75,
		Reported: []string{model.UsagePromptTokens, model.UsageCompletionTokens, model.UsageCostUSD, "extra"}}
	if !reflect.DeepEqual(u, want) {
		t.Fatalf("Add = %+v, want %+v", u, want)
	}
}
