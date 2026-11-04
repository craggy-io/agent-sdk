package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"go.starlark.net/starlark"

	"github.com/craggy-io/agent-sdk/config"
	scriptschema "github.com/craggy-io/script-services/schema"
)

// manifest is the parsed WORKFLOW = workflow(...) value.
type manifest struct {
	Description string
	// Schema validates run() input: the manifest's input dict converted
	// with schema.Of (a closed object schema). Empty accepts anything.
	Schema   json.RawMessage
	Fields   []string          // sorted input field names, for traces
	Agents   []string          // allowlist globs (empty = all)
	Profiles map[string]string // profile remap
	Limits   *config.LimitsPatch
}

func (m *manifest) json() map[string]any {
	fields := append([]string(nil), m.Fields...)
	sort.Strings(fields)
	return map[string]any{
		"description": m.Description, "input_fields": fields,
		"agents": m.Agents, "profiles": m.Profiles, "limits": m.Limits,
		"input_schema": json.RawMessage(m.inputSchema()),
	}
}

// inputSchema is the JSON Schema validating run() input.
func (m *manifest) inputSchema() json.RawMessage {
	if len(m.Schema) == 0 {
		return json.RawMessage(`{}`)
	}
	return m.Schema
}

// schemaJSON converts a script schema argument (schema.* value, field
// dict, raw JSON Schema dict or string, or None) to a JSON Schema
// document, using the shared script-services schema conversion.
func schemaJSON(v starlark.Value) (json.RawMessage, error) {
	if v == nil || v == starlark.None {
		return json.RawMessage(`{}`), nil
	}
	s, err := scriptschema.Of(v)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return json.RawMessage(`{}`), nil
	}
	return json.Marshal(s)
}

// layer converts the manifest to a §3.2 config layer, or nil when it
// sets nothing. Limits can only tighten (config.Resolve minimum rule).
func (m *manifest) layer(name string) *config.Layer {
	if len(m.Profiles) == 0 && m.Limits == nil {
		return nil
	}
	return &config.Layer{Name: name, ProfileRemap: m.Profiles, Limits: m.Limits}
}

type manifestValue struct{ man *manifest }

func (v *manifestValue) String() string        { return fmt.Sprintf("workflow(%q)", v.man.Description) }
func (v *manifestValue) Type() string          { return "workflow" }
func (v *manifestValue) Freeze()               {}
func (v *manifestValue) Truth() starlark.Bool  { return starlark.True }
func (v *manifestValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: workflow") }

// workflowBuiltin implements workflow(description, input, agents,
// profiles, limits) at file scope (§6.1).
func workflowBuiltin(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var desc string
	var inputV, agentsV, profilesV, limitsV starlark.Value
	if err := starlark.UnpackArgs("workflow", args, kwargs,
		"description?", &desc, "input?", &inputV, "agents?", &agentsV,
		"profiles?", &profilesV, "limits?", &limitsV); err != nil {
		return nil, err
	}
	m := &manifest{Description: desc}
	if inputV != nil && inputV != starlark.None {
		d, ok := inputV.(*starlark.Dict)
		if !ok {
			return nil, fmt.Errorf("workflow: input must be a dict of field: schema.*")
		}
		for _, pair := range d.Items() {
			k, ok := pair[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("workflow: input keys must be strings")
			}
			m.Fields = append(m.Fields, string(k))
		}
		sch, err := schemaJSON(inputV)
		if err != nil {
			return nil, fmt.Errorf("workflow: input: %w", err)
		}
		m.Schema = sch
	}
	agents, err := strSlice(agentsV)
	if err != nil {
		return nil, fmt.Errorf("workflow: agents: %w", err)
	}
	m.Agents = agents
	remap, err := strMap(profilesV)
	if err != nil {
		return nil, fmt.Errorf("workflow: profiles: %w", err)
	}
	m.Profiles = remap
	if limitsV != nil && limitsV != starlark.None {
		lm, err := dictAny(limitsV)
		if err != nil {
			return nil, fmt.Errorf("workflow: limits: %w", err)
		}
		p, err := limitsPatch(lm)
		if err != nil {
			return nil, err
		}
		m.Limits = p
	}
	return &manifestValue{man: m}, nil
}

func limitsPatch(m map[string]any) (*config.LimitsPatch, error) {
	p := &config.LimitsPatch{}
	for k, v := range m {
		switch k {
		case "usd":
			f, ok := anyFloat(v)
			if !ok {
				return nil, fmt.Errorf("limits.usd: want number")
			}
			p.USD = &f
		case "wall_clock":
			if s, ok := v.(string); ok {
				d, err := time.ParseDuration(s)
				if err != nil {
					return nil, fmt.Errorf("limits.wall_clock: %w", err)
				}
				p.WallClock = &d
			} else if f, ok := anyFloat(v); ok {
				d := time.Duration(f * float64(time.Second))
				p.WallClock = &d
			} else {
				return nil, fmt.Errorf("limits.wall_clock: want duration or seconds")
			}
		case "steps", "tool_calls", "depth", "concurrency":
			n, ok := anyInt(v)
			if !ok {
				return nil, fmt.Errorf("limits.%s: want int", k)
			}
			switch k {
			case "steps":
				p.Steps = &n
			case "tool_calls":
				p.ToolCalls = &n
			case "depth":
				p.Depth = &n
			case "concurrency":
				p.Concurrency = &n
			}
		default:
			return nil, fmt.Errorf("limits: unknown key %q", k)
		}
	}
	return p, nil
}

// nsValue is an attribute namespace (schema, json, tools trees).
type nsValue struct {
	name     string
	children map[string]starlark.Value
}

func (n *nsValue) String() string       { return n.name }
func (n *nsValue) Type() string         { return n.name }
func (n *nsValue) Freeze()              {}
func (n *nsValue) Truth() starlark.Bool { return starlark.True }
func (n *nsValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable: %s", n.name)
}
func (n *nsValue) AttrNames() []string {
	out := make([]string, 0, len(n.children))
	for k := range n.children {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func (n *nsValue) Attr(name string) (starlark.Value, error) {
	v, ok := n.children[name]
	if !ok {
		return nil, nil
	}
	return v, nil
}

var _ starlark.HasAttrs = (*nsValue)(nil)
