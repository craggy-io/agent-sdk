package flow_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/flow"
)

// references are the pipelines under testdata, by folder name.
func references(t *testing.T) map[string]flow.Bundle {
	t.Helper()
	b, err := flow.ReadDir("testdata/project-zero-skills")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]flow.Bundle{"project-zero-skills": b}
}

// Every reference pipeline opens as a flow and is written back byte for byte
// when nothing was edited.
func TestReferencePipelinesRoundTripByteForByte(t *testing.T) {
	for name, b := range references(t) {
		src := []byte(b[flow.FileName])
		fl, err := flow.ReadFlow(src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !fl.HasSteps() || len(fl.Steps) == 0 {
			t.Fatalf("%s: no steps", name)
		}
		if got := string(fl.Source(fl.Steps)); got != string(src) {
			t.Fatalf("%s: written back differently:\n%s", name, got)
		}
		// Through JSON, as the UI sends a flow back.
		data, err := json.Marshal(fl.Steps)
		if err != nil {
			t.Fatal(err)
		}
		var back []*flow.Step
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		if got := string(fl.Source(back)); got != string(src) {
			t.Fatalf("%s: a flow sent back through JSON was written differently:\n%s", name, got)
		}
		h, err := flow.ReadHeader(src)
		if err != nil {
			t.Fatal(err)
		}
		again, err := flow.SetHeader(src, h)
		if err != nil || string(again) != string(src) {
			t.Fatalf("%s: header written back differently (%v):\n%s", name, err, again)
		}
	}
}

const handWritten = `load("@craggy/flow.star", "flow")

pipeline(
    title = "Hand-written",
    inputs = {"posts": flow.input("project-zero/posts", key = "url")},
    requires = ["core.packs.write@1"],
)

# A helper the flow can't express: kept as it is.
def titles(env):
    return [p["title"].upper() for p in env["posts"]]

STEPS = [
    flow.read(id = "posts", input = "posts"),
    flow.code(id = "titles", fn = titles, out = "titles"),
]

def run(ctx):
    return flow.run(ctx, STEPS)
`

// A hand-written function shows as a Code step and survives an edit to
// another step.
func TestCodeStepSurvivesAnEditElsewhere(t *testing.T) {
	b := flow.Bundle{flow.FileName: handWritten}
	fl, err := flow.ReadFlow([]byte(handWritten))
	if err != nil {
		t.Fatal(err)
	}
	if fl.Steps[1].Kind != "code" || !strings.Contains(fl.Functions["titles"], "upper()") {
		t.Fatalf("code step = %+v, functions = %v", fl.Steps[1], fl.Functions)
	}
	out, summary, err := flow.Apply(b, []flow.Op{{Op: "edit", ID: "posts", Set: map[string]json.RawMessage{"title": json.RawMessage(`"The posts"`)}}})
	if err != nil {
		t.Fatal(err)
	}
	src := out[flow.FileName]
	if !strings.Contains(src, "def titles(env):\n    return [p[\"title\"].upper() for p in env[\"posts\"]]\n") ||
		!strings.Contains(src, "# A helper the flow can't express: kept as it is.") ||
		!strings.Contains(src, `flow.code(id = "titles", fn = titles, out = "titles")`) ||
		!strings.Contains(src, `flow.read(id = "posts", title = "The posts", input = "posts")`) {
		t.Fatalf("edit lost something:\n%s", src)
	}
	if summary != "Changed the title of posts" {
		t.Fatalf("summary = %q", summary)
	}
}

func TestOpsAddMoveRemoveAndInputs(t *testing.T) {
	b := references(t)["project-zero-skills"]
	step := &flow.Step{}
	if err := json.Unmarshal([]byte(`{"kind":"sort","id":"by_name","title":"","over":"skills","by":"slug","desc":false}`), step); err != nil {
		t.Fatal(err)
	}
	text := "x"
	out, summary, err := flow.Apply(b, []flow.Op{
		{Op: "add", Step: step, After: "by_technique"},
		{Op: "edit", ID: "confident", Set: map[string]json.RawMessage{"value": json.RawMessage(`0.8`)}},
		{Op: "move", ID: "confident", Parent: "each_post"},
		{Op: "file", Path: "prompts/extra.md", Text: &text},
		{Op: "input", Name: "cves", Input: &flow.Input{Dataset: "nvd/cves", Key: "id"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	src := out[flow.FileName]
	for _, want := range []string{
		`    flow.sort(id = "by_name", over = "skills", by = "slug"),` + "\n",
		`            flow.keep_if(id = "confident", title = "Keep the confident ones", over = "techniques", field = "confidence", op = ">=", value = 0.8),` + "\n            flow.agent(",
		`"cves": flow.input("nvd/cves", key = "id")`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("missing %q in:\n%s", want, src)
		}
	}
	if out["prompts/extra.md"] != "x" || b["prompts/extra.md"] != "" {
		t.Fatal("file op wrong, or the input bundle changed")
	}
	if !strings.HasPrefix(summary, "Added by_name, changed the value of Keep the confident ones, moved") {
		t.Fatalf("summary = %q", summary)
	}
	out, _, err = flow.Apply(out, []flow.Op{{Op: "remove", ID: "by_name"}, {Op: "move", ID: "confident", Parent: "each_post", After: "find"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out[flow.FileName], "by_name") {
		t.Fatal("remove left the step")
	}
}

func env() flow.Env {
	served := map[string]bool{"core.models.complete@1": true, "core.workflows.run@1": true, "core.packs.write@1": true, "core.models.embed@1": true}
	return flow.Env{
		Host:   "the test host",
		Forbid: []string{"studio."},
		Served: func(r string) bool { return served[r] },
		Model: func(id string) (int, bool) {
			switch id {
			case "low":
				return 16000, true
			case "med":
				return 32000, true
			}
			return 0, false
		},
		Dataset: func(n string) bool { return n == "project-zero/posts" || n == "nvd/cves" },
	}
}

func TestReferencePipelinesValidate(t *testing.T) {
	for name, b := range references(t) {
		if probs := flow.Validate(b, env()); len(probs) > 0 {
			t.Fatalf("%s: %v", name, probs)
		}
	}
}

// A pipeline calling a Studio capability, an undeclared capability, an
// unlisted model or an over-cap max_tokens is refused at validation.
func TestValidationRefuses(t *testing.T) {
	base := references(t)["project-zero-skills"]
	cases := map[string]func(flow.Bundle) flow.Bundle{
		"can't call studio.* capabilities": func(b flow.Bundle) flow.Bundle {
			b["lib/sneaky.star"] = "def go():\n    return call(\"studio.runs.start@1\", pipeline = \"x\")\n"
			return b
		},
		"doesn't offer": func(b flow.Bundle) flow.Bundle {
			b[flow.FileName] = strings.Replace(b[flow.FileName], `"core.packs.write@1"]`, `"core.packs.write@1", "data.run@1"]`, 1)
			return b
		},
		"add it to requires": func(b flow.Bundle) flow.Bundle {
			b[flow.FileName] = strings.Replace(b[flow.FileName], `, "core.packs.write@1"]`, `]`, 1)
			return b
		},
		"isn't offered": func(b flow.Bundle) flow.Bundle {
			b[flow.FileName] = strings.Replace(b[flow.FileName], `model = "med", max_tokens`, `model = "gpt-9", max_tokens`, 1)
			return b
		},
		"over med's cap": func(b flow.Bundle) flow.Bundle {
			b[flow.FileName] = strings.Replace(b[flow.FileName], `max_tokens = 3000`, `max_tokens = 64000`, 1)
			return b
		},
		"no earlier step makes": func(b flow.Bundle) flow.Bundle {
			b[flow.FileName] = strings.Replace(b[flow.FileName], `over = "techniques", by = "slug"`, `over = "nothing", by = "slug"`, 1)
			return b
		},
		"craggy-data doesn't have": func(b flow.Bundle) flow.Bundle {
			b[flow.FileName] = strings.Replace(b[flow.FileName], `"project-zero/posts"`, `"elsewhere/posts"`, 1)
			return b
		},
		"doesn't compile": func(b flow.Bundle) flow.Bundle {
			b["schemas/find.json"] = `{"type": 7}`
			return b
		},
	}
	for want, mutate := range cases {
		probs := flow.Validate(mutate(base.Clone()), env())
		found := false
		for _, p := range probs {
			found = found || strings.Contains(p.Message, want)
		}
		if !found {
			t.Errorf("%s: problems %v", want, probs)
		}
	}
}

func TestDigestIsContentAddressed(t *testing.T) {
	b := references(t)["project-zero-skills"]
	c := b.Clone()
	if b.Digest() != c.Digest() {
		t.Fatal("same files, different digests")
	}
	c["prompts/find.md"] += " "
	if b.Digest() == c.Digest() {
		t.Fatal("a changed file kept the digest")
	}
}
