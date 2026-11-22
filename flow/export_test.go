package flow_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/flow"
)

const exports = `load("@craggy/flow.star", "flow")

pipeline(
    title = "Exports",
    inputs = {"posts": flow.input("project-zero/posts", key = "slug")},
    requires = ["core.packs.write@1"],
)

STEPS = [
    flow.read(id = "posts", input = "posts"),
    flow.export(id = "as_json", over = "posts", path = "data/posts.json"),
    flow.export(id = "as_jsonl", over = "posts", format = "jsonl", path = "data/posts.jsonl"),
    flow.export(id = "as_csv", over = "posts", format = "csv", path = "data/posts.csv"),
    flow.export(id = "as_xml", over = "posts", format = "xml", path = "data/posts.xml"),
    flow.export(id = "as_yaml", over = "posts", format = "yaml", path = "data/posts.yaml"),
    flow.export(id = "some", over = "posts", format = "csv", path = "data/some.csv", fields = "score, slug"),
    flow.for_each(id = "each", over = "posts", item = "post", steps = [
        flow.export(id = "one", over = "post", format = "jsonl", path = "data/{post.slug}.jsonl"),
    ]),
]

def run(ctx):
    return flow.run(ctx, STEPS)
`

// An export step writes its items into the pack in each format, fields in
// the order the rows first have them (or as named), leaving out _ fields,
// with every row's key as a source.
func TestExportWritesEachFormat(t *testing.T) {
	b := flow.Bundle{flow.FileName: exports}
	if probs := flow.Validate(b, flow.Env{Served: func(string) bool { return true }}); len(probs) > 0 {
		t.Fatalf("problems: %v", probs)
	}
	rows := flow.Keyed([]map[string]any{
		{"slug": "heap", "title": `Heap, "groomed"`, "score": 0.9, "tags": []string{"heap", "jit"}},
		{"slug": "xml", "title": "Line one\nline <two> & three", "score": 1, "note": nil},
	}, "slug")
	written := map[string]string{}
	sources := map[string]string{}
	calls, _, _ := run(t, b, map[string]any{"posts": rows}, func(c called) any {
		path := c.Args["path"].(string)
		written[path] = c.Args["text"].(string)
		var src []string
		for _, s := range c.Args["sources"].([]any) {
			src = append(src, s.(string))
		}
		sources[path] = strings.Join(src, ",")
		return map[string]any{"path": path, "bytes": len(written[path])}
	})
	if len(calls) != 8 {
		t.Fatalf("%d calls", len(calls))
	}
	// Keyed rows reach the run with their fields sorted, so note, which only
	// the second row has, comes last.
	// q is a string as JSON writes it (<, > and & escaped).
	q := func(s string) string { raw, _ := json.Marshal(s); return string(raw) }
	groomed, lines := q(`Heap, "groomed"`), q("Line one\nline <two> & three")
	want := map[string]string{
		"data/posts.json": "[\n  {\n    \"score\": 0.9,\n    \"slug\": \"heap\",\n    \"tags\": [\n      \"heap\",\n      \"jit\"\n    ],\n" +
			"    \"title\": " + groomed + ",\n    \"note\": null\n  },\n" +
			"  {\n    \"score\": 1,\n    \"slug\": \"xml\",\n    \"tags\": null,\n    \"title\": " + lines + ",\n    \"note\": null\n  }\n]\n",
		"data/posts.jsonl": `{"score":0.9,"slug":"heap","tags":["heap","jit"],"title":` + groomed + `,"note":null}` + "\n" +
			`{"score":1,"slug":"xml","tags":null,"title":` + lines + `,"note":null}` + "\n",
		"data/posts.csv": "score,slug,tags,title,note\n" +
			`0.9,heap,"[""heap"",""jit""]","Heap, ""groomed""",` + "\n" +
			"1,xml,,\"Line one\nline <two> & three\",\n",
		"data/posts.xml": `<?xml version="1.0" encoding="UTF-8"?>
<rows>
  <row>
    <score>0.9</score>
    <slug>heap</slug>
    <tags>[&quot;heap&quot;,&quot;jit&quot;]</tags>
    <title>Heap, &quot;groomed&quot;</title>
    <note></note>
  </row>
  <row>
    <score>1</score>
    <slug>xml</slug>
    <tags></tags>
    <title>Line one
line &lt;two&gt; &amp; three</title>
    <note></note>
  </row>
</rows>
`,
		"data/posts.yaml": "- score: 0.9\n  slug: \"heap\"\n  tags: [\"heap\",\"jit\"]\n  title: " + groomed + "\n  note: null\n" +
			"- score: 1\n  slug: \"xml\"\n  tags: null\n  title: " + lines + "\n  note: null\n",
		"data/some.csv":   "score,slug\n0.9,heap\n1,xml\n",
		"data/heap.jsonl": `{"score":0.9,"slug":"heap","tags":["heap","jit"],"title":` + groomed + `}` + "\n",
	}
	for path, text := range want {
		if written[path] != text {
			t.Errorf("%s:\n%s\nwant:\n%s", path, written[path], text)
		}
	}
	if sources["data/posts.csv"] != "heap,xml" || sources["data/heap.jsonl"] != "heap" {
		t.Errorf("sources = %v", sources)
	}
}

// XML and YAML quote field names they can't write bare.
func TestExportQuotesOddNames(t *testing.T) {
	src := strings.Replace(exports, `flow.export(id = "as_json", over = "posts", path = "data/posts.json"),`,
		`flow.export(id = "as_json", over = "posts", format = "xml", path = "data/odd.xml", fields = "2nd, yes, xmlns"),
    flow.export(id = "y", over = "posts", format = "yaml", path = "data/odd.yaml", fields = "2nd, yes, ok-name"),`, 1)
	b := flow.Bundle{flow.FileName: src}
	written := map[string]string{}
	run(t, b, map[string]any{"posts": flow.Keyed([]map[string]any{{"slug": "a"}}, "slug")}, func(c called) any {
		written[c.Args["path"].(string)] = c.Args["text"].(string)
		return map[string]any{}
	})
	if got := written["data/odd.xml"]; !strings.Contains(got, `<field name="2nd"></field>`) || !strings.Contains(got, "<yes></yes>") || !strings.Contains(got, `<field name="xmlns"></field>`) {
		t.Errorf("xml:\n%s", got)
	}
	if got := written["data/odd.yaml"]; got != "- \"2nd\": null\n  \"yes\": null\n  ok-name: null\n" {
		t.Errorf("yaml:\n%s", got)
	}
}

// A format the step doesn't write is a problem, and an edited format is
// written back.
func TestExportIsChecked(t *testing.T) {
	src := strings.Replace(exports, `format = "jsonl", path = "data/posts.jsonl"`, `format = "pdf", path = "data/posts.pdf"`, 1)
	probs := flow.Validate(flow.Bundle{flow.FileName: src}, flow.Env{})
	if len(probs) != 1 || !strings.Contains(probs[0].Error(), `step as_jsonl: format is "pdf"; it must be one of json, jsonl, csv, xml, yaml`) {
		t.Fatalf("problems: %v", probs)
	}
	b, _, err := flow.Apply(flow.Bundle{flow.FileName: exports}, []flow.Op{{Op: "edit", ID: "as_csv", Set: map[string]json.RawMessage{"format": json.RawMessage(`"json"`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `flow.export(id = "as_csv", over = "posts", format = "json", path = "data/posts.csv"),`; !strings.Contains(b[flow.FileName], want) {
		t.Fatalf("no %s in:\n%s", want, b[flow.FileName])
	}
}
