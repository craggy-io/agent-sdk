package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.starlark.net/starlark"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
)

// --- generic Starlark <-> JSON-shaped conversion ---------------------------

// objValue is a frozen string-keyed value with attribute and index
// access, used for inputs, results and receipts so scripts can write
// plan.text and input.message (§6.1/§6.2).
type objValue struct {
	typ    string
	fields map[string]any
	keys   []string // sorted
	frozen bool
}

func newObj(typ string, fields map[string]any) *objValue {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return &objValue{typ: typ, fields: fields, keys: keys}
}

func (o *objValue) String() string {
	parts := make([]string, 0, len(o.keys))
	for _, k := range o.keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, o.fields[k]))
	}
	return o.typ + "(" + strings.Join(parts, ", ") + ")"
}
func (o *objValue) Type() string          { return o.typ }
func (o *objValue) Freeze()               { o.frozen = true }
func (o *objValue) Truth() starlark.Bool  { return starlark.True }
func (o *objValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: %s", o.typ) }
func (o *objValue) AttrNames() []string   { return append([]string(nil), o.keys...) }
func (o *objValue) Attr(name string) (starlark.Value, error) {
	v, ok := o.fields[name]
	if !ok {
		return nil, nil
	}
	return toStar(v), nil
}

// Get implements starlark.Mapping for input["message"] access.
func (o *objValue) Get(k starlark.Value) (starlark.Value, bool, error) {
	s, ok := k.(starlark.String)
	if !ok {
		return nil, false, nil
	}
	v, ok := o.fields[string(s)]
	if !ok {
		return nil, false, nil
	}
	return toStar(v), true, nil
}

var (
	_ starlark.HasAttrs = (*objValue)(nil)
	_ starlark.Mapping  = (*objValue)(nil)
)

// toStar converts JSON-shaped Go data to a frozen Starlark value.
func toStar(v any) starlark.Value {
	switch t := v.(type) {
	case nil:
		return starlark.None
	case bool:
		return starlark.Bool(t)
	case string:
		return starlark.String(t)
	case int:
		return starlark.MakeInt(t)
	case int64:
		return starlark.MakeInt64(t)
	case float64:
		if t == float64(int64(t)) && t < 1e15 && t > -1e15 {
			return starlark.MakeInt64(int64(t))
		}
		return starlark.Float(t)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return starlark.MakeInt64(i)
		}
		f, _ := t.Float64()
		return starlark.Float(f)
	case []any:
		elems := make([]starlark.Value, len(t))
		for i, el := range t {
			elems[i] = toStar(el)
		}
		l := starlark.NewList(elems)
		l.Freeze()
		return l
	case map[string]any:
		o := newObj("object", t)
		o.Freeze()
		return o
	case json.RawMessage:
		var x any
		if json.Unmarshal(t, &x) == nil {
			return toStar(x)
		}
		return starlark.String(string(t))
	case starlark.Value:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return starlark.String(fmt.Sprint(t))
		}
		var x any
		if json.Unmarshal(b, &x) == nil {
			return toStar(x)
		}
		return starlark.String(string(b))
	}
}

// fromStar converts a Starlark value to JSON-shaped Go data.
func fromStar(v starlark.Value) (any, error) {
	switch t := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(t), nil
	case starlark.String:
		return string(t), nil
	case starlark.Int:
		if i, ok := t.Int64(); ok {
			return i, nil
		}
		f, _ := starlark.AsFloat(t)
		return f, nil
	case starlark.Float:
		return float64(t), nil
	case *objValue:
		return t.fields, nil
	case *specValue:
		return t.json(), nil
	case *handleValue:
		return map[string]any{"handle": t.id}, nil
	case *defValue:
		return defJSON(t.def), nil
	case *starlark.List:
		return fromIterable(t)
	case starlark.Tuple:
		return fromIterable(t)
	case *starlark.Dict:
		out := map[string]any{}
		for _, pair := range t.Items() {
			ks, ok := pair[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("workflow: dict keys must be strings, got %s", pair[0].Type())
			}
			val, err := fromStar(pair[1])
			if err != nil {
				return nil, err
			}
			out[string(ks)] = val
		}
		return out, nil
	case starlark.HasAttrs:
		// Struct-like values: read every attribute.
		out := map[string]any{}
		for _, name := range t.AttrNames() {
			av, err := t.Attr(name)
			if err != nil || av == nil {
				continue
			}
			val, err := fromStar(av)
			if err != nil {
				return nil, err
			}
			out[name] = val
		}
		return out, nil
	default:
		return nil, fmt.Errorf("workflow: cannot convert %s to JSON data", v.Type())
	}
}

func fromIterable(it starlark.Iterable) (any, error) {
	iter := it.Iterate()
	defer iter.Done()
	var out []any
	var x starlark.Value
	for iter.Next(&x) {
		val, err := fromStar(x)
		if err != nil {
			return nil, err
		}
		out = append(out, val)
	}
	if out == nil {
		out = []any{}
	}
	return out, nil
}

func strSlice(v starlark.Value) ([]string, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	if s, ok := v.(starlark.String); ok {
		return []string{string(s)}, nil
	}
	it, ok := v.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("want list of strings, got %s", v.Type())
	}
	iter := it.Iterate()
	defer iter.Done()
	var out []string
	var x starlark.Value
	for iter.Next(&x) {
		s, ok := x.(starlark.String)
		if !ok {
			return nil, fmt.Errorf("want list of strings, got %s element", x.Type())
		}
		out = append(out, string(s))
	}
	return out, nil
}

func strMap(v starlark.Value) (map[string]string, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	d, ok := v.(*starlark.Dict)
	if !ok {
		return nil, fmt.Errorf("want dict of strings, got %s", v.Type())
	}
	out := map[string]string{}
	for _, pair := range d.Items() {
		k, kok := pair[0].(starlark.String)
		val, vok := pair[1].(starlark.String)
		if !kok || !vok {
			return nil, fmt.Errorf("want dict of strings")
		}
		out[string(k)] = string(val)
	}
	return out, nil
}

func dictAny(v starlark.Value) (map[string]any, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	x, err := fromStar(v)
	if err != nil {
		return nil, err
	}
	m, ok := x.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("want dict, got %s", v.Type())
	}
	return m, nil
}

// filterFromStar parses a tools= argument: a glob string, a list of
// include globs, or {include, exclude, max_effect} (§6.2: narrow only).
func filterFromStar(v starlark.Value) (*model.FilterSpec, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	switch t := v.(type) {
	case starlark.String:
		return &model.FilterSpec{Include: []string{string(t)}, Inherit: true}, nil
	case *starlark.List, starlark.Tuple:
		inc, err := strSlice(v)
		if err != nil {
			return nil, err
		}
		return &model.FilterSpec{Include: inc, Inherit: true}, nil
	case *starlark.Dict:
		m, err := dictAny(v)
		if err != nil {
			return nil, err
		}
		return filterFromMap(m)
	default:
		return nil, fmt.Errorf("tools=: want string, list or dict, got %s", v.Type())
	}
}

func filterFromMap(m map[string]any) (*model.FilterSpec, error) {
	f := &model.FilterSpec{Inherit: true}
	for k, val := range m {
		switch k {
		case "include":
			f.Include = anyStrSlice(val)
		case "exclude":
			f.Exclude = anyStrSlice(val)
		case "max_effect":
			f.MaxEffect, _ = val.(string)
		case "allow":
			f.Allow = anyStrSlice(val)
		case "inherit":
			f.Inherit, _ = val.(bool)
		default:
			return nil, fmt.Errorf("tools filter: unknown key %q", k)
		}
	}
	return f, nil
}

func anyStrSlice(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, el := range t {
			if s, ok := el.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func anyInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		i, err := t.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func anyFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	}
	return 0, false
}

// loopLayerFromMap maps an agent(loop={...}) dict to a config layer.
func loopLayerFromMap(m map[string]any) (*config.Layer, error) {
	if len(m) == 0 {
		return nil, nil
	}
	p := &config.LoopPatch{}
	for k, val := range m {
		switch k {
		case "max_steps":
			n, ok := anyInt(val)
			if !ok {
				return nil, fmt.Errorf("loop.max_steps: want int")
			}
			p.MaxSteps = &n
		case "tool_result_bytes":
			n, ok := anyInt(val)
			if !ok {
				return nil, fmt.Errorf("loop.tool_result_bytes: want int")
			}
			p.ToolResultBytes = &n
		case "compact_at":
			f, ok := anyFloat(val)
			if !ok {
				return nil, fmt.Errorf("loop.compact_at: want number")
			}
			p.CompactAt = &f
		case "keep_recent_tokens":
			n, ok := anyInt(val)
			if !ok {
				return nil, fmt.Errorf("loop.keep_recent_tokens: want int")
			}
			p.KeepRecentTokens = &n
		case "summary_tokens":
			n, ok := anyInt(val)
			if !ok {
				return nil, fmt.Errorf("loop.summary_tokens: want int")
			}
			p.SummaryTokens = &n
		default:
			return nil, fmt.Errorf("loop: unknown key %q (use def loop fields via define for completion/output)", k)
		}
	}
	return &config.Layer{Name: "call", Loop: p}, nil
}

// loopSpecFromMap maps a define(loop={...}) dict to a agentdef.LoopSpec.
func loopSpecFromMap(m map[string]any) (agentdef.LoopSpec, error) {
	var ls agentdef.LoopSpec
	for k, val := range m {
		switch k {
		case "max_steps":
			n, ok := anyInt(val)
			if !ok {
				return ls, fmt.Errorf("loop.max_steps: want int")
			}
			ls.MaxSteps = &n
		case "completion":
			s, _ := val.(string)
			if s != "text" && s != "structured" {
				return ls, fmt.Errorf("loop.completion %q, want text|structured", s)
			}
			ls.Completion = &s
		case "output":
			b, err := json.Marshal(val)
			if err != nil {
				return ls, err
			}
			s := string(b)
			ls.Output = &s
		case "tool_result_bytes":
			n, ok := anyInt(val)
			if !ok {
				return ls, fmt.Errorf("loop.tool_result_bytes: want int")
			}
			ls.ToolResultBytes = &n
		case "wait_jobs":
			if s, ok := val.(string); ok {
				d, err := time.ParseDuration(s)
				if err != nil {
					return ls, fmt.Errorf("loop.wait_jobs: %w", err)
				}
				ls.WaitJobs = &d
			} else if f, ok := anyFloat(val); ok {
				d := time.Duration(f * float64(time.Second))
				ls.WaitJobs = &d
			} else {
				return ls, fmt.Errorf("loop.wait_jobs: want duration or seconds")
			}
		case "compact_at":
			f, ok := anyFloat(val)
			if !ok {
				return ls, fmt.Errorf("loop.compact_at: want number")
			}
			ls.CompactAt = &f
		case "keep_recent_tokens":
			n, ok := anyInt(val)
			if !ok {
				return ls, fmt.Errorf("loop.keep_recent_tokens: want int")
			}
			ls.KeepRecentTokens = &n
		case "summary_tokens":
			n, ok := anyInt(val)
			if !ok {
				return ls, fmt.Errorf("loop.summary_tokens: want int")
			}
			ls.SummaryTokens = &n
		case "progressive_tools":
			b, ok := val.(bool)
			if !ok {
				return ls, fmt.Errorf("loop.progressive_tools: want bool")
			}
			ls.ProgressiveTools = &b
		case "clear_results_at":
			f, ok := anyFloat(val)
			if !ok {
				return ls, fmt.Errorf("loop.clear_results_at: want number")
			}
			ls.ClearResultsAt = &f
		case "keep_recent_results":
			n, ok := anyInt(val)
			if !ok {
				return ls, fmt.Errorf("loop.keep_recent_results: want int")
			}
			ls.KeepRecentResults = &n
		default:
			return ls, fmt.Errorf("loop: unknown key %q", k)
		}
	}
	return ls, nil
}

func defJSON(d agentdef.Definition) map[string]any {
	b, _ := json.Marshal(d)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func defFromJSON(m map[string]any) (agentdef.Definition, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return agentdef.Definition{}, err
	}
	var d agentdef.Definition
	if err := json.Unmarshal(b, &d); err != nil {
		return agentdef.Definition{}, err
	}
	// LoopSpec has no yaml tags; JSON round-trip is exact.
	return d, nil
}

// agentResultJSON is the journal/Starlark shape of a host.Result (§6.2).
func agentResultJSON(r host.Result) map[string]any {
	m := map[string]any{
		"status": r.Status, "text": r.Text, "node": r.NodeID,
		"run": r.RunID, "def": r.DefName, "steps": r.Steps,
		"stop_reason": r.StopReason, "check_retries_used": r.OutputCheckRetriesUsed,
		"usage": map[string]any{
			"prompt_tokens": r.Usage.PromptTokens, "cached_tokens": r.Usage.CachedTokens,
			"completion_tokens": r.Usage.CompletionTokens, "reasoning_tokens": r.Usage.ReasoningTokens,
			"cost_usd": r.Usage.CostUSD,
		},
	}
	if len(r.Output) > 0 {
		var out any
		if json.Unmarshal(r.Output, &out) == nil {
			m["output"] = out
		} else {
			m["output"] = string(r.Output)
		}
	}
	return m
}

func resultObj(m map[string]any) *objValue {
	if m == nil {
		m = map[string]any{"status": "failed: unknown", "text": ""}
	}
	o := newObj("Result", m)
	o.Freeze()
	return o
}
