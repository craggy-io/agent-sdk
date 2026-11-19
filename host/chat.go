package host

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/internal/flowbridge"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

// RunAgent runs one agent to completion via the implicit agent-turn (§2).
// nameOrDef is a definition name, a agentdef.Definition, or *agentdef.Definition.
func (s *Host) RunAgent(ctx context.Context, nameOrDef any, task string, opts ...RunOption) (Result, error) {
	spec := ExecSpec{Task: task}
	switch v := nameOrDef.(type) {
	case string:
		spec.Name = v
	case agentdef.Definition:
		cp := v
		spec.Def = &cp
	case *agentdef.Definition:
		spec.Def = v
	default:
		return Result{}, fmt.Errorf("host: RunAgent wants a definition name or agentdef.Definition, got %T", nameOrDef)
	}
	for _, o := range opts {
		o(&spec)
	}
	return s.ExecAgent(ctx, spec)
}

// ExecAgent executes one agent node. With no nodeCtx in ctx it starts a
// new run (run.start/run.end, fresh budget/semaphore, config snapshot).
func (s *Host) ExecAgent(ctx context.Context, spec ExecSpec) (Result, error) {
	return s.runChat(ctx, "", spec)
}

// RunChat runs a named, installed workflow as a chat turn. Each role
// runs within the same host run and retains its own conversation thread.
func (s *Host) RunChat(ctx context.Context, workflow, message string, opts ...RunOption) (Result, error) {
	if workflow == "" {
		return Result{}, fmt.Errorf("host: missing chat workflow")
	}
	spec := ExecSpec{Task: message}
	for _, opt := range opts {
		opt(&spec)
	}
	return s.runChat(ctx, workflow, spec)
}

// WithGroup lets the run, and every agent it starts, call the named
// agents (ExecSpec.Group). A chat workflow declared roles = "roster"
// takes them as the turn's roster; any other refuses a group.
func WithGroup(names ...string) RunOption {
	return func(s *ExecSpec) { s.Group = slices.Clone(names) }
}

// WithLocalCalls observes tool requests and results from authored chat loops.
func WithLocalCalls(observer loop.CallObserver) RunOption {
	return func(s *ExecSpec) { s.OnLocalCall = observer }
}

// NodeRunner is bound to a workflow's run tree and original caller.
type NodeRunner func(context.Context, ExecSpec) (Result, error)

type chatEngine func(context.Context, *Host, string, ExecSpec, NodeRunner) (Result, error)

var chatRunner chatEngine

// RegisterChatEngine connects the workflow package to the host.
func RegisterChatEngine(_ flowbridge.Engine, run chatEngine) { chatRunner = run }

func (s *Host) runChat(ctx context.Context, name string, spec ExecSpec) (res Result, err error) {
	if chatRunner == nil {
		return Result{}, fmt.Errorf("host: chat workflow engine not registered (import the workflow package)")
	}
	label := name
	if label == "" {
		label = "agent-turn"
	}
	err = s.WithWorkflow(flowbridge.Engine{}, ctx, label, "", func(scoped context.Context, invoke NodeRunner) error {
		var inner error
		res, inner = chatRunner(scoped, s, name, spec, invoke)
		return inner
	})
	return res, err
}

// WithWorkflow binds workflow execution and all its nodes to one budget,
// semaphore, caller, and run ID. Existing runs retain their own ceiling.
func (s *Host) WithWorkflow(_ flowbridge.Engine, ctx context.Context, name, id string, execute func(context.Context, NodeRunner) error) error {
	if s.isClosed() {
		return fmt.Errorf("host: closed")
	}
	parent, inRun := nodeCtxFrom(ctx)
	top := !inRun
	if top {
		run := s.newRun(ctx)
		if id != "" {
			run.id = id
		}
		parent = s.rootNodeCtx(ctx, run)
		ctx = withNodeCtx(ctx, parent)
		_ = s.emit(ctx, trace.Event{Type: "run.start", RunID: run.id,
			Data: mustJSON(map[string]any{"workflow": name})})
		defer func() {
			run.end()
			_ = s.emit(context.WithoutCancel(ctx), trace.Event{Type: "run.end", RunID: run.id,
				Data: mustJSON(run.budget.snapshot())})
		}()
	}
	return execute(ctx, func(inner context.Context, spec ExecSpec) (Result, error) {
		return s.execNode(inner, parent, spec, top)
	})
}

func (s *Host) resolveNode(ctx context.Context, spec ExecSpec) (*agentdef.Resolved, error) {
	// Resolve the definition (§4).
	if s.defs == nil {
		return nil, fmt.Errorf("host: no definition loader")
	}
	// Pick up edits to the definition layers. A failed reload keeps the
	// previous catalogue; the trace says why.
	if spec.DefinitionCatalog == nil {
		if _, err := s.defs.Reload(ctx); err != nil {
			_ = s.emit(ctx, trace.Event{Type: "agentdef.reload_failed", SessionID: s.id,
				Data: mustJSON(map[string]any{"error": err.Error()})})
		}
	}
	var resolved *agentdef.Resolved
	var err error
	if spec.Def != nil {
		resolved, err = s.defs.ResolveDynamicFilled(*spec.Def, spec.Vars, spec.Fills)
	} else if spec.DefinitionCatalog != nil {
		resolved, err = agentdef.ResolveIn(spec.DefinitionCatalog, spec.Name, spec.Vars, spec.Fills)
	} else {
		resolved, err = s.defs.ResolveFilled(spec.Name, spec.Vars, spec.Fills)
	}
	if err != nil {
		return nil, err
	}
	if err := validateNodeSpec(spec, resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

func validateNodeSpec(spec ExecSpec, resolved *agentdef.Resolved) error {
	if spec.LocalBehavior && resolved.Behavior == nil {
		return fmt.Errorf("host: %s needs an authored chat loop", resolved.Name)
	}
	if resolved.Behavior != nil {
		skill := model.Tool{Name: "skill", Effect: "read-only"}
		if !resolved.Tools.Allows(skill) || (spec.Tools != nil && !spec.Tools.Allows(skill)) {
			resolved.WithoutSkills()
		}
	}
	if resolved.Behavior != nil && resolved.Behavior.Function == nil {
		return fmt.Errorf("agent %s has an uncompiled loop", resolved.Name)
	}
	if len(spec.Output) > 0 && !json.Valid(spec.Output) {
		return fmt.Errorf("host: WithOutput schema is not valid JSON")
	}

	return nil
}
