package defs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/defs"
)

// The shipped catalogue loads and every built-in agent resolves.
func TestBuiltinCatalogueResolves(t *testing.T) {
	l, err := agentdef.Open(context.Background(), agentdef.Options{Shipped: defs.Source()})
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Names(); strings.Join(got, ",") != "assistant,base,reviewer" {
		t.Fatalf("agents=%v", got)
	}
	for _, name := range []string{"base", "assistant", "reviewer"} {
		r, err := l.Resolve(name, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.SystemPrompt == "" {
			t.Errorf("%s: empty system prompt", name)
		}
	}
	rev, err := l.Resolve("reviewer", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Tools.MaxEffect != "read-only" || !strings.Contains(string(rev.OutputSchema), `"verdict"`) ||
		!strings.Contains(rev.SystemPrompt, "example-review") || strings.Contains(rev.SystemPrompt, "When investigating") {
		t.Errorf("reviewer resolved=%+v", rev)
	}
	if def, ok := l.Get("reviewer"); !ok || def.Source != "agents/reviewer.star" {
		t.Errorf("reviewer source=%+v", def)
	}
	wfs := l.Workflows()
	if len(wfs) != 2 || wfs[0] != "agent-turn" || wfs[1] != "layered" {
		t.Errorf("workflows=%v", wfs)
	}
	if got := l.SkillNames(); len(got) != 1 || got[0] != "example-review" {
		t.Errorf("skills=%v", got)
	}
}
