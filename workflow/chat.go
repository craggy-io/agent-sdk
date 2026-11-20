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
	// players are the agents that play each of the workflow's roles this
	// turn, by role, in seat order; group is each of them once. Every
	// agent the turn runs may call the group as subagents.
	players map[string][]string
	group   []string

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
// input is the run's input, which records the turn's cast.
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
	if err := t.castRoles(wf.ChatRoles(catalog, t.owner), castOf(input)); err != nil {
		return nil, err
	}
	return t, nil
}

// castOf is the cast a run's input.cast records, read back on resume.
func castOf(input map[string]any) map[string][]string {
	recorded, _ := input["cast"].(map[string]any)
	cast := map[string][]string{}
	for role, seats := range recorded {
		entries, _ := seats.([]any)
		cast[role] = []string{}
		for _, entry := range entries {
			if m, ok := entry.(map[string]any); ok {
				if name, ok := m["name"].(string); ok && name != "" {
					cast[role] = append(cast[role], name)
				}
			}
		}
	}
	return cast
}

// castRoles settles who plays each role this turn: the agents cast in it,
// or the ones the workflow declares. A role the workflow doesn't have, or
// an agent that isn't installed, fails the turn rather than be left out
// unseen.
func (t *chatTurn) castRoles(roles []agentdef.Role, cast map[string][]string) error {
	for role := range cast {
		if !slices.ContainsFunc(roles, func(r agentdef.Role) bool { return r.Name == role }) {
			return fmt.Errorf("workflow %s has no role %q to cast", t.owner, role)
		}
	}
	t.players, t.group = map[string][]string{}, nil
	for _, role := range roles {
		players, given := cast[role.Name]
		if !given {
			players = t.declaredPlayers(role)
		}
		for _, name := range players {
			if _, ok := bundle.Lookup(t.catalog, agentdef.AgentKind, name); !ok {
				return fmt.Errorf("the cast puts %q in the role %q, and no agent of that name is installed", name, role.Name)
			}
			if !slices.Contains(t.group, name) {
				t.group = append(t.group, name)
			}
		}
		t.players[role.Name] = slices.Clone(players)
	}
	return nil
}

// declaredPlayers are the agents that play role when a turn casts none:
// agentdef.EveryAgent for a role of every seat, else the agent it names.
func (t *chatTurn) declaredPlayers(role agentdef.Role) []string {
	if role.Seats == agentdef.SeatsAll {
		return agentdef.EveryAgent(t.catalog, t.owner)
	}
	if role.Agent != "" {
		return []string{role.Agent}
	}
	return nil
}

// castInput is the cast as the workflow reads it, input.cast: by role,
// each agent in its seats with its name and description.
func (t *chatTurn) castInput() map[string]any {
	out := map[string]any{}
	for role, players := range t.players {
		seats := make([]any, 0, len(players))
		for _, name := range players {
			def, _ := bundle.Lookup(t.catalog, agentdef.AgentKind, name)
			seats = append(seats, map[string]any{"name": name, "description": def.Value.Description})
		}
		out[role] = seats
	}
	return out
}

// A chat workflow orchestrates agent calls. Its roles share the turn's
// budget but each role keeps its own thread across messages.
func runChat(ctx context.Context, h *host.Host, name string, spec host.ExecSpec, invoke host.NodeRunner) (host.Result, error) {
	if h.Defs() == nil {
		return host.Result{}, fmt.Errorf("chat: no definition loader")
	}
	_, _ = h.Defs().Reload(ctx)
	t := &chatTurn{interactive: true, spec: spec, catalog: h.Defs().Catalog()}
	// The group, the cast and the settings are this workflow's: the
	// agents it runs are given the group it casts (run), and don't
	// inherit the rest.
	t.spec.Group, t.spec.Cast, t.spec.Settings = nil, nil, nil
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
		if len(spec.Cast) > 0 || len(spec.Settings) > 0 {
			return host.Result{}, fmt.Errorf("a turn of one agent takes no cast and no settings")
		}
	}
	if wf.Chat {
		if err := t.castRoles(wf.ChatRoles(t.catalog, t.owner), spec.Cast); err != nil {
			return host.Result{}, err
		}
		input["cast"] = t.castInput()
	}
	for field, value := range spec.Settings {
		if field == "message" || field == "cast" {
			return host.Result{}, fmt.Errorf("chat workflow %q: a setting can't be its %s, which each turn gives it", wf.Name, field)
		}
		input[field] = value
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

// player is the agent the workflow runs when it calls name: the first
// agent cast in the role of that name, else an agent cast in any role, or
// one of the workflow's own bundle.
func (t *chatTurn) player(name string) (string, error) {
	if players, ok := t.players[name]; ok {
		if len(players) == 0 {
			return "", fmt.Errorf("no agent plays the role %q of workflow %s this turn", name, t.owner)
		}
		return players[0], nil
	}
	if slices.Contains(t.group, name) {
		return name, nil
	}
	if def, ok := bundle.Lookup(t.catalog, agentdef.AgentKind, name); ok && def.Owner.ID == t.owner {
		return name, nil
	}
	return "", fmt.Errorf("workflow %s has no role %q, and no agent of that name is cast this turn", t.owner, name)
}

func (t *chatTurn) run(ctx context.Context, sv *specValue, invoke host.NodeRunner, layers []config.Layer) map[string]any {
	t.mu.Lock()
	agent := sv.name
	if t.err == nil && t.owner != "" {
		agent, t.err = t.player(sv.name)
	}
	if t.err != nil {
		defer t.mu.Unlock()
		return agentResultJSON(t.last)
	}
	spec := t.spec
	if t.owner != "" {
		// The workflow's own agents are its authored chat loops; an agent
		// of another bundle runs as that bundle wrote it. Each keeps its
		// own thread in the chat, whichever role it plays.
		def, _ := bundle.Lookup(t.catalog, agentdef.AgentKind, agent)
		spec.Name, spec.Def, spec.LocalBehavior = agent, nil, def.Owner.ID == t.owner
		spec.Thread = host.MemberThread(spec.Thread, agent)
	}
	t.calls++
	t.mu.Unlock()

	spec.Task = sv.task
	spec.Layers = append(slices.Clone(spec.Layers), layers...)
	spec.DefinitionCatalog = t.catalog
	if len(t.group) > 1 {
		spec.Group, spec.GroupThread = t.group, t.spec.Thread
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
