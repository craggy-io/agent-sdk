package model_test

import (
	"testing"

	"github.com/craggy-io/agent-sdk/model"
)

// §4.3: And(Or(Match(include)), Not(Match(exclude)),
// Or(EffectAtMost(max_effect), Match(allow))).
func TestFilterSpecAllows(t *testing.T) {
	tool := func(name, effect string) model.Tool { return model.Tool{Name: name, Effect: effect} }
	cases := []struct {
		name   string
		filter model.FilterSpec
		tool   model.Tool
		want   bool
	}{
		{"empty admits all", model.FilterSpec{}, tool("storage.get", "mutating"), true},
		{"include glob match", model.FilterSpec{Include: []string{"storage.*"}}, tool("storage.get", "read-only"), true},
		{"include glob nonmatch", model.FilterSpec{Include: []string{"storage.*"}}, tool("jobs.list", "read-only"), false},
		{"exclude wins over include", model.FilterSpec{Include: []string{"storage.*"}, Exclude: []string{"storage.revise.*"}}, tool("storage.revise.put", "read-only"), false},
		{"exclude exact", model.FilterSpec{Exclude: []string{"a.b"}}, tool("a.b", "read-only"), false},
		// Effect ordering: read-only < idempotent < mutating; long counts as mutating.
		{"max read-only admits read-only", model.FilterSpec{MaxEffect: "read-only"}, tool("x", "read-only"), true},
		{"max read-only rejects idempotent", model.FilterSpec{MaxEffect: "read-only"}, tool("x", "idempotent"), false},
		{"max read-only rejects mutating", model.FilterSpec{MaxEffect: "read-only"}, tool("x", "mutating"), false},
		{"max read-only rejects long", model.FilterSpec{MaxEffect: "read-only"}, tool("x", "long"), false},
		{"max idempotent admits idempotent", model.FilterSpec{MaxEffect: "idempotent"}, tool("x", "idempotent"), true},
		{"max idempotent rejects long-as-mutating", model.FilterSpec{MaxEffect: "idempotent"}, tool("x", "long"), false},
		{"max mutating admits long", model.FilterSpec{MaxEffect: "mutating"}, tool("x", "long"), true},
		{"any admits mutating", model.FilterSpec{MaxEffect: "any"}, tool("x", "mutating"), true},
		{"empty effect fails closed under read-only", model.FilterSpec{MaxEffect: "read-only"}, tool("x", ""), false},
		{"unknown effect fails closed under idempotent", model.FilterSpec{MaxEffect: "idempotent"}, tool("x", "weird"), false},
		{"unknown max_effect admits nothing", model.FilterSpec{MaxEffect: "weird"}, tool("x", "read-only"), false},
		{"bad glob is literal", model.FilterSpec{Include: []string{"a["}}, tool("a[", "read-only"), true},
		// Allow exempts named tools from max_effect only.
		{"allow admits a mutating tool past read-only", model.FilterSpec{MaxEffect: "read-only", Allow: []string{"feedback.submit"}}, tool("feedback.submit", "mutating"), true},
		{"allow glob", model.FilterSpec{MaxEffect: "read-only", Allow: []string{"feedback.*"}}, tool("feedback.submit", "mutating"), true},
		{"allow names only its tools", model.FilterSpec{MaxEffect: "read-only", Allow: []string{"feedback.submit"}}, tool("storage.put", "mutating"), false},
		{"exclude wins over allow", model.FilterSpec{MaxEffect: "read-only", Allow: []string{"feedback.submit"}, Exclude: []string{"feedback.*"}}, tool("feedback.submit", "mutating"), false},
		{"include still gates allow", model.FilterSpec{Include: []string{"storage.*"}, MaxEffect: "read-only", Allow: []string{"feedback.submit"}}, tool("feedback.submit", "mutating"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Allows(tc.tool); got != tc.want {
				t.Fatalf("Allows(%+v) = %v, want %v", tc.tool, got, tc.want)
			}
		})
	}
}

func TestNewIDIsUUIDv7(t *testing.T) {
	a, b := model.NewID(), model.NewID()
	if a == b || len(a) != 36 {
		t.Fatalf("ids %q %q", a, b)
	}
	if a[14] != '7' { // version nibble of canonical UUID string (§2: UUIDv7)
		t.Fatalf("id %q is not version 7", a)
	}
}

func TestErrorString(t *testing.T) {
	e := &model.Error{Status: 429, Code: "rate_limited", Message: "slow down", Transient: true}
	if s := e.Error(); s != "model error 429 rate_limited: slow down" {
		t.Fatalf("Error() = %q", s)
	}
	var nilErr *model.Error
	if nilErr.Error() != "<nil>" {
		t.Fatal("nil Error() should not panic")
	}
}
