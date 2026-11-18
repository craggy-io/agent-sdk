package agentdef

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/bundle/compile"
	"github.com/craggy-io/script-services/starcompile"
	"go.starlark.net/starlark"
)

// Behavior pins the executable loop and its loaded modules to the defining bundle.
type Behavior struct {
	Path     string             `json:"path"`
	Digest   string             `json:"digest"`
	Function *starlark.Function `json:"-"`
}

type agentTools struct {
	model.FilterSpec
	Inherit *bool `json:"inherit"`
}

func (t agentTools) filter() model.FilterSpec {
	f := t.FilterSpec
	f.Inherit = t.Inherit == nil || *t.Inherit
	return f
}

type agentFile struct {
	Name        string        `json:"name"`
	Extends     string        `json:"extends"`
	Description string        `json:"description"`
	Profile     string        `json:"profile"`
	Prompt      []PromptEntry `json:"prompt"`
	Tools       *agentTools   `json:"tools"`
	Skills      []string      `json:"skills"`
	Loop        string        `json:"loop"`
}

func agentCompiler(ctx context.Context, c bundle.Contribution) (bundle.Compiled[*Definition], []bundle.Diagnostic) {
	if path.Ext(c.Path) != ".yaml" {
		return starcompile.Compiler(agentOptions(), compileAgent)(ctx, c)
	}
	file, diags := compile.YAML[agentFile]()(ctx, c)
	if len(diags) > 0 {
		return bundle.Compiled[*Definition]{}, diags
	}
	f := file.Value
	fail := func(err error) (bundle.Compiled[*Definition], []bundle.Diagnostic) {
		return bundle.Compiled[*Definition]{}, []bundle.Diagnostic{{Code: CodeAgent, Severity: bundle.SeverityError,
			Bundle: c.Bundle.ID, Contribution: c.Ref.ID, Path: c.Path, Message: err.Error()}}
	}
	if f.Name != c.Ref.ID {
		return fail(fmt.Errorf("agent name %q must match the contribution id %q", f.Name, c.Ref.ID))
	}
	if f.Loop == "" && f.Extends == "" {
		return fail(fmt.Errorf("an agent needs an executable loop or a parent to inherit it from"))
	}
	d := &Definition{Name: f.Name, Description: f.Description, Profile: f.Profile, Extends: f.Extends,
		Skills: f.Skills, Prompt: f.Prompt, Source: c.Path, Tools: model.FilterSpec{Inherit: true}}
	if f.Tools != nil {
		d.Tools = f.Tools.filter()
		if err := ValidateMaxEffect(d.Name, d.Tools.MaxEffect); err != nil {
			return fail(err)
		}
	}
	for i := range d.Prompt {
		p := &d.Prompt[i]
		if p.File != "" {
			b, err := readSupport(c, joinPath(path.Dir(c.Path), p.File))
			if err != nil {
				return fail(err)
			}
			p.Text = string(b)
		}
	}
	var err error
	d.SourceText, err = fs.ReadFile(c.Files, c.Path)
	if err != nil {
		return fail(err)
	}
	if f.Loop != "" {
		var ds []bundle.Diagnostic
		d.Behavior, ds, err = compileBehavior(ctx, c, f.Loop)
		if err != nil {
			return fail(err)
		}
		if len(ds) > 0 {
			return bundle.Compiled[*Definition]{}, ds
		}
	}
	return bundle.Compiled[*Definition]{Value: d}, nil
}

func compileBehavior(ctx context.Context, c bundle.Contribution, name string) (*Behavior, []bundle.Diagnostic, error) {
	if !fs.ValidPath(name) || strings.Contains(name, "\\") {
		return nil, nil, fmt.Errorf("invalid loop path %q", name)
	}
	c.Path = path.Join(path.Dir(c.Path), name)
	unit, ds := starcompile.New(ctx, c, starcompile.Options{})
	if unit == nil {
		return nil, ds, nil
	}
	top, err := unit.Exec(ctx, starcompile.ExecOptions{})
	if err != nil {
		return nil, unit.Diagnostics(err), nil
	}
	fn, ok := top.Globals["run"].(*starlark.Function)
	if !ok || fn.NumParams() != 2 || fn.NumKwonlyParams() != 0 || fn.HasVarargs() || fn.HasKwargs() {
		return nil, nil, fmt.Errorf("loop must define run(step, max_steps)")
	}
	return &Behavior{Path: c.Path, Digest: unit.Ref().Digest, Function: fn}, nil, nil
}
