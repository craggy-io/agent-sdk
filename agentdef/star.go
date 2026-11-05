package agentdef

import (
	"encoding/json"
	"fmt"
	"path"
	"time"

	"go.starlark.net/starlark"

	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/model"
	scriptschema "github.com/craggy-io/script-services/schema"
)

// ValidateMaxEffect rejects unknown max_effect caps at definition time
// (an unrecognised cap would silently admit nothing at run time).
func ValidateMaxEffect(name, eff string) error {
	switch eff {
	case "", "read-only", "idempotent", "mutating", "any":
		return nil
	default:
		return fmt.Errorf("agentdef: %s: tools.max_effect %q, want read-only|idempotent|mutating|any", name, eff)
	}
}

// collector gathers the agent() declarations of one definition file. read
// serves the file's bundle, for prompt and schema files.
type collector struct {
	file string // path in the bundle, e.g. agents/reviewer.star
	dir  string // directory of file, for relative file reads
	read func(name string) ([]byte, error)
	defs []*Definition
}

// predeclared is what a definition file sees: agent() plus the pure
// helpers in helpers.
func (c *collector) predeclared() starlark.StringDict {
	out := helpers()
	out["agent"] = starlark.NewBuiltin("agent", c.agentBuiltin)
	return out
}

// helpers are the pure built-ins definition files and the modules they
// load() share. Modules cannot declare agents.
func helpers() starlark.StringDict {
	return starlark.StringDict{
		"tools_filter": starlark.NewBuiltin("tools_filter", specBuiltin("tools_filter")),
		"loop_spec":    starlark.NewBuiltin("loop_spec", specBuiltin("loop_spec")),
		"schema":       scriptschema.Module(),
	}
}

// specBuiltin turns kwargs into a plain dict; agent() decodes it. This
// keeps tools_filter/loop_spec declarative and composable (a script may
// build and reuse them as values).
func specBuiltin(name string) func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
	return func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) > 0 {
			return nil, fmt.Errorf("%s: unexpected positional arguments", name)
		}
		d := new(starlark.Dict)
		for _, kv := range kwargs {
			if err := d.SetKey(kv[0], kv[1]); err != nil {
				return nil, err
			}
		}
		return d, nil
	}
}

func (c *collector) agentBuiltin(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, extends, description, profile string
	var promptV, toolsV, loopV starlark.Value
	var skillsV, delegatesV *starlark.List
	var workflows bool
	if err := starlark.UnpackArgs("agent", args, kwargs,
		"name", &name,
		"extends?", &extends,
		"description?", &description,
		"profile?", &profile,
		"prompt?", &promptV,
		"tools?", &toolsV,
		"skills?", &skillsV,
		"delegates?", &delegatesV,
		"workflows?", &workflows,
		"loop?", &loopV,
	); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("agent: name is required")
	}
	d := &Definition{
		Name: name, Extends: extends, Description: description, Profile: profile,
		Workflows: workflows, Source: c.file,
		Tools: model.FilterSpec{Inherit: true}, // normalised; see package doc
	}
	var err error
	if d.Prompt, err = c.decodePrompt(name, promptV); err != nil {
		return nil, err
	}
	if toolsV != nil {
		if d.Tools, err = decodeTools(name, toolsV); err != nil {
			return nil, err
		}
	}
	if d.Skills, err = strList("skills", skillsV); err != nil {
		return nil, err
	}
	if d.Delegates, err = strList("delegates", delegatesV); err != nil {
		return nil, err
	}
	if loopV != nil {
		if d.Loop, err = c.decodeLoop(name, loopV); err != nil {
			return nil, err
		}
	}
	c.defs = append(c.defs, d)
	return starlark.None, nil
}

// decodePrompt accepts a string (one anonymous fragment), a dict of
// name -> text (appended in insertion order), or a list whose entries
// are strings or dicts: {"file": "role.md"}, {"name": "x", "text": "..."}
// or {"x": "..."} to append, {"replace": "x", "text"/"file": ...} to
// substitute, {"remove": "x"} to delete. File contents are read now,
// relative to the declaring file, from its bundle.
func (c *collector) decodePrompt(agent string, v starlark.Value) ([]PromptEntry, error) {
	if v == nil {
		return nil, nil
	}
	switch p := v.(type) {
	case starlark.String:
		return []PromptEntry{{Text: string(p)}}, nil
	case *starlark.Dict:
		var out []PromptEntry
		for _, kv := range p.Items() {
			name, ok := kv[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("agent %s: prompt keys must be strings", agent)
			}
			text, ok := kv[1].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("agent %s: prompt %q must be a string", agent, string(name))
			}
			out = append(out, PromptEntry{Name: string(name), Text: string(text)})
		}
		return out, nil
	case *starlark.List:
		var out []PromptEntry
		iter := p.Iterate()
		defer iter.Done()
		var item starlark.Value
		for i := 0; iter.Next(&item); i++ {
			e, err := c.decodePromptEntry(agent, i, item)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("agent %s: prompt must be a string, dict or list, got %s", agent, v.Type())
	}
}

func (c *collector) decodePromptEntry(agent string, i int, v starlark.Value) (PromptEntry, error) {
	if s, ok := v.(starlark.String); ok {
		return PromptEntry{Text: string(s)}, nil
	}
	d, ok := v.(*starlark.Dict)
	if !ok {
		return PromptEntry{}, fmt.Errorf("agent %s: prompt entry %d must be a string or dict, got %s", agent, i, v.Type())
	}
	get := func(key string) (string, bool, error) {
		val, found, err := d.Get(starlark.String(key))
		if err != nil || !found {
			return "", found, err
		}
		s, ok := val.(starlark.String)
		if !ok {
			return "", true, fmt.Errorf("agent %s: prompt entry %d: %q must be a string", agent, i, key)
		}
		return string(s), true, nil
	}
	var e PromptEntry
	var err error
	if e.Remove, _, err = get("remove"); err != nil {
		return e, err
	}
	if e.Replace, _, err = get("replace"); err != nil {
		return e, err
	}
	if e.File, _, err = get("file"); err != nil {
		return e, err
	}
	if e.Name, _, err = get("name"); err != nil {
		return e, err
	}
	if e.Text, _, err = get("text"); err != nil {
		return e, err
	}
	if e.Remove == "" && e.Replace == "" && e.File == "" && e.Name == "" && e.Text == "" {
		// Single-key shorthand {"fragment": "text"}.
		if d.Len() == 1 {
			kv := d.Items()[0]
			name, nok := kv[0].(starlark.String)
			text, tok := kv[1].(starlark.String)
			if nok && tok {
				return PromptEntry{Name: string(name), Text: string(text)}, nil
			}
		}
		return e, fmt.Errorf("agent %s: prompt entry %d is empty", agent, i)
	}
	if e.File != "" {
		b, err := c.read(joinPath(c.dir, e.File))
		if err != nil {
			return e, fmt.Errorf("agent %s: prompt file %q: %w", agent, e.File, err)
		}
		e.Text = string(b)
	}
	return e, nil
}

func decodeTools(agent string, v starlark.Value) (model.FilterSpec, error) {
	f := model.FilterSpec{Inherit: true}
	d, ok := v.(*starlark.Dict)
	if !ok {
		return f, fmt.Errorf("agent %s: tools must be a tools_filter(...) dict, got %s", agent, v.Type())
	}
	for _, kv := range d.Items() {
		key, ok := kv[0].(starlark.String)
		if !ok {
			return f, fmt.Errorf("agent %s: tools keys must be strings", agent)
		}
		var err error
		switch string(key) {
		case "include":
			f.Include, err = valueStrList("tools.include", kv[1])
		case "exclude":
			f.Exclude, err = valueStrList("tools.exclude", kv[1])
		case "allow":
			f.Allow, err = valueStrList("tools.allow", kv[1])
		case "max_effect":
			s, ok := kv[1].(starlark.String)
			if !ok {
				return f, fmt.Errorf("agent %s: tools.max_effect must be a string", agent)
			}
			f.MaxEffect = string(s)
		case "inherit":
			b, ok := kv[1].(starlark.Bool)
			if !ok {
				return f, fmt.Errorf("agent %s: tools.inherit must be a bool", agent)
			}
			f.Inherit = bool(b)
		default:
			return f, fmt.Errorf("agent %s: unknown tools key %q", agent, string(key))
		}
		if err != nil {
			return f, fmt.Errorf("agent %s: %w", agent, err)
		}
	}
	if err := ValidateMaxEffect(agent, f.MaxEffect); err != nil {
		return f, err
	}
	return f, nil
}

func (c *collector) decodeLoop(agent string, v starlark.Value) (LoopSpec, error) {
	var l LoopSpec
	d, ok := v.(*starlark.Dict)
	if !ok {
		return l, fmt.Errorf("agent %s: loop must be a loop_spec(...) dict, got %s", agent, v.Type())
	}
	for _, kv := range d.Items() {
		key, ok := kv[0].(starlark.String)
		if !ok {
			return l, fmt.Errorf("agent %s: loop keys must be strings", agent)
		}
		val := kv[1]
		var err error
		switch string(key) {
		case "max_steps":
			l.MaxSteps, err = intPtr("loop.max_steps", val)
		case "completion":
			l.Completion, err = strPtr("loop.completion", val)
			if err == nil && *l.Completion != "text" && *l.Completion != "structured" {
				return l, fmt.Errorf("agentdef: %s: loop.completion %q, want text|structured", agent, *l.Completion)
			}
		case "output":
			l.Output, err = c.outputSchema(agent, val)
		case "tool_result_bytes":
			l.ToolResultBytes, err = sizePtr("loop.tool_result_bytes", val)
		case "wait_jobs":
			l.WaitJobs, err = durationPtr("loop.wait_jobs", val)
		case "compact_at":
			l.CompactAt, err = floatPtr("loop.compact_at", val)
		case "keep_recent_tokens":
			l.KeepRecentTokens, err = intPtr("loop.keep_recent_tokens", val)
		case "summary_tokens":
			l.SummaryTokens, err = intPtr("loop.summary_tokens", val)
		case "progressive_tools":
			l.ProgressiveTools, err = boolPtr("loop.progressive_tools", val)
		case "clear_results_at":
			l.ClearResultsAt, err = floatPtr("loop.clear_results_at", val)
		case "keep_recent_results":
			l.KeepRecentResults, err = intPtr("loop.keep_recent_results", val)
		default:
			return l, fmt.Errorf("agent %s: unknown loop key %q", agent, string(key))
		}
		if err != nil {
			return l, fmt.Errorf("agent %s: %w", agent, err)
		}
	}
	return l, nil
}

// outputSchema converts a schema.* value, a dict, an inline JSON string
// or a schema filename (relative to the declaring file) to JSON.
func (c *collector) outputSchema(agent string, v starlark.Value) (*string, error) {
	if s, ok := v.(starlark.String); ok {
		str := string(s)
		if len(str) > 0 && str[0] == '{' {
			if !json.Valid([]byte(str)) {
				return nil, fmt.Errorf("agentdef: %s: loop.output is not valid JSON", agent)
			}
			return &str, nil
		}
		b, err := c.read(joinPath(c.dir, str))
		if err != nil {
			return nil, fmt.Errorf("agentdef: %s: loop.output %q: %w", agent, str, err)
		}
		if !json.Valid(b) {
			return nil, fmt.Errorf("agentdef: %s: loop.output %q is not valid JSON", agent, str)
		}
		out := string(b)
		return &out, nil
	}
	sch, err := scriptschema.Of(v)
	if err != nil {
		return nil, fmt.Errorf("agentdef: %s: loop.output: %w", agent, err)
	}
	b, err := json.Marshal(sch)
	if err != nil {
		return nil, fmt.Errorf("agentdef: %s: loop.output: %w", agent, err)
	}
	out := string(b)
	return &out, nil
}

func strList(field string, l *starlark.List) ([]string, error) {
	if l == nil {
		return nil, nil
	}
	return valueStrList(field, l)
}

func valueStrList(field string, v starlark.Value) ([]string, error) {
	seq, ok := v.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of strings, got %s", field, v.Type())
	}
	iter := seq.Iterate()
	defer iter.Done()
	var out []string
	var item starlark.Value
	for iter.Next(&item) {
		s, ok := item.(starlark.String)
		if !ok {
			return nil, fmt.Errorf("%s must be a list of strings, got %s element", field, item.Type())
		}
		out = append(out, string(s))
	}
	return out, nil
}

func intPtr(field string, v starlark.Value) (*int, error) {
	i, ok := v.(starlark.Int)
	if !ok {
		return nil, fmt.Errorf("%s must be an int, got %s", field, v.Type())
	}
	n, ok2 := i.Int64()
	if !ok2 {
		return nil, fmt.Errorf("%s is out of range", field)
	}
	out := int(n)
	return &out, nil
}

func floatPtr(field string, v starlark.Value) (*float64, error) {
	switch n := v.(type) {
	case starlark.Float:
		out := float64(n)
		return &out, nil
	case starlark.Int:
		i, ok := n.Int64()
		if !ok {
			return nil, fmt.Errorf("%s is out of range", field)
		}
		f := float64(i)
		return &f, nil
	default:
		return nil, fmt.Errorf("%s must be a number, got %s", field, v.Type())
	}
}

func strPtr(field string, v starlark.Value) (*string, error) {
	s, ok := v.(starlark.String)
	if !ok {
		return nil, fmt.Errorf("%s must be a string, got %s", field, v.Type())
	}
	out := string(s)
	return &out, nil
}

func boolPtr(field string, v starlark.Value) (*bool, error) {
	b, ok := v.(starlark.Bool)
	if !ok {
		return nil, fmt.Errorf("%s must be a bool, got %s", field, v.Type())
	}
	out := bool(b)
	return &out, nil
}

func sizePtr(field string, v starlark.Value) (*int, error) {
	switch n := v.(type) {
	case starlark.Int:
		return intPtr(field, v)
	case starlark.String:
		sz, err := config.ParseSize(string(n))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		return &sz, nil
	default:
		return nil, fmt.Errorf("%s must be an int or size string, got %s", field, v.Type())
	}
}

func durationPtr(field string, v starlark.Value) (*time.Duration, error) {
	switch n := v.(type) {
	case starlark.String:
		d, err := time.ParseDuration(string(n))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		return &d, nil
	case starlark.Int:
		secs, ok := n.Int64()
		if !ok {
			return nil, fmt.Errorf("%s is out of range", field)
		}
		d := time.Duration(secs) * time.Second
		return &d, nil
	case starlark.Float:
		d := time.Duration(float64(n) * float64(time.Second))
		return &d, nil
	default:
		return nil, fmt.Errorf("%s must be a duration string or seconds, got %s", field, v.Type())
	}
}

// joinPath resolves name against the declaring file's directory. A path
// that leaves the bundle fails to read: bundle files are only served under
// their own root.
func joinPath(dir, name string) string {
	return path.Join(dir, name)
}
