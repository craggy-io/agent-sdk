package agentdef

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"testing/fstest"

	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/kind"

	"github.com/craggy-io/script-services/check"
	"github.com/craggy-io/script-services/script"
	"github.com/craggy-io/script-services/starcompile"
)

// The kinds of definition content. Each is global: a contribution's id is
// the name hosts and scripts use (an agent's, skill's or workflow's), and
// it is unique across a catalog unless a layer overrides another.
var (
	// AgentKind is a .star file that calls agent(...) exactly once, with
	// name equal to the contribution id.
	AgentKind = kind.New[*Definition]("agentsdk.agent", 1, kind.Global())
	// SkillKind is a SKILL.md file: front matter (name, description)
	// and a body. A name in the front matter must equal the contribution
	// id.
	SkillKind = kind.New[*Skill]("agentsdk.skill", 1, kind.Global())
	// WorkflowKind is a workflow .star file, checked when loaded and run
	// by package workflow.
	WorkflowKind = kind.New[*Workflow]("agentsdk.workflow", 1, kind.Global())
)

// Diagnostic codes this package reports besides starcompile's
// (script.check, script.load, script.exec).
const (
	// CodeAgent: an agent file declares no agent, several, or one whose
	// name is not its contribution id.
	CodeAgent bundle.Code = "agentsdk.agent"
	// CodeSkill: a SKILL.md file is unreadable or malformed, or names
	// another skill.
	CodeSkill bundle.Code = "agentsdk.skill"
)

// Workflow is a catalogued workflow: its checked script, kept to run
// later. Package workflow runs Unit's top level with its built-ins, then
// run(input).
type Workflow struct {
	Name string
	Unit *starcompile.Unit
}

// Source returns the workflow's source text.
func (w *Workflow) Source() string { return string(w.Unit.Source()) }

// workflowBuiltins are the names a workflow script and the modules it
// loads see. Package workflow provides exactly these (its tests check).
var workflowBuiltins = []string{
	"agent", "ask", "budget", "call", "close", "config", "define", "interrupt",
	"json", "list_agents", "log", "parallel", "phase", "render", "resume",
	"schema", "send", "spawn", "spec", "start", "status", "tools", "wait",
	"workflow", "workflow_run",
}

// WorkflowBuiltins returns the predeclared names workflow files are
// checked against when they load.
func WorkflowBuiltins() []string { return append([]string(nil), workflowBuiltins...) }

// Compilers returns the compilers for AgentKind, SkillKind and
// WorkflowKind. Open uses them; a host that builds its own catalog with
// bundle.Load adds them.
//
// Agent and workflow files compile through script-services' shared
// compiler (starcompile): load() is served by the file's own bundle and,
// as @<dependency-id>/path, by the dependencies its bundle.yaml declares.
// Prompt {"file": ...} fragments and loop.output schema files are read
// from the agent file's bundle or a declared @bundle.id/path dependency. Compiling has no
// side effects, so bundle.Live reuses compiled definitions across
// reloads.
func Compilers() []bundle.AnyCompiler {
	return []bundle.AnyCompiler{
		bundle.Compile(AgentKind, starcompile.Compiler(agentOptions(), compileAgent)),
		bundle.Compile(SkillKind, compileSkill),
		bundle.Compile(WorkflowKind, starcompile.Compiler(workflowOptions(), compileWorkflow)),
	}
}

// agentOptions: an agent file sees agent() and the helpers; the modules
// it loads see only the helpers.
func agentOptions() starcompile.Options {
	return starcompile.Options{
		Check:             check.Env{Predeclared: []string{"agent", "tools_filter", "loop_spec", "schema"}},
		ModulePredeclared: helpers(),
	}
}

func workflowOptions() starcompile.Options {
	return starcompile.Options{Check: check.Env{
		Predeclared:       workflowBuiltins,
		ModulePredeclared: workflowBuiltins,
	}}
}

func compileAgent(ctx context.Context, u *starcompile.Unit) (bundle.Compiled[*Definition], []bundle.Diagnostic) {
	d, diags := buildAgent(ctx, u)
	if d == nil {
		return bundle.Compiled[*Definition]{}, diags
	}
	if id := u.Contribution().Ref.ID; d.Name != id {
		return bundle.Compiled[*Definition]{}, []bundle.Diagnostic{u.Errorf(CodeAgent, script.Position{},
			"%s declares agent %q but bundle.yaml names the contribution %q; they must match", u.Path(), d.Name, id)}
	}
	return bundle.Compiled[*Definition]{Value: d}, nil
}

// buildAgent runs an agent file's top level with a fresh collector and
// returns the one agent it declares.
func buildAgent(ctx context.Context, u *starcompile.Unit) (*Definition, []bundle.Diagnostic) {
	contribution := u.Contribution()
	col := &collector{
		file: u.Path(), dir: path.Dir(u.Path()),
		read: func(name string) ([]byte, error) { return readSupport(contribution, name) },
	}
	if _, err := u.Exec(ctx, starcompile.ExecOptions{Predeclared: col.predeclared()}); err != nil {
		return nil, u.Diagnostics(err)
	}
	if len(col.defs) != 1 {
		return nil, []bundle.Diagnostic{u.Errorf(CodeAgent, script.Position{},
			"%s declares %d agents; a definition file declares exactly one agent(...)", u.Path(), len(col.defs))}
	}
	d := col.defs[0]
	d.SourceText = u.Source()
	return d, nil
}

func compileSkill(_ context.Context, c bundle.Contribution) (bundle.Compiled[*Skill], []bundle.Diagnostic) {
	fail := func(err error) (bundle.Compiled[*Skill], []bundle.Diagnostic) {
		return bundle.Compiled[*Skill]{}, []bundle.Diagnostic{{
			Code: CodeSkill, Severity: bundle.SeverityError, Bundle: c.Bundle.ID,
			Contribution: c.Ref.ID, Path: c.Path, Message: err.Error(),
		}}
	}
	data, err := fs.ReadFile(c.Files, c.Path)
	if err != nil {
		return fail(err)
	}
	sk, err := parseSkill(data, c.Ref.ID, c.Path)
	if err != nil {
		return fail(err)
	}
	return bundle.Compiled[*Skill]{Value: sk}, nil
}

func compileWorkflow(_ context.Context, u *starcompile.Unit) (bundle.Compiled[*Workflow], []bundle.Diagnostic) {
	return bundle.Compiled[*Workflow]{Value: &Workflow{Name: u.Contribution().Ref.ID, Unit: u}}, nil
}

// ParseSource compiles a runtime-supplied definition source (agents.run
// and agents.define with source, and promote's validation), which must
// declare exactly one agent. The result is not catalogued. The source goes
// through the same compiler as a bundle's agent file, as a lone file:
// there is nothing to load() and no prompt or schema file to read. A
// failure is a *bundle.LoadError holding starcompile's diagnostics.
func ParseSource(ctx context.Context, src []byte) (*Definition, error) {
	const file = "dynamic.star"
	c := bundle.Contribution{
		Ref:    bundle.Ref{Owner: "agentsdk.dynamic", ID: "dynamic"},
		Kind:   AgentKind.ID(),
		Bundle: bundle.Identity{ID: "agentsdk.dynamic"},
		Path:   file,
		Files:  fstest.MapFS{file: {Data: src}},
	}
	u, diags := starcompile.New(ctx, c, agentOptions())
	if u == nil {
		return nil, &bundle.LoadError{Diagnostics: diags}
	}
	d, diags := buildAgent(ctx, u)
	if d == nil {
		return nil, &bundle.LoadError{Diagnostics: diags}
	}
	return d, nil
}

// readSupport reads only the accepted bundle snapshot or a declared dependency.
func readSupport(c bundle.Contribution, name string) ([]byte, error) {
	if !strings.HasPrefix(name, "@") {
		return fs.ReadFile(c.Files, name)
	}
	owner, file, ok := strings.Cut(strings.TrimPrefix(name, "@"), "/")
	if !ok || !fs.ValidPath(file) {
		return nil, fmt.Errorf("invalid dependency support path %q", name)
	}
	files, _, ok := c.Dependency(owner)
	if !ok {
		return nil, fmt.Errorf("undeclared support dependency %q", owner)
	}
	return fs.ReadFile(files, file)
}
