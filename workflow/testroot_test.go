package workflow_test

import (
	"os"
	"path/filepath"
	"testing"
)

// testRoot writes the shared Starlark agent catalogue used by workflow tests.
func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := `
agent(name = "base", profile = "default", prompt = [{"p": "You are base."}], loop = loop_spec(completion = "text"))
agent(name = "assistant", extends = "base", delegates = ["reviewer"], workflows = True)
agent(name = "reviewer", extends = "base", tools = tools_filter(max_effect = "read-only"))
`
	if err := os.WriteFile(filepath.Join(dir, "team.star"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}
