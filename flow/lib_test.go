package flow_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	starjson "go.starlark.net/lib/json"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"github.com/craggy-io/agent-sdk/flow"
	"github.com/craggy-io/script-services/dialect"
	"github.com/craggy-io/script-services/values"
)

// called is one call a run made.
type called struct {
	Ref  string
	Args map[string]any
}

// run runs a bundle's run(ctx) over Lib with a call built-in that answers
// with answer, and returns what it called and printed.
func run(t *testing.T, b flow.Bundle, inputs map[string][]map[string]any, answer func(called) any) ([]called, []flow.Event, starlark.Value) {
	t.Helper()
	var calls []called
	var events []flow.Event
	call := starlark.NewBuiltin("call", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		c := called{Ref: string(args[0].(starlark.String)), Args: map[string]any{}}
		for _, kv := range kwargs {
			raw, err := values.ToJSON(kv[1])
			if err != nil {
				return nil, err
			}
			var v any
			json.Unmarshal(raw, &v)
			c.Args[string(kv[0].(starlark.String))] = v
		}
		calls = append(calls, c)
		raw, _ := json.Marshal(answer(c))
		return starjson.Module.Members["decode"].(*starlark.Builtin).CallInternal(nil, starlark.Tuple{starlark.String(raw)}, nil)
	})
	pre := starlark.StringDict{
		"call":   call,
		"struct": starlark.NewBuiltin("struct", starlarkstruct.Make),
		"json":   starjson.Module,
		"pipeline": starlark.NewBuiltin("pipeline", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
			return starlark.None, nil
		}),
	}
	mods := b.Modules()
	loaded := map[string]starlark.StringDict{}
	thread := &starlark.Thread{Print: func(_ *starlark.Thread, msg string) {
		if ev, ok := flow.ReadEvent(msg); ok {
			events = append(events, ev)
		}
	}}
	thread.Load = func(th *starlark.Thread, module string) (starlark.StringDict, error) {
		if g, ok := loaded[module]; ok {
			return g, nil
		}
		src, ok := mods[module]
		if !ok {
			return nil, fmt.Errorf("no module %s", module)
		}
		g, err := starlark.ExecFileOptions(dialect.FileOptions, th, module, src, pre)
		loaded[module] = g
		return g, err
	}
	g, err := starlark.ExecFileOptions(dialect.FileOptions, thread, flow.FileName, b[flow.FileName], pre)
	if err != nil {
		t.Fatal(err)
	}
	ctx := starlark.NewDict(2)
	in, _ := json.Marshal(inputs)
	inV, err := starjson.Module.Members["decode"].(*starlark.Builtin).CallInternal(thread, starlark.Tuple{starlark.String(in)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx.SetKey(starlark.String("inputs"), inV)
	files := starlark.NewDict(len(b))
	for p, text := range b.Flow() {
		files.SetKey(starlark.String(p), starlark.String(text))
	}
	ctx.SetKey(starlark.String("files"), files)
	out, err := starlark.Call(thread, g["run"], starlark.Tuple{ctx}, nil)
	if err != nil {
		if ee, ok := err.(*starlark.EvalError); ok {
			t.Fatal(ee.Backtrace())
		}
		t.Fatal(err)
	}
	return calls, events, out
}

// A call step calls the capability it names with its fixed arguments and
// those it binds to earlier steps, once per item inside a For each, and
// Needs adds that capability to requires.
func TestACallStepCallsAnyCapability(t *testing.T) {
	src := flow.Template(flow.Header{Title: "Notes", Inputs: map[string]flow.Input{"posts": {Dataset: "project-zero/posts", Key: "url"}}, InputOrder: []string{"posts"}})
	b := flow.Bundle{flow.FileName: src}
	var each, read, fetch, keep flow.Step
	for raw, s := range map[string]*flow.Step{
		`{"kind":"read","id":"posts","input":"posts"}`:                                                                            &read,
		`{"kind":"for_each","id":"each","over":"posts","item":"post","steps":[]}`:                                                 &each,
		`{"kind":"call","id":"fetch","capability":"artifacts.get@1","args":{"kind":"note"},"bind":{"id":"post.url"},"out":"doc"}`: &fetch,
		`{"kind":"keep_if","id":"big","over":"doc","field":"size","op":">","value":10}`:                                           &keep,
	} {
		if err := json.Unmarshal([]byte(raw), s); err != nil {
			t.Fatal(err)
		}
	}
	b, summary, err := flow.Apply(b, []flow.Op{
		{Op: "add", Step: &read},
		{Op: "add", Step: &each, After: "posts"},
		{Op: "add", Step: &fetch, Parent: "each"},
		{Op: "add", Step: &keep, After: "each"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(summary, "Added posts, added each, added fetch") {
		t.Fatalf("summary = %q", summary)
	}
	h, err := flow.ReadHeader([]byte(b[flow.FileName]))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.Requires, []string{"artifacts.get@1"}) {
		t.Fatalf("requires = %v", h.Requires)
	}
	if want := `flow.call(id = "fetch", capability = "artifacts.get@1", args = {"kind": "note"}, bind = {"id": "post.url"}, out = "doc")`; !strings.Contains(b[flow.FileName], want) {
		t.Fatalf("no %s in:\n%s", want, b[flow.FileName])
	}
	if probs := flow.Validate(b, flow.Env{Served: func(string) bool { return true }}); len(probs) > 0 {
		t.Fatalf("problems: %v", probs)
	}

	inputs := map[string][]map[string]any{"posts": flow.Keyed([]map[string]any{{"url": "a"}, {"url": "b"}}, "url")}
	calls, events, out := run(t, b, inputs, func(c called) any {
		return map[string]any{"id": c.Args["id"], "size": map[string]int{"a": 5, "b": 50}[c.Args["id"].(string)]}
	})
	want := []called{
		{Ref: "artifacts.get@1", Args: map[string]any{"kind": "note", "id": "a"}},
		{Ref: "artifacts.get@1", Args: map[string]any{"kind": "note", "id": "b"}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %+v", calls)
	}
	if got := out.(*starlark.Dict); got.String() != `{"posts": 2, "doc": 1}` {
		t.Fatalf("run returned %s", got)
	}
	var done []string
	for _, ev := range events {
		if ev.Done != "" {
			done = append(done, fmt.Sprintf("%s=%d", ev.Done, ev.Count))
		}
	}
	if strings.Join(done, " ") != "posts=2 fetch=2 each=2 big=1" {
		t.Fatalf("done events = %v", done)
	}
}

// A call step that binds a variable no step makes, or names no capability,
// is a problem.
func TestACallStepIsChecked(t *testing.T) {
	src := strings.Replace(flow.Template(flow.Header{Title: "Bad", Requires: []string{"artifacts.get@1"}}), "STEPS = []",
		`STEPS = [
    flow.call(id = "a", capability = "not a ref", out = "x"),
    flow.call(id = "b", capability = "artifacts.get@1", bind = {"id": "nothing.id"}),
]`, 1)
	probs := flow.Validate(flow.Bundle{flow.FileName: src}, flow.Env{})
	var msgs []string
	for _, p := range probs {
		msgs = append(msgs, p.Error())
	}
	got := strings.Join(msgs, "\n")
	for _, want := range []string{`step a: "not a ref" isn't a capability reference`, `step b: binds id to "nothing.id", which no earlier step makes`} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
}

func TestReadEvent(t *testing.T) {
	ev, ok := flow.ReadEvent(`flow:{"step":"find","item":2,"keys":["a"]}`)
	if !ok || ev.Step != "find" || *ev.Item != 2 || ev.Keys[0] != "a" {
		t.Fatalf("%+v %v", ev, ok)
	}
	if _, ok := flow.ReadEvent("hello"); ok {
		t.Fatal("a plain print is not an event")
	}
}
