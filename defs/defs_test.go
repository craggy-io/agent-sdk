package defs_test

import (
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/defs"
)

// The shipped catalogue loads and every built-in agent resolves.
func TestBuiltinCatalogueResolves(t *testing.T) {
	l := defs.Builtin()
	if err := l.Load(); err != nil {
		t.Fatal(err)
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
	if rev.Tools.MaxEffect != "read-only" || len(rev.OutputSchema) == 0 ||
		!strings.Contains(rev.SystemPrompt, "example-review") {
		t.Errorf("reviewer resolved=%+v", rev)
	}
	wfs := l.Workflows()
	if len(wfs) != 2 || wfs[0] != "agent-turn" || wfs[1] != "layered" {
		t.Errorf("workflows=%v", wfs)
	}
}
