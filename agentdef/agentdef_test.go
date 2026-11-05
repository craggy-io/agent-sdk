package agentdef_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/agent-sdk/agentdef"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"bundle.yaml": file(`apiVersion: craggy.io/bundle/v1
metadata: {id: agentsdk.shipped, version: 1.0.0}
contributions:
  - {id: base, kind: agentsdk.agent@1, path: agents/base.star}
  - {id: reviewer, kind: agentsdk.agent@1, path: agents/reviewer.star}
  - {id: assistant, kind: agentsdk.agent@1, path: agents/assistant.star}
  - {id: example-review, kind: agentsdk.skill@1, path: skills/example-review/SKILL.md}
  - {id: layered, kind: agentsdk.workflow@1, path: workflows/layered.star}
`),
		"agents/lib/common.star": file(`
def fragment(name, text):
    return {name: text}
`),
		"agents/base.star": file(`
load("./lib/common.star", "fragment")
agent(
    name = "base",
    description = "Foundation.",
    profile = "default",
    prompt = [
        fragment("role", "You are {{.Agent.Name}}."),
        {"conduct": "Be careful."},
        {"investigation": "Investigate first."},
    ],
    tools = tools_filter(max_effect = "any"),
    loop = loop_spec(max_steps = 80, completion = "text"),
)
`),
		"agents/reviewer.star": file(`
agent(
    name = "reviewer",
    extends = "base",
    profile = "deep",
    prompt = [
        {"review": "Review the proposal."},
        {"replace": "conduct", "text": "Be strict."},
        {"remove": "investigation"},
    ],
    tools = tools_filter(max_effect = "read-only"),
    skills = ["example-review"],
    loop = loop_spec(
        max_steps = 40,
        completion = "structured",
        output = schema.object({"verdict": schema.string()}),
    ),
)
`),
		"agents/assistant.star": file(`
agent(
    name = "assistant",
    extends = "base",
    prompt = [{"file": "assistant.md"}],
    delegates = ["reviewer"],
)
`),
		"agents/assistant.md":            file("Help interactively."),
		"skills/example-review/SKILL.md": file("---\nname: example-review\ndescription: Review checklist.\n---\nCheck everything.\n"),
		"workflows/layered.star":         file("def run(input):\n    return {}\n"),
	}
}

func open(t *testing.T, files fstest.MapFS) *agentdef.Loader {
	t.Helper()
	l, err := agentdef.Open(context.Background(), agentdef.Options{Shipped: bundle.Embedded("test", files)})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// openErr opens files as the shipped layer and returns the load error.
func openErr(t *testing.T, files fstest.MapFS) error {
	t.Helper()
	_, err := agentdef.Open(context.Background(), agentdef.Options{Shipped: bundle.Embedded("test", files)})
	if err == nil {
		t.Fatal("Open succeeded")
	}
	return err
}

func TestLoadAndResolveReviewer(t *testing.T) {
	l := open(t, testFS())
	if got := l.Names(); len(got) != 3 || got[0] != "assistant" {
		t.Fatalf("names=%v", got)
	}
	r, err := l.Resolve("reviewer", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Profile != "deep" || r.Provenance["profile"] != "reviewer" {
		t.Errorf("profile=%q prov=%v", r.Profile, r.Provenance)
	}
	if r.Tools.MaxEffect != "read-only" {
		t.Errorf("max_effect=%q, want tightened read-only", r.Tools.MaxEffect)
	}
	wantPrompt := "You are reviewer.\n\nBe strict.\n\nReview the proposal."
	if !strings.HasPrefix(r.SystemPrompt, wantPrompt) {
		t.Errorf("system prompt:\n%s\nwant prefix:\n%s", r.SystemPrompt, wantPrompt)
	}
	if !strings.Contains(r.SystemPrompt, "- `example-review`: Review checklist.") {
		t.Errorf("skills catalogue missing:\n%s", r.SystemPrompt)
	}
	if len(r.Fragments) != 3 || r.Fragments[1] != "conduct" || r.Provenance["prompt.conduct"] != "reviewer" {
		t.Errorf("fragments=%v prov=%v", r.Fragments, r.Provenance)
	}
	if r.Loop.MaxSteps == nil || *r.Loop.MaxSteps != 40 {
		t.Errorf("loop=%+v", r.Loop)
	}
	if !strings.Contains(string(r.OutputSchema), "verdict") {
		t.Errorf("output schema=%s", r.OutputSchema)
	}
	body, ok := l.SkillBody("example-review")
	if !ok || !strings.Contains(body, "Check everything.") {
		t.Errorf("skill body=%q ok=%v", body, ok)
	}
	if wf, ok := l.Workflow("layered"); !ok || !strings.Contains(wf.Source(), "def run") || wf.Unit.Path() != "workflows/layered.star" {
		t.Errorf("workflow not catalogued: %+v", wf)
	}
}

func TestResolveAssistantFileFragmentAndDelegates(t *testing.T) {
	l := open(t, testFS())
	r, err := l.Resolve("assistant", map[string]any{"Date": "2026-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.SystemPrompt, "Help interactively.") {
		t.Errorf("file fragment missing:\n%s", r.SystemPrompt)
	}
	if len(r.Delegates) != 1 || r.Delegates[0] != "reviewer" {
		t.Errorf("delegates=%v", r.Delegates)
	}
	if len(r.Chain) != 2 || r.Chain[0] != "base" {
		t.Errorf("chain=%v", r.Chain)
	}
}

func TestResolveDynamicRequiresExtends(t *testing.T) {
	l := open(t, testFS())
	if _, err := l.ResolveDynamic(agentdef.Definition{Name: "x"}, nil); err == nil {
		t.Fatal("blank dynamic accepted")
	}
	r, err := l.ResolveDynamic(agentdef.Definition{
		Name: "quick", Extends: "base", Profile: "fast",
		Prompt: []agentdef.PromptEntry{{Text: "Be quick."}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Profile != "fast" || !strings.Contains(r.SystemPrompt, "Be quick.") {
		t.Errorf("resolved=%+v", r)
	}
}

func TestExtendsCycleAndUnknown(t *testing.T) {
	l := open(t, fstest.MapFS{
		"bundle.yaml": file(`apiVersion: craggy.io/bundle/v1
metadata: {id: agentsdk.shipped, version: 1.0.0}
contributions:
  - {id: a, kind: agentsdk.agent@1, path: a.star}
  - {id: b, kind: agentsdk.agent@1, path: b.star}
  - {id: c, kind: agentsdk.agent@1, path: c.star}
`),
		"a.star": file(`agent(name = "a", extends = "b")`),
		"b.star": file(`agent(name = "b", extends = "a")`),
		"c.star": file(`agent(name = "c", extends = "ghost")`),
	})
	if _, err := l.Resolve("a", nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("cycle err=%v", err)
	}
	if _, err := l.Resolve("c", nil); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown err=%v", err)
	}
}

// One agent per contribution, named by its id; a SKILL.md names its own
// contribution too.
func TestAgentFileRules(t *testing.T) {
	manifest := func(contribs string) *fstest.MapFile {
		return file("apiVersion: craggy.io/bundle/v1\nmetadata: {id: agentsdk.shipped, version: 1.0.0}\ncontributions:\n" + contribs)
	}
	for name, tc := range map[string]struct {
		files fstest.MapFS
		want  string
	}{
		"name mismatch": {fstest.MapFS{
			"bundle.yaml": manifest("  - {id: x, kind: agentsdk.agent@1, path: x.star}\n"),
			"x.star":      file(`agent(name = "y")`),
		}, `declares agent "y" but bundle.yaml names the contribution "x"`},
		"two agents": {fstest.MapFS{
			"bundle.yaml": manifest("  - {id: x, kind: agentsdk.agent@1, path: x.star}\n"),
			"x.star":      file("agent(name = \"x\")\nagent(name = \"z\")\n"),
		}, "declares 2 agents"},
		"no agent": {fstest.MapFS{
			"bundle.yaml": manifest("  - {id: x, kind: agentsdk.agent@1, path: x.star}\n"),
			"x.star":      file("X = 1\n"),
		}, "declares 0 agents"},
		"skill name": {fstest.MapFS{
			"bundle.yaml": manifest("  - {id: s, kind: agentsdk.skill@1, path: s/SKILL.md}\n"),
			"s/SKILL.md":  file("---\nname: other\n---\nbody\n"),
		}, `names "other" but bundle.yaml names the contribution "s"`},
		"prompt file outside the bundle": {fstest.MapFS{
			"bundle.yaml":   manifest("  - {id: x, kind: agentsdk.agent@1, path: agents/x.star}\n"),
			"agents/x.star": file(`agent(name = "x", prompt = [{"file": "../../secret.md"}])`),
		}, "prompt file"},
		"undeclared load alias": {fstest.MapFS{
			"bundle.yaml": manifest("  - {id: x, kind: agentsdk.agent@1, path: x.star}\n"),
			"x.star":      file("load(\"@lib/h.star\", \"H\")\nagent(name = \"x\")\n"),
		}, "@lib"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := openErr(t, tc.files); !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func writeBundle(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Project replaces user replaces shipped, by name; Shadowed reports it.
func TestLayers(t *testing.T) {
	ctx := context.Background()
	userDir, project := t.TempDir(), t.TempDir()
	writeBundle(t, filepath.Join(userDir, agentdef.UserID), map[string]string{
		"bundle.yaml": `apiVersion: craggy.io/bundle/v1
metadata: {id: agentsdk.user, version: 1.0.0}
contributions:
  - {id: reviewer, kind: agentsdk.agent@1, path: agents/reviewer.star}
  - {id: assistant, kind: agentsdk.agent@1, path: agents/assistant.star}
`,
		"agents/reviewer.star":  `agent(name = "reviewer", extends = "base", profile = "user-deep")`,
		"agents/assistant.star": `agent(name = "assistant", extends = "base", profile = "user-fast")`,
	})
	writeBundle(t, project, map[string]string{
		"bundle.yaml": `apiVersion: craggy.io/bundle/v1
metadata: {id: agentsdk.project, version: 1.0.0}
contributions:
  - {id: assistant, kind: agentsdk.agent@1, path: assistant.star}
`,
		"assistant.star": `agent(name = "assistant", extends = "base", profile = "project")`,
	})
	l, err := agentdef.Open(ctx, agentdef.Options{
		Shipped: bundle.Embedded("test", testFS()), UserDir: userDir, Project: project,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"reviewer": "user-deep", "assistant": "project", "base": "default"} {
		if r, err := l.Resolve(name, nil); err != nil || r.Profile != want {
			t.Errorf("%s: profile=%v err=%v, want %s", name, r, err, want)
		}
	}
	var shadowed []string
	for _, o := range l.Shadowed() {
		shadowed = append(shadowed, o.Target.String()+">"+o.Replacement.String())
	}
	want := "agentsdk.shipped#assistant>agentsdk.project#assistant,agentsdk.shipped#reviewer>agentsdk.user#reviewer,agentsdk.user#assistant>agentsdk.project#assistant"
	if got := strings.Join(shadowed, ","); got != want {
		t.Errorf("shadowed=%s\nwant     %s", got, want)
	}
}

func TestLayerFolders(t *testing.T) {
	ctx := context.Background()
	// Missing folders are no layer.
	l, err := agentdef.Open(ctx, agentdef.Options{UserDir: filepath.Join(t.TempDir(), "none"), Project: filepath.Join(t.TempDir(), "none")})
	if err != nil || len(l.Names()) != 0 {
		t.Fatalf("names=%v err=%v", l, err)
	}
	// A folder without bundle.yaml is an error.
	project := t.TempDir()
	writeBundle(t, project, map[string]string{"agents/x.star": `agent(name = "x")`})
	if _, err := agentdef.Open(ctx, agentdef.Options{Project: project}); err == nil || !strings.Contains(err.Error(), "has no bundle.yaml") {
		t.Fatalf("err=%v", err)
	}
}

// InstallAgent writes the user bundle, file and manifest entry, and the
// definition replaces the shipped one; a source that does not load is
// rolled back.
func TestInstallAgent(t *testing.T) {
	ctx := context.Background()
	userDir := t.TempDir()
	l, err := agentdef.Open(ctx, agentdef.Options{Shipped: bundle.Embedded("test", testFS()), UserDir: userDir})
	if err != nil {
		t.Fatal(err)
	}
	p, err := l.InstallAgent(ctx, "quick", []byte(`agent(name = "quick", extends = "base", profile = "fast")`))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(userDir, agentdef.UserID, "agents", "quick.star"); p != want {
		t.Errorf("path=%s, want %s", p, want)
	}
	if r, err := l.Resolve("quick", nil); err != nil || r.Profile != "fast" {
		t.Fatalf("quick=%+v err=%v", r, err)
	}
	if _, err := l.InstallAgent(ctx, "reviewer", []byte(`agent(name = "reviewer", extends = "base", profile = "mine")`)); err != nil {
		t.Fatal(err)
	}
	// Reinstalling replaces the file in place; the manifest keeps one entry.
	if _, err := l.InstallAgent(ctx, "quick", []byte(`agent(name = "quick", extends = "base", profile = "faster")`)); err != nil {
		t.Fatal(err)
	}
	if r, _ := l.Resolve("reviewer", nil); r == nil || r.Profile != "mine" {
		t.Errorf("reviewer not replaced: %+v", r)
	}
	if r, _ := l.Resolve("quick", nil); r == nil || r.Profile != "faster" {
		t.Errorf("quick not replaced: %+v", r)
	}
	manifest, err := os.ReadFile(filepath.Join(userDir, agentdef.UserID, "bundle.yaml"))
	if err != nil || strings.Count(string(manifest), `id: "quick"`) != 1 || !strings.Contains(string(manifest), `id: "reviewer"`) {
		t.Fatalf("manifest=%s err=%v", manifest, err)
	}
	// A wrong name fails the install's load and changes nothing.
	_, err = l.InstallAgent(ctx, "broken", []byte(`agent(name = "other")`))
	var le *bundle.LoadError
	if !errors.As(err, &le) || !strings.Contains(err.Error(), `declares agent "other"`) {
		t.Fatalf("err=%v", err)
	}
	if _, ok := l.Get("broken"); ok {
		t.Error("broken agent catalogued")
	}
	if _, err := os.Stat(filepath.Join(userDir, agentdef.UserID, "agents", "broken.star")); !os.IsNotExist(err) {
		t.Errorf("broken file left behind: %v", err)
	}
	// A fresh loader over the same folders sees the installs.
	l2, err := agentdef.Open(ctx, agentdef.Options{Shipped: bundle.Embedded("test", testFS()), UserDir: userDir})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := l2.Resolve("quick", nil); r == nil || r.Profile != "faster" {
		t.Errorf("reopened quick=%+v", r)
	}
}

func TestAddDefinition(t *testing.T) {
	ctx := context.Background()
	l := open(t, testFS())
	if err := l.AddDefinition(ctx, &agentdef.Definition{Name: "gocode", Extends: "base", Profile: "go"}); err != nil {
		t.Fatal(err)
	}
	if r, err := l.Resolve("gocode", nil); err != nil || r.Profile != "go" {
		t.Fatalf("gocode=%+v err=%v", r, err)
	}
	if err := l.AddDefinition(ctx, &agentdef.Definition{Name: "gocode", Extends: "base", Profile: "go2"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := l.Resolve("gocode", nil); r == nil || r.Profile != "go2" {
		t.Errorf("gocode not replaced: %+v", r)
	}
	// A Go definition replaces nothing from a bundle.
	err := l.AddDefinition(ctx, &agentdef.Definition{Name: "base"})
	if err == nil || !strings.Contains(err.Error(), string(bundle.CodeIDConflict)) {
		t.Fatalf("err=%v", err)
	}
	if r, _ := l.Resolve("base", nil); r == nil || r.Profile != "default" {
		t.Errorf("base changed: %+v", r)
	}
}

func TestParseSourceAndBadMaxEffect(t *testing.T) {
	ctx := context.Background()
	if d, err := agentdef.ParseSource(ctx, []byte("agent(name = \"x\", tools = tools_filter(max_effect = \"wild\"))\n")); err == nil {
		t.Fatalf("bad max_effect accepted: %v", d)
	}
	d, err := agentdef.ParseSource(ctx, []byte("agent(name = \"x\", profile = \"fast\")\n"))
	if err != nil || d.Name != "x" || d.Profile != "fast" {
		t.Fatalf("def=%+v err=%v", d, err)
	}
	if _, err := agentdef.ParseSource(ctx, []byte("agent(name = \"x\")\nagent(name = \"y\")\n")); err == nil || !strings.Contains(err.Error(), "declares 2 agents") {
		t.Fatalf("two agents err=%v", err)
	}
	if _, err := agentdef.ParseSource(ctx, []byte("skill(name = \"s\", body = \"b\")\nagent(name = \"x\")\n")); err == nil || !strings.Contains(err.Error(), "skill") {
		t.Fatalf("skill() err=%v", err)
	}
}
