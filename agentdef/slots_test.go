package agentdef_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/craggy-io/agent-sdk/agentdef"
)

// slotFS is an agent that seeks memory, and two memories that fill it.
func slotFS() fstest.MapFS {
	return fstest.MapFS{
		"bundle.yaml": file(`apiVersion: craggy.io/bundle/v1
metadata: {id: agentsdk.slots, version: 1.0.0}
contributions:
  - {id: base, kind: agentsdk.agent@1, path: base.star}
  - {id: helper, kind: agentsdk.agent@1, path: helper.star}
  - {id: basic-memory, kind: agentsdk.slot-filler@1, path: basic/fills.yaml}
  - {id: graph-memory, kind: agentsdk.slot-filler@1, path: graph/fills.yaml}
  - {id: notebook, kind: agentsdk.skill@1, path: basic/SKILL.md}
`),
		"base.star": file(`
agent(name = "base", prompt = [{"role": "You are {{.Agent.Name}}."}], seeks = ["memory"])
`),
		"helper.star": file(`
agent(name = "helper", extends = "base", seeks = [seek("memory", optional = True, prefer = "graph-memory"), "search"])
`),
		"basic/fills.yaml": file(`slot: memory
title: Basic memory
priority: 10
tools: [memory_*]
prompt: activation.md
skills: [notebook]
seed: memory.seed
`),
		"basic/activation.md": file("Search memory before you start, {{.Agent.Name}}."),
		"basic/SKILL.md":      file("---\nname: notebook\ndescription: Keeping notes.\n---\nWrite what you found.\n"),
		"graph/fills.yaml": file(`slot: memory
title: Graph memory
priority: 5
tools: [graph_*]
prompt: graph.md
`),
		"graph/graph.md": file("Use the graph."),
	}
}

func TestSeeksMergeAlongTheChain(t *testing.T) {
	l := open(t, slotFS())
	r, err := l.Resolve("helper", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []agentdef.Seek{{Slot: "memory", Optional: true, Prefer: "graph-memory"}, {Slot: "search"}}
	if len(r.Seeks) != 2 || r.Seeks[0] != want[0] || r.Seeks[1] != want[1] {
		t.Fatalf("seeks = %+v, want %+v", r.Seeks, want)
	}
	if r.Provenance["seeks.memory"] != "helper" {
		t.Fatalf("provenance = %v", r.Provenance)
	}
	if len(r.Filled) != 0 || len(r.Pinned) != 0 || strings.Contains(r.SystemPrompt, "Search memory") {
		t.Fatalf("an unfilled resolution has what a filler brings: %+v", r)
	}
}

func TestFillersAreRankedByPriority(t *testing.T) {
	l := open(t, slotFS())
	got := l.Fillers("memory")
	if len(got) != 2 || got[0].Name != "basic-memory" || got[1].Name != "graph-memory" {
		t.Fatalf("fillers = %+v", got)
	}
	f := got[0]
	if f.Title != "Basic memory" || f.Seed != "memory.seed" || f.PromptFile != "basic/activation.md" || !strings.Contains(f.Prompt, "Search memory") {
		t.Fatalf("filler = %+v", f)
	}
	if len(l.Fillers("search")) != 0 {
		t.Fatal("a slot nothing fills has fillers")
	}
}

func TestResolveFilledAddsWhatTheFillerBrings(t *testing.T) {
	l := open(t, slotFS())
	r, err := l.ResolveFilled("base", nil, []agentdef.Fill{{Slot: "memory", Filler: "basic-memory"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.SystemPrompt, "You are base.\n\nSearch memory before you start, base.\n\n## Skills") {
		t.Fatalf("system prompt:\n%s", r.SystemPrompt)
	}
	if !strings.Contains(r.SystemPrompt, "- `notebook`: Keeping notes.") || r.SkillBodies["notebook"] == "" {
		t.Fatalf("the filler's skill is missing:\n%s", r.SystemPrompt)
	}
	if len(r.Fragments) != 2 || r.Fragments[1] != "slot.memory" || r.Provenance["prompt.slot.memory"] != "basic-memory" {
		t.Fatalf("fragments = %v provenance = %v", r.Fragments, r.Provenance)
	}
	if len(r.Pinned) != 1 || r.Pinned[0] != "memory_*" {
		t.Fatalf("pinned = %v", r.Pinned)
	}
	// The other memory's tools are left out; the chosen one's are not.
	if len(r.Tools.Exclude) != 1 || r.Tools.Exclude[0] != "graph_*" {
		t.Fatalf("exclude = %v", r.Tools.Exclude)
	}
	if len(r.Filled) != 1 || r.Filled[0] != (agentdef.Filled{Slot: "memory", Filler: "basic-memory"}) {
		t.Fatalf("filled = %+v", r.Filled)
	}
}

func TestResolveFilledRefusesAWrongFill(t *testing.T) {
	l := open(t, slotFS())
	for _, fills := range [][]agentdef.Fill{
		{{Slot: "memory", Filler: "nosuch"}},
		{{Slot: "search", Filler: "basic-memory"}},
		{{Slot: "memory", Filler: "basic-memory"}, {Slot: "memory", Filler: "graph-memory"}},
	} {
		if _, err := l.ResolveFilled("base", nil, fills); err == nil {
			t.Errorf("fills %+v resolved", fills)
		}
	}
}

func TestSlotRules(t *testing.T) {
	bad := map[string]string{
		"a bad slot name":        `agent(name = "base", seeks = ["Memory"])`,
		"a slot sought twice":    `agent(name = "base", seeks = ["memory", seek("memory")])`,
		"a seek of a wrong type": `agent(name = "base", seeks = [1])`,
	}
	for what, src := range bad {
		files := fstest.MapFS{
			"bundle.yaml": file("apiVersion: craggy.io/bundle/v1\nmetadata: {id: agentsdk.bad, version: 1.0.0}\ncontributions:\n  - {id: base, kind: agentsdk.agent@1, path: base.star}\n"),
			"base.star":   file(src),
		}
		if err := openErr(t, files); err == nil {
			t.Errorf("%s loaded", what)
		}
	}
	fillers := map[string]string{
		"no title":         "slot: memory\ntools: [memory_*]\n",
		"brings nothing":   "slot: memory\ntitle: Empty\n",
		"a bad slot":       "slot: Memory\ntitle: X\ntools: [a]\n",
		"an unknown key":   "slot: memory\ntitle: X\ntools: [a]\ncolour: red\n",
		"a missing prompt": "slot: memory\ntitle: X\nprompt: nope.md\n",
	}
	for what, src := range fillers {
		files := fstest.MapFS{
			"bundle.yaml": file("apiVersion: craggy.io/bundle/v1\nmetadata: {id: agentsdk.bad, version: 1.0.0}\ncontributions:\n  - {id: f, kind: agentsdk.slot-filler@1, path: fills.yaml}\n"),
			"fills.yaml":  file(src),
		}
		if err := openErr(t, files); err == nil {
			t.Errorf("a filler with %s loaded", what)
		}
	}
}
