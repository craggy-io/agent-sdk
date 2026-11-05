// Package agentcap registers the agent host as capability-services
// capabilities: agents.*, workflows.*, sessions.*, recovery.*,
// trace.*, llm.* and memory.*. Register once and every projection
// reaches the same surface — Starlark scripts (nested built-ins, via
// starlarkcap), agent tool loops (tools.Defs over a view), MCP and
// HTTP. A host picks what an agent may call with registry views;
// definition tool filters narrow further per agent, and authority only
// ever narrows.
//
// Runtime registration is two-step by design: agents.define and
// workflows.define add session-scoped Starlark definitions; promote
// installs them in the host loader's user layer (agentdef.Loader
// InstallAgent/InstallWorkflow) only when the host's Approve hook
// accepts the exact bytes. Agents can propose definitions; they cannot
// approve them. workflows.pending/answer carry the "host" tag: keep
// them out of agent views.
package agentcap

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/memory"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/capability-services/registry"
)

// Options configures Register.
type Options struct {
	// Approve gates promotion: called with kind ("agent"|"workflow"),
	// the name, and the exact bytes that would be written. No Approve,
	// no promotion. Promotion writes into the user layer of the host's
	// loader (agentdef.Options.UserDir); without one it is unavailable.
	Approve func(ctx context.Context, kind, name string, payload []byte) bool
	// Memory backs the memory.* capabilities. Nil uses an in-memory
	// store scoped to this registration.
	Memory memory.Store
}

type service struct {
	h    *host.Host
	opts Options
	mem  memory.Store

	mu    sync.Mutex
	dyn   map[string]*dynAgent // session-scoped dynamic definitions
	dynWf map[string]string    // session-scoped dynamic workflow sources
}

type dynAgent struct {
	def    *agentdef.Definition
	source string // Starlark source when defined from source; "" for JSON defines
}

// Register adds the agent capabilities and their groups to r, bound to
// h. It is the only wiring a host needs beyond choosing views.
func Register(r *registry.Registry, h *host.Host, opts Options) error {
	s := &service{h: h, opts: opts, mem: opts.Memory,
		dyn: map[string]*dynAgent{}, dynWf: map[string]string{}}
	if s.mem == nil {
		s.mem = memory.NewMemStore()
	}
	if err := r.AddGroup(
		registry.Group{Name: "agents", Description: "Agent definitions and agent handles.", LoadAlways: true},
		registry.Group{Name: "workflows", Description: "Durable Starlark workflows."},
		registry.Group{Name: "sessions", Description: "Thread input delivery."},
		registry.Group{Name: "recovery", Description: "Workflow operation recovery."},
		registry.Group{Name: "trace", Description: "Run trace queries and usage."},
		registry.Group{Name: "llm", Description: "Direct model sampling through the host provider."},
		registry.Group{Name: "memory", Description: "Durable namespaced memory.", LoadAlways: true},
	); err != nil {
		return err
	}
	caps := append(s.agentCaps(), s.workflowCaps()...)
	caps = append(caps, s.traceCaps()...)
	caps = append(caps, s.llmCaps()...)
	caps = append(caps, s.memoryCaps()...)
	return r.Add(caps...)
}

// coded maps host errors onto registry codes: "unknown ..." failures
// are not_found, the rest keep their message under internal unless
// already coded.
func coded(err error) error {
	if err == nil {
		return nil
	}
	if registry.CodeOf(err) != "" {
		return err
	}
	if strings.Contains(err.Error(), "unknown") || strings.Contains(err.Error(), "not found") {
		return registry.WrapCode(registry.NotFound, err)
	}
	return registry.WrapCode(registry.Internal, err)
}

func invalid(format string, args ...any) error {
	return registry.Errorf(registry.Invalid, format, args...)
}

// --- agents & sessions -------------------------------------------------------

type nameIn struct {
	Name string `json:"name"`
}

type agentRunIn struct {
	Name       string          `json:"name,omitempty"`
	Definition json.RawMessage `json:"definition,omitempty"`
	Source     string          `json:"source,omitempty"`
	Task       string          `json:"task"`
	Profile    string          `json:"profile,omitempty"`
	Thread     string          `json:"thread,omitempty"`
	Vars       map[string]any  `json:"vars,omitempty"`
	Tools      json.RawMessage `json:"tools,omitempty"`
}

type agentRunOut struct {
	Status     string          `json:"status"`
	Text       string          `json:"text,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Steps      int             `json:"steps"`
	RunID      string          `json:"run_id"`
	NodeID     string          `json:"node_id,omitempty"`
	HandleID   string          `json:"handle_id,omitempty"`
	Def        string          `json:"def,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      model.Usage     `json:"usage"`
	Error      string          `json:"error,omitempty"`
}

func runOut(r host.Result, err error) agentRunOut {
	o := agentRunOut{
		Status: r.Status, Text: r.Text, Steps: r.Steps, RunID: r.RunID,
		NodeID: r.NodeID, HandleID: r.HandleID, Def: r.DefName,
		StopReason: r.StopReason, Usage: r.Usage,
	}
	if len(r.Output) > 0 {
		o.Output = r.Output
	}
	if err != nil {
		o.Error = err.Error()
	}
	return o
}

type handleIn struct {
	Handle string `json:"handle"`
}

type sendIn struct {
	Handle             string `json:"handle,omitempty"`
	Thread             string `json:"thread,omitempty"`
	Text               string `json:"text"`
	Mode               string `json:"mode,omitempty" jsonschema:"message|steer|follow_up"`
	RequestID          string `json:"request_id,omitempty"`
	ExpectedActivation string `json:"expected_activation,omitempty"`
	Interrupt          bool   `json:"interrupt,omitempty"`
}

type waitIn struct {
	Handles   []string `json:"handles,omitempty"`
	Handle    string   `json:"handle,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
}

type defineIn struct {
	Definition json.RawMessage `json:"definition,omitempty"`
	Source     string          `json:"source,omitempty"`
}

func (s *service) execSpec(ctx context.Context, in *agentRunIn) (host.ExecSpec, error) {
	spec := host.ExecSpec{Task: in.Task, Profile: in.Profile, Thread: in.Thread, Vars: in.Vars}
	switch {
	case in.Source != "":
		d, err := agentdef.ParseSource(ctx, []byte(in.Source))
		if err != nil {
			return spec, invalid("source: %v", err)
		}
		spec.Def = d
	case len(in.Definition) > 0:
		d, err := definitionFromJSON(in.Definition)
		if err != nil {
			return spec, invalid("%v", err)
		}
		spec.Def = d
	case in.Name != "":
		s.mu.Lock()
		dyn, ok := s.dyn[in.Name]
		s.mu.Unlock()
		if ok {
			cp := *dyn.def
			spec.Def = &cp
		} else {
			spec.Name = in.Name
		}
	default:
		return spec, invalid("name, definition or source is required")
	}
	if len(in.Tools) > 0 {
		f, err := filterFromJSON(in.Tools)
		if err != nil {
			return spec, invalid("%v", err)
		}
		spec.Tools = f
	}
	return spec, nil
}

// definitionFromJSON decodes a Definition from its JSON shape, also
// accepting inline prompt text under "prompt_text".
func definitionFromJSON(raw json.RawMessage) (*agentdef.Definition, error) {
	var d agentdef.Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("definition: %w", err)
	}
	var extra struct {
		PromptText string `json:"prompt_text"`
	}
	_ = json.Unmarshal(raw, &extra)
	if extra.PromptText != "" {
		d.Prompt = append(d.Prompt, agentdef.PromptEntry{Text: extra.PromptText})
	}
	return &d, nil
}

// filterFromJSON parses a tools filter: a list of include globs or a
// FilterSpec object. It can only narrow (the host intersects).
func filterFromJSON(raw json.RawMessage) (*model.FilterSpec, error) {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return &model.FilterSpec{Include: list, Inherit: true}, nil
	}
	var f model.FilterSpec
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("tools filter: %w", err)
	}
	return &f, nil
}

type agentListEntry struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Profile     string            `json:"profile,omitempty"`
	Provenance  map[string]string `json:"provenance,omitempty"`
	Dynamic     bool              `json:"dynamic,omitempty"`
	Error       string            `json:"error,omitempty"`
}

func (s *service) agentCaps() []registry.Capability {
	ro := registry.WithEffects(registry.ReadOnly)
	return []registry.Capability{
		registry.Func("agents.list", "List agent definitions",
			func(ctx context.Context, _ struct{}) ([]agentListEntry, error) {
				out := []agentListEntry{}
				if s.h.Defs() != nil {
					_, _ = s.h.Defs().Reload(ctx) // a failed reload keeps the previous catalogue
					for _, n := range s.h.Defs().Names() {
						e := agentListEntry{Name: n}
						if r, err := s.h.Defs().Resolve(n, nil); err == nil {
							e.Description, e.Profile, e.Provenance = r.Description, r.Profile, r.Provenance
						} else {
							e.Error = err.Error()
						}
						out = append(out, e)
					}
				}
				s.mu.Lock()
				for n, d := range s.dyn {
					out = append(out, agentListEntry{Name: n, Description: d.def.Description, Profile: d.def.Profile, Dynamic: true})
				}
				s.mu.Unlock()
				sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
				return out, nil
			}, ro, registry.Describe("List agent definitions (resolved, with provenance), including session-scoped dynamic ones.")),
		registry.Func("agents.get", "Get one resolved definition",
			func(_ context.Context, in struct {
				Name string         `json:"name"`
				Vars map[string]any `json:"vars,omitempty"`
			}) (json.RawMessage, error) {
				s.mu.Lock()
				dyn, isDyn := s.dyn[in.Name]
				s.mu.Unlock()
				if s.h.Defs() == nil {
					return nil, registry.Errorf(registry.Unavailable, "no definition loader")
				}
				var r *agentdef.Resolved
				var err error
				if isDyn {
					r, err = s.h.Defs().ResolveDynamic(*dyn.def, in.Vars)
				} else {
					r, err = s.h.Defs().Resolve(in.Name, in.Vars)
				}
				if err != nil {
					return nil, coded(err)
				}
				return json.Marshal(r)
			}, ro),
		registry.Func("agents.run", "Run an agent to completion",
			func(ctx context.Context, in agentRunIn) (agentRunOut, error) {
				spec, err := s.execSpec(ctx, &in)
				if err != nil {
					return agentRunOut{}, err
				}
				res, err := s.h.ExecAgent(ctx, spec)
				return runOut(res, err), nil
			}, registry.AsLong(0), registry.Describe("Run a definition (by name, JSON definition, or Starlark source) on a task to completion.")),
		registry.Func("agents.spawn", "Spawn a reusable agent handle",
			func(ctx context.Context, in agentRunIn) (host.HandleInfo, error) {
				spec, err := s.execSpec(ctx, &in)
				if err != nil {
					return host.HandleInfo{}, err
				}
				inf, err := s.h.Spawn(ctx, host.SpawnSpec{ExecSpec: spec})
				return inf, coded(err)
			}),
		registry.Func("agents.send", "Send input to a handle",
			func(ctx context.Context, in sendIn) (host.Receipt, error) {
				r, _ := s.h.SendInput(ctx, host.SendRequest{
					Target: in.Handle, Text: in.Text, Mode: in.Mode,
					RequestID: in.RequestID, ExpectedActivation: in.ExpectedActivation,
					Interrupt: in.Interrupt,
				})
				return r, nil // the receipt carries acceptance or rejection
			}),
		registry.Func("agents.interrupt", "Interrupt a handle's current activation",
			func(ctx context.Context, in handleIn) (host.HandleInfo, error) {
				inf, err := s.h.Interrupt(ctx, in.Handle)
				return inf, coded(err)
			}),
		registry.Func("agents.resume", "Reopen a closed handle as idle",
			func(ctx context.Context, in handleIn) (host.HandleInfo, error) {
				inf, err := s.h.Resume(ctx, in.Handle)
				return inf, coded(err)
			}),
		registry.Func("agents.close", "Close a handle (history retained)",
			func(ctx context.Context, in handleIn) (host.HandleInfo, error) {
				inf, err := s.h.CloseHandle(ctx, in.Handle)
				return inf, coded(err)
			}),
		registry.Func("agents.status", "One handle's state and latest result",
			func(ctx context.Context, in handleIn) (host.HandleInfo, error) {
				inf, err := s.h.Status(ctx, in.Handle)
				return inf, coded(err)
			}, ro),
		registry.Func("agents.handles", "Handles visible to the caller",
			func(ctx context.Context, _ struct{}) ([]host.HandleInfo, error) {
				return s.h.ListAgents(ctx), nil
			}, ro),
		registry.Func("agents.wait", "Wait for handles to settle",
			func(ctx context.Context, in waitIn) ([]host.WaitResult, error) {
				ids := in.Handles
				if len(ids) == 0 && in.Handle != "" {
					ids = []string{in.Handle}
				}
				to := time.Duration(in.TimeoutMS) * time.Millisecond
				if to <= 0 {
					to = 30 * time.Second
				}
				res, _ := s.h.Wait(ctx, ids, to) // timeout never cancels; results say so
				return res, nil
			}, ro),
		registry.Func("agents.define", "Define a session-scoped agent",
			func(ctx context.Context, in defineIn) (json.RawMessage, error) {
				return s.defineAgent(ctx, in)
			}, registry.Describe("Create a dynamic definition for this session from Starlark source or a JSON definition. It must extend an existing definition.")),
		registry.Func("agents.promote", "Install a dynamic definition in the user layer",
			func(ctx context.Context, in nameIn) (map[string]any, error) {
				s.mu.Lock()
				d, ok := s.dyn[in.Name]
				s.mu.Unlock()
				if !ok {
					return nil, registry.Errorf(registry.NotFound, "no dynamic definition %q to promote", in.Name)
				}
				p, err := s.promoteAgent(ctx, d)
				if err != nil {
					return nil, err
				}
				return map[string]any{"promoted": true, "name": in.Name, "path": p}, nil
			}, registry.WithEffects(registry.Destructive), registry.WithTags("admin")),
		registry.Func("sessions.input", "Send input to a thread",
			func(ctx context.Context, in sendIn) (host.Receipt, error) {
				target := in.Thread
				if target == "" {
					target = in.Handle
				}
				r, _ := s.h.SendInput(ctx, host.SendRequest{
					Target: target, Text: in.Text, Mode: in.Mode,
					RequestID: in.RequestID, ExpectedActivation: in.ExpectedActivation,
				})
				return r, nil
			}),
		registry.Func("sessions.inputs", "Input receipts for a thread or handle",
			func(_ context.Context, in struct {
				Target string `json:"target,omitempty"`
				Thread string `json:"thread,omitempty"`
			}) ([]host.Receipt, error) {
				t := in.Target
				if t == "" {
					t = in.Thread
				}
				return s.h.Inputs(t), nil
			}, ro),
	}
}

// defineAgent validates and stores a dynamic definition: Starlark
// source declaring exactly one agent, or a JSON definition. It returns
// the resolved form.
func (s *service) defineAgent(ctx context.Context, in defineIn) (json.RawMessage, error) {
	if s.h.Defs() == nil {
		return nil, registry.Errorf(registry.Unavailable, "no definition loader")
	}
	var d *agentdef.Definition
	switch {
	case in.Source != "":
		parsed, err := agentdef.ParseSource(ctx, []byte(in.Source))
		if err != nil {
			return nil, invalid("source: %v", err)
		}
		d = parsed
	case len(in.Definition) > 0:
		parsed, err := definitionFromJSON(in.Definition)
		if err != nil {
			return nil, invalid("%v", err)
		}
		d = parsed
	default:
		return nil, invalid("source or definition is required")
	}
	r, err := s.h.Defs().ResolveDynamic(*d, nil) // must extend a catalogued def
	if err != nil {
		return nil, coded(err)
	}
	s.mu.Lock()
	s.dyn[d.Name] = &dynAgent{def: d, source: in.Source}
	s.mu.Unlock()
	return json.Marshal(r)
}
