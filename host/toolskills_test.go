package host_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
)

// toolSkillRoot is a bundle with a skill for the secret tool and three
// agents: one that sees every tool, one whose filter keeps only echo, and
// one that names the skill itself.
func toolSkillRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"bundle.yaml": `apiVersion: craggy.io/bundle/v1
metadata: {id: agentsdk.toolskills, version: 1.0.0}
contributions:
  - {id: seer, kind: agentsdk.agent@1, path: seer.star}
  - {id: blind, kind: agentsdk.agent@1, path: blind.star}
  - {id: named, kind: agentsdk.agent@1, path: named.star}
  - {id: secret-guide, kind: agentsdk.skill@1, path: SKILL.md}
`,
		"seer.star":  `agent(name = "seer", profile = "default", prompt = [{"p": "You are seer."}])`,
		"blind.star": `agent(name = "blind", profile = "default", prompt = [{"p": "You are blind."}], tools = tools_filter(include = ["echo"]))`,
		"named.star": `agent(name = "named", profile = "default", prompt = [{"p": "You are named."}], skills = ["secret-guide"])`,
		"SKILL.md":   "---\nname: secret-guide\ndescription: How to keep secrets.\nfor_tools: [secr*, nothing.*]\n---\nAsk before telling.\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A skill whose for_tools matches a tool the agent can call comes with
// it: listed in the prompt, readable through the skill tool and recorded
// in provenance. An agent whose filter leaves the tool out never gets it,
// and one that names the skill carries it once.
func TestSkillsComeWithTheToolsTheyExplain(t *testing.T) {
	sink := tracemem.New()
	prov := newRouted(map[string][]func(model.Request) model.Response{
		"seer":  {builtinCall("c1", "skill", map[string]string{"name": "secret-guide"}), text("done")},
		"blind": {text("done")},
		"named": {text("done")},
	})
	s, err := host.Open(context.Background(), host.Options{
		Config: config.Defaults(), View: echoView(), Provider: prov,
		Defs: openDefs(t, toolSkillRoot(t)), Trace: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, name := range []string{"seer", "blind", "named"} {
		if _, err := s.RunAgent(context.Background(), name, "go", host.WithThread(name)); err != nil {
			t.Fatal(name, err)
		}
	}

	seer := prov.sent("seer")
	if !strings.Contains(seer[0].Items[0].Text, "- `secret-guide`: How to keep secrets.") || !toolNames(seer[0])["skill"] {
		t.Fatalf("seer was not given the skill: prompt %q tools %v", seer[0].Items[0].Text, toolNames(seer[0]))
	}
	if got := lastToolResult(seer[1]); got != "Ask before telling.\n" {
		t.Fatalf("skill body = %q", got)
	}
	blind := prov.sent("blind")[0]
	if strings.Contains(blind.Items[0].Text, "secret-guide") || toolNames(blind)["skill"] {
		t.Fatalf("an agent that cannot call secret was given its skill: %q", blind.Items[0].Text)
	}
	named := prov.sent("named")[0].Items[0].Text
	if strings.Count(named, "secret-guide") != 1 {
		t.Fatalf("a named skill is listed %d times: %q", strings.Count(named, "secret-guide"), named)
	}

	var provenance []string
	for _, e := range readAll(t, sink) {
		if e.Type == "agentdef.resolved" && strings.Contains(string(e.Data), `"def":"seer"`) {
			provenance = append(provenance, string(e.Data))
		}
	}
	if len(provenance) != 1 || !strings.Contains(provenance[0], `"skills.secret-guide":"tool secret"`) {
		t.Fatalf("provenance = %v", provenance)
	}
}
