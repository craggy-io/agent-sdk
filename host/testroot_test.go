package host_test

import (
	"os"
	"path/filepath"
	"testing"
)

// testRoot writes the shared Starlark agent catalogue used by host tests.
func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := `
agent(name = "solo", profile = "default", prompt = [{"p": "You are solo."}])
agent(name = "parent", profile = "default", prompt = [{"p": "You are parent."}], delegates = ["child"])
agent(name = "child", profile = "default", prompt = [{"p": "You are child."}], delegates = ["grand"], tools = tools_filter(include = ["echo"]))
agent(name = "grand", profile = "default", prompt = [{"p": "You are grand."}])
agent(name = "badprof", profile = "nope", prompt = [{"p": "You are badprof."}])
agent(name = "lazy", profile = "default", prompt = [{"p": "You are lazy."}], tools = tools_filter(include = ["echo"]), loop = loop_spec(progressive_tools = True))
`
	if err := os.WriteFile(filepath.Join(dir, "team.star"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}
