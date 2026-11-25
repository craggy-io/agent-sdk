package flow_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/flow"
)

func TestNestedForEachGathersOutputsAndLineage(t *testing.T) {
	src := `load("@craggy/flow.star", "flow")
pipeline(title = "Nested", inputs = {"posts": flow.input("posts", key = "id")}, requires = ["notes.get@1"])
STEPS = [
    flow.read(id = "read", input = "posts"),
    flow.for_each(id = "outer", over = "posts", item = "post", steps = [
        flow.for_each(id = "inner", over = "post.groups", item = "group", steps = [
            flow.call(id = "fetch", capability = "notes.get@1", bind = {"id": "group.id"}, out = "found"),
        ]),
    ]),
    flow.sort(id = "sort", over = "found", by = "id"),
]
def run(ctx):
    return flow.run(ctx, STEPS)
`
	b := flow.Bundle{flow.FileName: src}
	if probs := flow.Validate(b, flow.Env{Served: func(string) bool { return true }}); len(probs) != 0 {
		t.Fatalf("validation: %v", probs)
	}
	inputs := map[string]any{"posts": flow.Keyed([]map[string]any{
		{"id": "p1", "groups": []map[string]any{{"id": "a", "_key": "g1"}, {"id": "b", "_key": "g2"}}},
		{"id": "p2", "groups": []map[string]any{}},
		{"id": "p3", "groups": []map[string]any{{"id": "c", "_key": "g3"}}},
	}, "id")}
	calls, events, out := run(t, b, inputs, func(c called) any {
		return map[string]any{"id": c.Args["id"]}
	})
	if len(calls) != 3 || calls[0].Args["id"] != "a" || calls[1].Args["id"] != "b" || calls[2].Args["id"] != "c" {
		t.Fatalf("calls = %+v", calls)
	}
	if got := out.String(); got != `{"posts": 3, "found": 3}` {
		t.Fatalf("output = %s", got)
	}
	var paths [][]int
	var keys [][]string
	done := map[string]int{}
	for _, ev := range events {
		if ev.Step == "fetch" {
			paths = append(paths, ev.Items)
			keys = append(keys, ev.Keys)
		}
		if ev.Done != "" {
			done[ev.Done] = ev.Count
		}
	}
	if !reflect.DeepEqual(paths, [][]int{{0, 0}, {0, 1}, {2, 0}}) {
		t.Fatalf("nested paths = %v", paths)
	}
	if !reflect.DeepEqual(keys, [][]string{{"p1", "g1"}, {"p1", "g2"}, {"p3", "g3"}}) {
		t.Fatalf("nested keys = %v", keys)
	}
	if done["outer"] != 3 || done["inner"] != 3 || done["fetch"] != 3 || done["sort"] != 3 {
		t.Fatalf("done counts = %v", done)
	}
}

func TestNestedForEachKeepsItemsScopedAndRestrictions(t *testing.T) {
	src := `load("@craggy/flow.star", "flow")
pipeline(title = "Nested")
STEPS = [
    flow.for_each(id = "outer", over = "rows", item = "row", steps = [
        flow.for_each(id = "inner", over = "row.parts", item = "part", steps = [
            flow.approve(id = "ask", prompt = "Review?"),
            flow.dataset(id = "save", over = "part", collection = "parts"),
        ]),
    ]),
    flow.sort(id = "bad", over = "part", by = "id"),
]
def run(ctx):
    return flow.run(ctx, STEPS)
`
	got := problems(flow.Validate(flow.Bundle{flow.FileName: src}, flow.Env{}))
	for _, want := range []string{"step ask: ask a person outside a For each", "step save: save as dataset outside a For each", "step bad: reads part, which no earlier step makes"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestThreeLevelsOfForEach(t *testing.T) {
	src := `load("@craggy/flow.star", "flow")
pipeline(title = "Three levels", inputs = {"rows": flow.input("rows", key = "id")}, requires = ["notes.get@1"])
STEPS = [
    flow.read(id = "read", input = "rows"),
    flow.for_each(id = "outer", over = "rows", item = "row", steps = [
        flow.for_each(id = "middle", over = "row.groups", item = "group", steps = [
            flow.for_each(id = "inner", over = "group.notes", item = "note", steps = [
                flow.call(id = "fetch", capability = "notes.get@1", bind = {"id": "note.id"}, out = "found"),
            ]),
        ]),
    ]),
]
def run(ctx):
    return flow.run(ctx, STEPS)
`
	b := flow.Bundle{flow.FileName: src}
	if probs := flow.Validate(b, flow.Env{Served: func(string) bool { return true }}); len(probs) != 0 {
		t.Fatalf("validation: %v", probs)
	}
	inputs := map[string]any{"rows": flow.Keyed([]map[string]any{{"id": "r", "groups": []map[string]any{{"notes": []map[string]any{{"id": "n"}}}}}}, "id")}
	_, events, out := run(t, b, inputs, func(c called) any { return map[string]any{"id": c.Args["id"]} })
	if out.String() != `{"rows": 1, "found": 1}` {
		t.Fatalf("output = %s", out)
	}
	for _, ev := range events {
		if ev.Step == "fetch" {
			if !reflect.DeepEqual(ev.Items, []int{0, 0, 0}) || ev.Item == nil || *ev.Item != 0 || !reflect.DeepEqual(ev.Keys, []string{"r"}) {
				t.Fatalf("deep event = %+v", ev)
			}
			return
		}
	}
	t.Fatal("no deep step event")
}
