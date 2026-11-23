package host_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agentdef"
	_ "github.com/craggy-io/agent-sdk/workflow"
)

// testAgents is the shared agent catalogue used by host_test tests, one
// definition file per agent.
var testAgents = [][2]string{
	{"solo", `agent(name = "solo", profile = "default", prompt = [{"p": "You are solo."}])`},
	{"parent", `agent(name = "parent", profile = "default", prompt = [{"p": "You are parent."}], delegates = ["child"])`},
	{"child", `agent(name = "child", profile = "default", prompt = [{"p": "You are child."}], delegates = ["grand"], tools = tools_filter(include = ["echo"]))`},
	{"grand", `agent(name = "grand", profile = "default", prompt = [{"p": "You are grand."}])`},
	{"badprof", `agent(name = "badprof", profile = "nope", prompt = [{"p": "You are badprof."}])`},
	{"needy", `agent(name = "needy", profile = "default", prompt = [{"p": "You are needy."}], tools = tools_filter(include = ["echo", "web_fetch", "web_*", "gone"], exclude = ["gone"]))`},
	{"lazy", `agent(name = "lazy", profile = "default", prompt = [{"p": "You are lazy."}], tools = tools_filter(include = ["echo"]), loop = loop_spec(progressive_tools = True))`},
}

var testBehaviorAgents = [][2]string{
	{"custom-all", "tools: {}"},
	{"custom-read", "tools:\n  max_effect: read-only"},
	{"custom-filter", "tools:\n  include: [echo, secret, missing]\n  exclude: [secret]"},
	{"custom-progressive", "tools:\n  progressive: true\n  always_loaded: [echo]"},
}

// testRoot writes testAgents as a project-layer bundle, with its
// bundle.yaml, and returns its directory.
func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	var manifest strings.Builder
	manifest.WriteString("apiVersion: craggy.io/bundle/v1\nmetadata: {id: agentsdk.project, version: 1.0.0}\ncontributions:\n")
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, a := range testAgents {
		manifest.WriteString("  - {id: " + a[0] + ", kind: agentsdk.agent@1, path: agents/" + a[0] + ".star}\n")
		if err := os.WriteFile(filepath.Join(root, "agents", a[0]+".star"), []byte(a[1]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range testBehaviorAgents {
		manifest.WriteString("  - {id: " + a[0] + ", kind: agentsdk.agent@1, path: agents/" + a[0] + ".yaml}\n")
		body := "name: " + a[0] + "\nprofile: default\nprompt:\n  - {name: role, text: Test agent.}\n" + a[1] + "\nloop: loop.star\n"
		if err := os.WriteFile(filepath.Join(root, "agents", a[0]+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loop := "def run(step, max_steps):\n    for _ in range(max_steps):\n        status, value = step()\n        if status == 'actions':\n            status = value()\n        if status != 'continue':\n            return\n"
	if err := os.WriteFile(filepath.Join(root, "agents", "loop.star"), []byte(loop), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bundle.yaml"), []byte(manifest.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// openDefs opens a loader with root as its project layer.
func openDefs(t *testing.T, root string) *agentdef.Loader {
	t.Helper()
	l, err := agentdef.Open(context.Background(), agentdef.Options{Project: root})
	if err != nil {
		t.Fatal(err)
	}
	return l
}
