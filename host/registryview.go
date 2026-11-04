package host

import (
	"context"
	"encoding/json"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/tools"
)

// RegistryView is a ToolView over a capability-services registry view:
// every ready capability in the view is an agent tool, so registering a
// capability makes it callable from agents (and, through the workflow
// engine, from Starlark) with no host changes. Tool names are the
// registry tool form (dots become underscores); definition filters match
// that form, as the loop sees it.
//
// The calling agent's identity (CallerFromContext) is stamped as the
// registry caller, so capability policy and job access see the agent,
// with the host's root caller as its parent.
type RegistryView struct {
	view  *registry.View
	specs []model.FilterSpec // narrowing filters, all must allow
}

// NewRegistryView wraps v. A nil view yields an empty tool set.
func NewRegistryView(v *registry.View) *RegistryView {
	return &RegistryView{view: v}
}

// View returns the underlying registry view.
func (r *RegistryView) View() *registry.View { return r.view }

func (r *RegistryView) allows(t model.Tool) bool {
	for _, s := range r.specs {
		if !s.Allows(t) {
			return false
		}
	}
	return true
}

func (r *RegistryView) Tools() []model.Tool {
	if r.view == nil {
		return nil
	}
	defs := tools.Defs(r.view)
	out := make([]model.Tool, 0, len(defs))
	for _, d := range defs {
		schema, _ := json.Marshal(d.InputSchema)
		t := model.Tool{
			Name:        d.Name,
			Description: d.Description,
			Schema:      schema,
			Effect:      effectOf(d.Entry),
			Group:       d.Entry.Group,
		}
		if r.allows(t) {
			out = append(out, t)
		}
	}
	return out
}

func (r *RegistryView) Groups() []model.ToolGroup {
	if r.view == nil {
		return nil
	}
	gs := r.view.Groups()
	out := make([]model.ToolGroup, 0, len(gs))
	for _, g := range gs {
		out = append(out, model.ToolGroup{Name: g.Name, Description: g.Description, LoadAlways: g.LoadAlways})
	}
	return out
}

// effectOf maps registry effects onto the loop's effect ranking. A Long
// capability ranks as mutating for ordering but keeps its "long" label so
// dispatch treats it as a job. Unclassified effects fail closed.
func effectOf(e registry.Entry) string {
	if e.Long {
		return "long"
	}
	switch {
	case e.Effects.Has(registry.ReadOnly):
		return "read-only"
	case e.Effects.Has(registry.Idempotent):
		return "idempotent"
	default:
		return "mutating"
	}
}

func (r *RegistryView) listed(name string) bool {
	for _, t := range r.Tools() {
		if t.Name == name {
			return true
		}
	}
	return false
}

func (r *RegistryView) Run(ctx context.Context, name string, args json.RawMessage) (model.ToolResult, error) {
	if r.view == nil || !r.listed(name) {
		return unknownTool(name), nil
	}
	res := tools.Do(withRegistryCaller(ctx), r.view, name, args)
	return toolResultOf(res), nil
}

// WaitJob blocks until the job settles or ctx ends. An unknown or
// inaccessible job reports done=false.
func (r *RegistryView) WaitJob(ctx context.Context, jobID string) (model.ToolResult, bool) {
	if r.view == nil {
		return model.ToolResult{}, false
	}
	ctx = withRegistryCaller(ctx)
	job, ok := r.view.JobFor(ctx, jobID)
	if !ok {
		return model.ToolResult{}, false
	}
	out, err := job.Wait(ctx)
	if ctx.Err() != nil {
		return model.ToolResult{}, false
	}
	entry, _ := r.view.Get(job.Capability())
	return toolResultOf(tools.Outcome(entry, out, err)), true
}

func (r *RegistryView) Narrow(spec model.FilterSpec) ToolView {
	return &RegistryView{view: r.view, specs: append(append([]model.FilterSpec(nil), r.specs...), spec)}
}

func toolResultOf(res tools.Result) model.ToolResult {
	code := res.Reason
	if code == "" && res.Code != "" {
		code = string(res.Code)
	}
	return model.ToolResult{
		Text:    res.Render(),
		IsError: res.IsError,
		Code:    code,
		JobID:   res.Job,
	}
}

// withRegistryCaller stamps the calling agent as the registry caller
// when the host put one in ctx; an existing registry caller wins.
func withRegistryCaller(ctx context.Context) context.Context {
	if _, ok := registry.CallerFrom(ctx); ok {
		return ctx
	}
	if c, ok := CallerFromContext(ctx); ok {
		return registry.WithCaller(ctx, registryCallerOf(c))
	}
	return ctx
}

func registryCallerOf(c model.CallerInfo) registry.Caller {
	out := registry.Caller{ID: c.ID, Kind: c.Kind, Session: c.Session, Attrs: c.Attrs}
	if c.Parent != nil {
		p := registryCallerOf(*c.Parent)
		out.Parent = &p
	}
	return out
}

// CallerInfoOf converts a registry caller to the host's caller shape.
func CallerInfoOf(c registry.Caller) model.CallerInfo {
	out := model.CallerInfo{ID: c.ID, Kind: c.Kind, Session: c.Session, Attrs: c.Attrs}
	if c.Parent != nil {
		p := CallerInfoOf(*c.Parent)
		out.Parent = &p
	}
	return out
}
