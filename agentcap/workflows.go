package agentcap

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/workflow"
	"github.com/craggy-io/capability-services/registry"
)

type workflowRunIn struct {
	Name        string         `json:"name,omitempty"`
	Source      string         `json:"source,omitempty"`
	Input       map[string]any `json:"input,omitempty"`
	ResumeRunID string         `json:"resume_run_id,omitempty"`
}

type workflowRunOut struct {
	Status  string                 `json:"status"`
	Value   json.RawMessage        `json:"value,omitempty"`
	RunID   string                 `json:"run_id"`
	Pending *workflow.PendingInput `json:"pending,omitempty"`
	Error   string                 `json:"error,omitempty"`
}

func wfOut(res workflow.Result, err error) (workflowRunOut, error) {
	o := workflowRunOut{Status: res.Status, RunID: res.RunID, Pending: res.Pending}
	if res.Value != nil {
		b, mErr := json.Marshal(res.Value)
		if mErr != nil {
			return o, coded(mErr)
		}
		o.Value = b
	}
	if err != nil {
		o.Error = err.Error()
	}
	return o, nil
}

func (s *service) workflowCaps() []registry.Capability {
	ro := registry.WithEffects(registry.ReadOnly)
	hostOnly := registry.WithTags("host")
	return []registry.Capability{
		registry.Func("workflows.list", "List catalogued workflows",
			func(ctx context.Context, _ struct{}) ([]string, error) {
				names := []string{}
				if s.h.Defs() != nil {
					_, _ = s.h.Defs().Reload(ctx) // a failed reload keeps the previous catalogue
					names = append(names, s.h.Defs().Workflows()...)
				}
				s.mu.Lock()
				for n := range s.dynWf {
					names = append(names, n)
				}
				s.mu.Unlock()
				sort.Strings(names)
				return names, nil
			}, ro),
		registry.Func("workflows.get", "Get one workflow's source",
			func(_ context.Context, in nameIn) (map[string]any, error) {
				s.mu.Lock()
				src, ok := s.dynWf[in.Name]
				s.mu.Unlock()
				if !ok && s.h.Defs() != nil {
					var wf *agentdef.Workflow
					if wf, ok = s.h.Defs().Workflow(in.Name); ok {
						src = wf.Source()
					}
				}
				if !ok {
					return nil, registry.Errorf(registry.NotFound, "unknown workflow %q", in.Name)
				}
				return map[string]any{"name": in.Name, "source": src}, nil
			}, ro),
		registry.Func("workflows.define", "Register a session-scoped workflow",
			func(_ context.Context, in struct {
				Name   string `json:"name"`
				Source string `json:"source"`
			}) (map[string]any, error) {
				if in.Name == "" || in.Source == "" {
					return nil, invalid("name and source are required")
				}
				s.mu.Lock()
				s.dynWf[in.Name] = in.Source
				s.mu.Unlock()
				return map[string]any{"name": in.Name, "defined": true}, nil
			}, registry.Describe("Register a named dynamic workflow source for this session.")),
		registry.Func("workflows.run", "Run or resume a workflow",
			func(ctx context.Context, in workflowRunIn) (workflowRunOut, error) {
				if in.ResumeRunID != "" {
					res, err := workflow.Resume(ctx, s.h, in.ResumeRunID)
					return wfOut(res, err)
				}
				nameOrSource := in.Source
				if nameOrSource == "" {
					s.mu.Lock()
					src, ok := s.dynWf[in.Name]
					s.mu.Unlock()
					if ok {
						nameOrSource = src
					} else {
						nameOrSource = in.Name
					}
				}
				res, err := workflow.RunEx(ctx, s.h, nameOrSource, in.Input, workflow.Options{})
				return wfOut(res, err)
			}, registry.AsLong(0)),
		registry.Func("workflows.pending", "List ask gates waiting for an answer",
			func(ctx context.Context, _ struct{}) ([]workflow.PendingInput, error) {
				pending, err := workflow.ListWaiting(ctx, s.h.Sink())
				if err != nil {
					return nil, coded(err)
				}
				if pending == nil {
					pending = []workflow.PendingInput{}
				}
				return pending, nil
			}, ro, hostOnly, registry.Describe("Host surface: never in an agent's default tool view.")),
		registry.Func("workflows.answer", "Answer a pending ask gate",
			func(ctx context.Context, in struct {
				RunID string          `json:"run_id"`
				Key   string          `json:"key"`
				Value json.RawMessage `json:"value,omitempty"`
			}) (map[string]any, error) {
				if err := workflow.Answer(ctx, s.h.Recorder(), in.RunID, in.Key, in.Value); err != nil {
					return nil, coded(err)
				}
				return map[string]any{"answered": true, "run_id": in.RunID, "key": in.Key}, nil
			}, hostOnly, registry.Describe("Host surface: agents can propose but never approve — never in an agent's default tool view.")),
		registry.Func("workflows.promote", "Install a dynamic workflow in the user layer",
			func(ctx context.Context, in struct {
				Name   string `json:"name"`
				Source string `json:"source,omitempty"`
			}) (map[string]any, error) {
				src := in.Source
				if src == "" {
					s.mu.Lock()
					src = s.dynWf[in.Name]
					s.mu.Unlock()
				}
				if src == "" {
					return nil, registry.Errorf(registry.NotFound, "no source for workflow %q to promote", in.Name)
				}
				p, err := s.promoteWorkflow(ctx, in.Name, src)
				if err != nil {
					return nil, err
				}
				return map[string]any{"promoted": true, "name": in.Name, "path": p}, nil
			}, registry.WithEffects(registry.Destructive), registry.WithTags("admin")),
	}
}
