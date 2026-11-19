package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/defs"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/script-services/script"
	"go.starlark.net/starlark"
)

type chatTurn struct {
	interactive bool
	spec        host.ExecSpec
	catalog     *bundle.Catalog
	owner       string
	// roster are the agents, from any bundle, a workflow declared
	// roles = "roster" may run this turn. Each agent it runs may call them.
	roster []string

	// mu guards what the turn's agent calls leave behind, which parallel
	// branches write at once.
	mu    sync.Mutex
	last  host.Result
	err   error
	usage model.Usage
	steps int
	calls int
}

// installedChat binds all role resolutions to the same installed revision.
// input is the run's input, which holds a roster workflow's roster.
func installedChat(h *host.Host, wf *agentdef.Workflow, thread, digest string, input map[string]any) (*chatTurn, error) {
	if !wf.Chat || h.Defs() == nil {
		return nil, fmt.Errorf("chat workflow %q is no longer installed", wf.Name)
	}
	catalog := h.Defs().Catalog()
	def, ok := bundle.Lookup(catalog, agentdef.WorkflowKind, wf.Name)
	if !ok || def.Value.Unit != wf.Unit || digest != "" && catalog.Digest() != digest {
		return nil, fmt.Errorf("chat workflow %q or its installed definitions changed", wf.Name)
	}
	t := &chatTurn{catalog: catalog, owner: def.Owner.ID, spec: host.ExecSpec{Thread: thread}}
	if wf.TakesRoster() {
		t.roster = rosterNames(input)
		if _, err := t.rosterInput(); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// rosterNames are the agents a run's input.roster names.
func rosterNames(input map[string]any) []string {
	entries, _ := input["roster"].([]any)
	var names []string
	for _, entry := range entries {
		if m, ok := entry.(map[string]any); ok {
			if name, ok := m["name"].(string); ok && name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// rosterInput is the roster as the workflow reads it, input.roster: each
// agent's name and description, from the turn's catalogue. An agent that
// is no longer installed fails the turn rather than be left out unseen.
func (t *chatTurn) rosterInput() ([]any, error) {
	out := make([]any, 0, len(t.roster))
	for _, name := range t.roster {
		def, ok := bundle.Lookup(t.catalog, agentdef.AgentKind, name)
		if !ok {
			return nil, fmt.Errorf("the roster names %q, and no agent of that name is installed", name)
		}
		out = append(out, map[string]any{"name": name, "description": def.Value.Description})
	}
	return out, nil
}

// A chat workflow orchestrates agent calls. Its roles share the turn's
// budget but each role keeps its own thread across messages.
func runChat(ctx context.Context, h *host.Host, name string, spec host.ExecSpec, invoke host.NodeRunner) (host.Result, error) {
	if h.Defs() == nil {
		return host.Result{}, fmt.Errorf("chat: no definition loader")
	}
	_, _ = h.Defs().Reload(ctx)
	t := &chatTurn{interactive: true, spec: spec, catalog: h.Defs().Catalog()}
	t.spec.Group = nil // each role is given the roster as its group (run)
	wf := &agentdef.Workflow{Name: "agent-turn"}
	src := defs.AgentTurn()
	input := map[string]any{"message": spec.Task}
	if name != "" {
		def, ok := bundle.Lookup(t.catalog, agentdef.WorkflowKind, name)
		if !ok || !def.Value.Chat {
			return host.Result{}, fmt.Errorf("unknown chat workflow %q", name)
		}
		wf, src, t.owner = def.Value, def.Value.Source(), def.Owner.ID
	} else {
		agent := spec.Name
		if spec.Def != nil {
			agent = spec.Def.Name
		}
		input["agent"] = agent
	}
	switch {
	case wf.TakesRoster():
		if len(spec.Group) == 0 {
			return host.Result{}, fmt.Errorf("chat workflow %q runs the agents each turn names, and this turn names none", wf.Name)
		}
		t.roster = slices.Clone(spec.Group)
		roster, err := t.rosterInput()
		if err != nil {
			return host.Result{}, err
		}
		input["roster"] = roster
	case len(spec.Group) > 0:
		return host.Result{}, fmt.Errorf("chat workflow %q does not take a roster", wf.Name)
	}
	out, err := executeScoped(ctx, h, wf, src, input, Options{chat: t}, invoke)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last.Usage, t.last.Steps = t.usage, t.steps
	if t.err != nil {
		err = t.err
	}
	if err != nil {
		if cancelledChat(ctx, err) {
			t.last.Status = StatusCancelled
			t.last.RunID, _ = host.RunIDFromContext(ctx)
			return t.last, nil
		}
		return t.last, err
	}
	value, ok := out.Value.(map[string]any)
	if !ok || t.calls == 0 || value["node"] != t.last.NodeID || value["status"] != t.last.Status {
		return t.last, fmt.Errorf("chat workflow must return its last agent result")
	}
	return t.last, nil
}

// A caller stopping a chat is a terminal status. Preserve distinct faults
// from the trace or provider even if cancellation happened concurrently.
func cancelledChat(ctx context.Context, err error) bool {
	cause := ctx.Err()
	return cause != nil && (errors.Is(err, cause) || script.IsCancellation(err, "context done: "+cause.Error()))
}

// chatArgs reads agent() and spec() in a chat workflow: the agent, its
// task, the phase it is shown under, and the output it must give.
func chatArgs(fn string, args starlark.Tuple, kwargs []starlark.Tuple) (*specValue, error) {
	var name, task, phase string
	var outputV starlark.Value
	if err := starlark.UnpackArgs(fn, args, kwargs, "name", &name, "task", &task, "phase?", &phase, "output?", &outputV); err != nil {
		return nil, err
	}
	sv := &specValue{name: name, task: task, phase: phase}
	if outputV != nil && outputV != starlark.None {
		b, err := schemaJSON(outputV)
		if err != nil {
			return nil, fmt.Errorf("%s: output: %w", fn, err)
		}
		sv.output = b
	}
	return sv, nil
}

func chatAgent(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	sv, err := chatArgs("agent", args, kwargs)
	if err != nil {
		return nil, err
	}
	return e.agentJournaled(sv)
}

func chatSpec(_ *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return chatArgs("spec", args, kwargs)
}

// role requires that the turn may run the agent name: one of the
// workflow's own bundle, or, for a roster workflow, one the roster names.
func (t *chatTurn) role(name string) error {
	if role, ok := bundle.Lookup(t.catalog, agentdef.AgentKind, name); ok && role.Owner.ID == t.owner {
		return nil
	}
	if slices.Contains(t.roster, name) {
		return nil
	}
	if t.roster != nil {
		return fmt.Errorf("workflow %s does not own chat role %q, and this turn's roster does not name it", t.owner, name)
	}
	return fmt.Errorf("workflow %s does not own chat role %q", t.owner, name)
}

func (t *chatTurn) run(ctx context.Context, sv *specValue, invoke host.NodeRunner, layers []config.Layer) map[string]any {
	t.mu.Lock()
	if t.err == nil && t.owner != "" {
		t.err = t.role(sv.name)
	}
	if t.err != nil {
		defer t.mu.Unlock()
		return agentResultJSON(t.last)
	}
	spec := t.spec
	if t.owner != "" {
		// The workflow's own roles are its authored chat loops; a roster
		// agent runs as its bundle wrote it.
		spec.Name, spec.Def, spec.LocalBehavior = sv.name, nil, !slices.Contains(t.roster, sv.name)
		spec.Thread = host.MemberThread(spec.Thread, sv.name)
	}
	t.calls++
	t.mu.Unlock()

	spec.Task = sv.task
	spec.Layers = append(slices.Clone(spec.Layers), layers...)
	spec.DefinitionCatalog = t.catalog
	spec.Group = t.roster
	if len(t.roster) > 0 {
		spec.GroupThread = t.spec.Thread
	}
	if len(sv.output) > 0 {
		spec.Output = append(json.RawMessage(nil), sv.output...)
	}
	res, err := invoke(ctx, spec)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = res
	if err != nil && t.err == nil {
		t.err = err
	}
	t.usage.Add(res.Usage)
	t.steps += res.Steps
	return agentResultJSON(res)
}
