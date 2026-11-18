package agentdef_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/foundation-kit/bundle"
)

func TestYAMLBehaviorDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, agent, program, want string }{
		{"unknown field", "name: demo\nskils: []", "", "skils"},
		{"duplicate", "name: demo\nname: another", "", "duplicate"},
		{"identity", "name: other\nloop: loop.star", "", "match"},
		{"no behavior", "name: demo", "", "executable loop"},
		{"path", "name: demo\nloop: ../loop.star", "", "loop path"},
		{"broken code", "name: demo\nloop: loop.star", "def run(", "loop.star"},
		{"wrong entry", "name: demo\nloop: loop.star", "def run(): pass", "run(step, max_steps)"},
		{"undeclared", "name: demo\nloop: loop.star", "load(\"@other/lib.star\",\"f\")\ndef run(step, limit): pass", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := fstest.MapFS{
				"bundle.yaml": file("apiVersion: craggy.io/bundle/v1\nmetadata: {id: chat.demo, version: 1.0.0}\ncontributions:\n  - {id: demo, kind: agentsdk.agent@1, path: agent.yaml}\n"),
				"agent.yaml":  file(tc.agent + "\n"), "loop.star": file(tc.program + "\n"),
			}
			_, err := agentdef.Open(context.Background(), agentdef.Options{Shipped: []bundle.Source{bundle.Embedded("demo", files)}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestYAMLToolsProgressivePolicy(t *testing.T) {
	files := fstest.MapFS{
		"bundle.yaml": file("apiVersion: craggy.io/bundle/v1\nmetadata: {id: chat.demo, version: 1.0.0}\ncontributions:\n  - {id: demo, kind: agentsdk.agent@1, path: agent.yaml}\n"),
		"agent.yaml":  file("name: demo\ntools:\n  include: ['*']\n  exclude: []\n  progressive: true\n  always_loaded: [skill, result_read]\nloop: loop.star\n"),
		"loop.star":   file("def run(step, max_steps):\n    step()\n"),
	}
	l, err := agentdef.Open(context.Background(), agentdef.Options{Shipped: []bundle.Source{bundle.Embedded("demo", files)}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := l.Resolve("demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools.Include) != 1 || r.Tools.Include[0] != "*" || len(r.Tools.Exclude) != 0 {
		t.Fatalf("tools = %+v", r.Tools)
	}
	if r.Loop.ProgressiveTools == nil || !*r.Loop.ProgressiveTools {
		t.Fatalf("progressive tools = %v", r.Loop.ProgressiveTools)
	}
	if got := strings.Join(r.Loop.PinnedTools, ","); got != "skill,result_read" {
		t.Fatalf("pinned tools = %q", got)
	}
}
