package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"

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
	last        host.Result
	err         error
	usage       model.Usage
	steps       int
	calls       int
}

// installedChat binds all role resolutions to the same installed revision.
func installedChat(h *host.Host, wf *agentdef.Workflow, thread, digest string) (*chatTurn, error) {
	if !wf.Chat || h.Defs() == nil {
		return nil, fmt.Errorf("chat workflow %q is no longer installed", wf.Name)
	}
	catalog := h.Defs().Catalog()
	def, ok := bundle.Lookup(catalog, agentdef.WorkflowKind, wf.Name)
	if !ok || def.Value.Unit != wf.Unit || digest != "" && catalog.Digest() != digest {
		return nil, fmt.Errorf("chat workflow %q or its installed definitions changed", wf.Name)
	}
	return &chatTurn{catalog: catalog, owner: def.Owner.ID, spec: host.ExecSpec{Thread: thread}}, nil
}

// A chat workflow orchestrates agent calls. Its roles share the turn's
// budget but each role keeps its own thread across messages.
func runChat(ctx context.Context, h *host.Host, name string, spec host.ExecSpec, invoke host.NodeRunner) (host.Result, error) {
	if h.Defs() == nil {
		return host.Result{}, fmt.Errorf("chat: no definition loader")
	}
	_, _ = h.Defs().Reload(ctx)
	t := &chatTurn{interactive: true, spec: spec, catalog: h.Defs().Catalog()}
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
	out, err := executeScoped(ctx, h, wf, src, input, Options{chat: t}, invoke)
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

func chatAgent(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, task, phase string
	if err := starlark.UnpackArgs("agent", args, kwargs, "name", &name, "task", &task, "phase?", &phase); err != nil {
		return nil, err
	}
	return e.agentJournaled(&specValue{name: name, task: task, phase: phase})
}

func (t *chatTurn) run(ctx context.Context, sv *specValue, invoke host.NodeRunner, layers []config.Layer) map[string]any {
	spec := t.spec
	spec.Task = sv.task
	spec.Layers = append(slices.Clone(spec.Layers), layers...)
	if t.err != nil {
		return agentResultJSON(t.last)
	}
	if t.owner != "" {
		role, ok := bundle.Lookup(t.catalog, agentdef.AgentKind, sv.name)
		if !ok || role.Owner.ID != t.owner {
			t.err = fmt.Errorf("workflow %s does not own chat role %q", t.owner, sv.name)
			return agentResultJSON(t.last)
		}
		spec.Name, spec.Def, spec.LocalBehavior = sv.name, nil, true
		spec.Thread = fmt.Sprintf("chat:%d:%s:%s", len(spec.Thread), spec.Thread, sv.name)
	}
	spec.DefinitionCatalog = t.catalog
	t.calls++
	t.last, t.err = invoke(ctx, spec)
	t.usage.Add(t.last.Usage)
	t.steps += t.last.Steps
	return agentResultJSON(t.last)
}
