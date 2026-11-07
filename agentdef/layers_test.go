package agentdef_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/foundation-kit/bundle"
)

func TestIndependentBundlesComposeAndReload(t *testing.T) {
	ctx := context.Background()
	shipped, user, project, shared := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	writeBundle(t, shared, map[string]string{
		"bundle.yaml": "apiVersion: craggy.io/bundle/v1\nmetadata: {id: test.shared, version: 1.0.0}\n",
		"prompt.md":   "Shared prompt for {{.Agent.Name}}",
		"output.json": `{"type":"object","properties":{"verdict":{"type":"string"}}}`,
	})
	writeBundle(t, filepath.Join(shipped, "definitions", "base"), map[string]string{
		"bundle.yaml": "apiVersion: craggy.io/bundle/v1\nmetadata: {id: shipped.base, version: 1.0.0}\ncontributions:\n  - {id: base, kind: agentsdk.agent@1, path: agent.star}\n",
		"agent.star":  `agent(name="base", profile="shipped", prompt=[{"base":"shipped"}])`,
	})
	writeBundle(t, filepath.Join(user, "definitions", "base"), map[string]string{
		"bundle.yaml": "apiVersion: craggy.io/bundle/v1\nmetadata: {id: user.base, version: 1.0.0}\ncontributions:\n  - {id: base, kind: agentsdk.agent@1, path: agent.star}\n",
		"agent.star":  `agent(name="base", profile="user", prompt=[{"base":"user"}])`,
	})
	base := filepath.Join(project, "definitions", "base")
	writeBundle(t, base, map[string]string{
		"bundle.yaml": "apiVersion: craggy.io/bundle/v1\nmetadata: {id: project.base, version: 1.0.0}\ncontributions:\n  - {id: base, kind: agentsdk.agent@1, path: agent.star}\n",
		"agent.star":  `agent(name="base", profile="project", prompt=[{"base":"project"}])`,
	})
	writeBundle(t, filepath.Join(project, "definitions", "reviewer"), map[string]string{
		"bundle.yaml": "apiVersion: craggy.io/bundle/v1\nmetadata: {id: project.reviewer, version: 1.0.0}\ndependencies:\n  - {id: test.shared}\ncontributions:\n  - {id: reviewer, kind: agentsdk.agent@1, path: agent.star}\n",
		"agent.star":  `agent(name="reviewer", extends="base", skills=["review"], prompt=[{"file":"@test.shared/prompt.md"}], loop=loop_spec(output="@test.shared/output.json"))`,
	})
	skill := filepath.Join(project, "skills", "review")
	writeBundle(t, skill, map[string]string{
		"bundle.yaml": "apiVersion: craggy.io/bundle/v1\nmetadata: {id: project.skill.review, version: 1.0.0}\ncontributions:\n  - {id: review, kind: agentsdk.skill@1, path: SKILL.md}\n",
		"SKILL.md":    "---\nname: review\ndescription: Review carefully\n---\nOld skill body",
	})
	sources, err := agentdef.SourcesIn(ctx, shipped)
	if err != nil {
		t.Fatal(err)
	}
	l, err := agentdef.Open(ctx, agentdef.Options{Shipped: sources, UserDir: user, Project: project, Sources: []bundle.Source{bundle.Dir(shared)}})
	if err != nil {
		t.Fatal(err)
	}
	old, err := l.Resolve("reviewer", nil)
	if err != nil {
		t.Fatal(err)
	}
	if old.Profile != "project" || !strings.Contains(old.SystemPrompt, "Shared prompt for reviewer") || len(old.OutputSchema) == 0 || !strings.Contains(old.SkillBodies["review"], "Old skill body") {
		t.Fatalf("resolved=%+v", old)
	}
	// A valid disk edit changes new resolutions without mutating in-flight data.
	writeBundle(t, skill, map[string]string{"SKILL.md": "---\nname: review\ndescription: Review carefully\n---\nNew skill body"})
	if changed, err := l.Reload(ctx); err != nil || !changed {
		t.Fatalf("reload=%v %v", changed, err)
	}
	next, err := l.Resolve("reviewer", nil)
	if err != nil || !strings.Contains(next.SkillBodies["review"], "New skill body") || !strings.Contains(old.SkillBodies["review"], "Old skill body") {
		t.Fatal("mixed skill snapshots")
	}
	accepted := l.Catalog()
	writeBundle(t, base, map[string]string{"agent.star": "agent("})
	if _, err := l.Reload(ctx); err == nil || l.Catalog() != accepted {
		t.Fatal("bad reload replaced accepted catalog")
	}
	writeBundle(t, base, map[string]string{"agent.star": `agent(name="base",profile="project")`})
	duplicate := filepath.Join(project, "definitions", "duplicate")
	writeBundle(t, duplicate, map[string]string{
		"bundle.yaml": "apiVersion: craggy.io/bundle/v1\nmetadata: {id: project.duplicate, version: 1.0.0}\ncontributions:\n  - {id: base, kind: agentsdk.agent@1, path: agent.star}\n",
		"agent.star":  `agent(name="base")`,
	})
	if _, err := l.Reload(ctx); err == nil || !strings.Contains(err.Error(), "bundle.id_conflict") || l.Catalog() != accepted {
		t.Fatalf("duplicate reload=%v", err)
	}
	if err := os.RemoveAll(duplicate); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reload(ctx); err != nil {
		t.Fatal(err)
	}
}
