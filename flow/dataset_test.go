package flow_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/flow"
)

const datasetSrc = `load("@craggy/flow.star", "flow")

pipeline(
    title = "Techniques",
    inputs = {"posts": flow.input("project-zero/posts", key = "slug")},
    requires = ["core.packs.write@1"],
)

STEPS = [
    flow.read(id = "posts", input = "posts"),
    flow.dataset(id = "save_ds", over = "posts", collection = "techniques", key = "slug", type = "notes.technique@1", fields = "slug:string, score:float", text = "slug"),
]

def run(ctx):
    return flow.run(ctx, STEPS)
`

func TestDatasetWritesRowsAndSchema(t *testing.T) {
	b := flow.Bundle{flow.FileName: datasetSrc}
	if probs := flow.Validate(b, flow.Env{Served: func(string) bool { return true }}); len(probs) > 0 {
		t.Fatalf("problems: %v", probs)
	}
	rows := flow.Keyed([]map[string]any{
		{"slug": "heap", "score": 0.9, "extra": "left out"},
		{"slug": "xml", "score": 1},
	}, "slug")
	written := map[string]string{}
	run(t, b, map[string]any{"posts": rows}, func(c called) any {
		written[c.Args["path"].(string)] = c.Args["text"].(string)
		return map[string]any{}
	})
	if got := written["datasets/techniques.jsonl"]; got != "{\"slug\":\"heap\",\"score\":0.9}\n{\"slug\":\"xml\",\"score\":1}\n" {
		t.Errorf("rows:\n%s", got)
	}
	cols, err := flow.ReadDatasetFiles(written)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 {
		t.Fatalf("%d collections", len(cols))
	}
	c := cols[0]
	if c.Type != "notes.technique@1" || c.Collection != "techniques" || c.Key != "slug" || c.Rows != 2 || c.File != "datasets/techniques.jsonl" {
		t.Errorf("collection: %+v", c)
	}
	if len(c.Fields) != 2 || c.Fields[0] != (flow.DatasetField{Name: "slug", Type: "string"}) || c.Fields[1] != (flow.DatasetField{Name: "score", Type: "float"}) {
		t.Errorf("fields: %+v", c.Fields)
	}
	if len(c.Text) != 1 || c.Text[0] != "slug" {
		t.Errorf("text: %v", c.Text)
	}
}

func TestDatasetInfersFields(t *testing.T) {
	src := strings.Replace(datasetSrc, `fields = "slug:string, score:float", text = "slug"`, ``, 1)
	b := flow.Bundle{flow.FileName: src}
	written := map[string]string{}
	run(t, b, map[string]any{"posts": flow.Keyed([]map[string]any{{"slug": "a", "n": 2, "ok": true}}, "slug")}, func(c called) any {
		written[c.Args["path"].(string)] = c.Args["text"].(string)
		return map[string]any{}
	})
	cols, err := flow.ReadDatasetFiles(written)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range cols[0].Fields {
		got[f.Name] = f.Type
	}
	if got["slug"] != "string" || got["n"] != "int" || got["ok"] != "bool" {
		t.Errorf("inferred: %v", got)
	}
}

func TestDatasetIsChecked(t *testing.T) {
	cases := []struct {
		from, to, want string
	}{
		{`fields = "slug:string, score:float"`, `fields = "slug:uuid"`, `type "uuid" isn't one of`},
		{`key = "slug", type`, `key = "nope", type`, `key "nope" isn't one of the fields named`},
		{`collection = "techniques"`, `collection = "Bad Name"`, `isn't a collection name`},
		{`type = "notes.technique@1"`, `type = "noversion"`, `isn't a dataset type`},
		{`fields = "slug:string, score:float"`, `fields = "slug:float"`, `a key is a string or an int`},
	}
	for _, tc := range cases {
		src := strings.Replace(datasetSrc, tc.from, tc.to, 1)
		probs := flow.Validate(flow.Bundle{flow.FileName: src}, flow.Env{})
		if len(probs) != 1 || !strings.Contains(probs[0].Error(), tc.want) {
			t.Errorf("%s -> %s: problems %v, want %q", tc.from, tc.to, probs, tc.want)
		}
	}
	// An edited collection is written back.
	b, _, err := flow.Apply(flow.Bundle{flow.FileName: datasetSrc}, []flow.Op{{Op: "edit", ID: "save_ds", Set: map[string]json.RawMessage{"collection": json.RawMessage(`"skills"`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b[flow.FileName], `flow.dataset(id = "save_ds", over = "posts", collection = "skills"`) {
		t.Fatalf("round trip:\n%s", b[flow.FileName])
	}
}

func TestReadDatasetFilesRefuses(t *testing.T) {
	schema := `{"type":"a.b@1","collection":"c","key":"id","fields":[{"name":"id","type":"string"}],"rows":1,"file":"datasets/c.jsonl"}`
	if _, err := flow.ReadDatasetFiles(map[string]string{"datasets/c.schema.json": schema}); err == nil || !strings.Contains(err.Error(), "not in the pack") {
		t.Errorf("missing rows: %v", err)
	}
	bad := strings.Replace(schema, `"string"`, `"uuid"`, 1)
	if _, err := flow.ReadDatasetFiles(map[string]string{"datasets/c.schema.json": bad, "datasets/c.jsonl": "{}\n"}); err == nil || !strings.Contains(err.Error(), `has type "uuid"`) {
		t.Errorf("bad type: %v", err)
	}
	if cols, err := flow.ReadDatasetFiles(map[string]string{"README.md": "x"}); err != nil || len(cols) != 0 {
		t.Errorf("no dataset: %v %v", cols, err)
	}
}
