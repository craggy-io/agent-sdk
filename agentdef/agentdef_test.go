package agentdef_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/craggy-io/agent-sdk/agentdef"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"agents/team.star": {Data: []byte(`
agent(
    name = "base",
    description = "Foundation.",
    profile = "default",
    prompt = [
        {"role": "You are {{.Agent.Name}}."},
        {"conduct": "Be careful."},
        {"investigation": "Investigate first."},
    ],
    tools = tools_filter(max_effect = "any"),
    loop = loop_spec(max_steps = 80, completion = "text"),
)

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

agent(
    name = "assistant",
    extends = "base",
    prompt = [{"file": "assistant.md"}],
    delegates = ["reviewer"],
)
`)},
		"agents/assistant.md":            {Data: []byte("Help interactively.")},
		"skills/example-review/SKILL.md": {Data: []byte("---\nname: example-review\ndescription: Review checklist.\n---\nCheck everything.\n")},
		"workflows/layered.star":         {Data: []byte("def run(input):\n    return {}\n")},
	}
}

func TestLoadAndResolveReviewer(t *testing.T) {
	l := agentdef.Roots(testFS())
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
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
	if _, ok := l.WorkflowSource("layered"); !ok {
		t.Errorf("workflow not catalogued")
	}
}

func TestResolveAssistantFileFragmentAndDelegates(t *testing.T) {
	l := agentdef.Roots(testFS())
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
	l := agentdef.Roots(testFS())
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
	l := agentdef.Roots(fstest.MapFS{
		"agents/a.star": {Data: []byte("agent(name = \"a\", extends = \"b\")\nagent(name = \"b\", extends = \"a\")\nagent(name = \"c\", extends = \"ghost\")\n")},
	})
	if _, err := l.Resolve("a", nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("cycle err=%v", err)
	}
	if _, err := l.Resolve("c", nil); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown err=%v", err)
	}
}

func TestParseSourceAndBadMaxEffect(t *testing.T) {
	defs, _, err := agentdef.ParseSource(nil, "agents/x.star", []byte("agent(name = \"x\", tools = tools_filter(max_effect = \"wild\"))\n"))
	if err == nil {
		t.Fatalf("bad max_effect accepted: %v", defs)
	}
	defs, skills, err := agentdef.ParseSource(nil, "agents/x.star", []byte("skill(name = \"s\", body = \"do it\")\nagent(name = \"x\")\n"))
	if err != nil || len(defs) != 1 || len(skills) != 1 {
		t.Fatalf("defs=%v skills=%v err=%v", defs, skills, err)
	}
}
