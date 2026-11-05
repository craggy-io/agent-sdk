package agentcap

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.starlark.net/starlark"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/capability-services/registry"
)

// Promotion: a dynamic definition or workflow is installed in the user
// layer of the host's loader (agentdef.Loader.InstallAgent and
// InstallWorkflow, which add the file and its bundle.yaml entry through
// bundle.Live.Edit; the bundle validates the name) only when the host's Approve hook accepts the exact
// bytes. Agents can propose a definition; they cannot approve it.

func (s *service) approve(ctx context.Context, kind, name string, payload []byte) error {
	if s.opts.Approve == nil {
		return registry.Errorf(registry.Denied, "%s.promote requires host approval; none configured", kind)
	}
	if s.h.Defs() == nil || s.h.Defs().UserDir() == "" {
		return registry.Errorf(registry.Unavailable, "%s.promote needs a user definition layer (agentdef.Options.UserDir)", kind)
	}
	if !s.opts.Approve(ctx, kind, name, payload) {
		return registry.Errorf(registry.Denied, "%s.promote of %q was not approved", kind, name)
	}
	return nil
}

func (s *service) promoteAgent(ctx context.Context, d *dynAgent) (string, error) {
	name := d.def.Name
	src := d.source
	if src == "" {
		var err error
		src, err = renderAgentStar(d.def)
		if err != nil {
			return "", invalid("%v", err)
		}
	}
	// Validate the exact bytes before asking for approval: they must
	// declare this agent, resolvable against the catalogue. The install
	// reloads every layer and is rolled back if that fails.
	pd, err := agentdef.ParseSource(ctx, []byte(src))
	if err != nil {
		return "", invalid("promoted source would not parse: %v", err)
	}
	if pd.Name != name {
		return "", invalid("promoted source declares agent %q, not %q", pd.Name, name)
	}
	if s.h.Defs() != nil && pd.Extends != "" {
		if _, err := s.h.Defs().ResolveDynamic(*pd, nil); err != nil {
			return "", coded(err)
		}
	}
	if err := s.approve(ctx, "agent", name, []byte(src)); err != nil {
		return "", err
	}
	p, err := s.h.Defs().InstallAgent(ctx, name, []byte(src))
	return p, coded(err)
}

func (s *service) promoteWorkflow(ctx context.Context, name, source string) (string, error) {
	if strings.TrimSpace(source) == "" || !strings.Contains(source, "WORKFLOW") ||
		!strings.Contains(source, "def run") {
		return "", invalid("workflow %q source lacks WORKFLOW/def run", name)
	}
	if err := s.approve(ctx, "workflow", name, []byte(source)); err != nil {
		return "", err
	}
	p, err := s.h.Defs().InstallWorkflow(ctx, name, []byte(source))
	return p, coded(err)
}

// q renders a Starlark string literal.
func q(s string) string { return starlark.String(s).String() }

func qList(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = q(it)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// renderAgentStar renders a JSON-defined dynamic definition as a
// Starlark agent(...) declaration. Prompt entries need inline text
// (catalogue-loaded file contents qualify); a bare file reference
// cannot be promoted.
func renderAgentStar(d *agentdef.Definition) (string, error) {
	var b strings.Builder
	b.WriteString("# Promoted by agents.promote. Review before editing by hand.\n\nagent(\n")
	fmt.Fprintf(&b, "    name = %s,\n", q(d.Name))
	if d.Extends != "" {
		fmt.Fprintf(&b, "    extends = %s,\n", q(d.Extends))
	}
	if d.Description != "" {
		fmt.Fprintf(&b, "    description = %s,\n", q(d.Description))
	}
	if d.Profile != "" {
		fmt.Fprintf(&b, "    profile = %s,\n", q(d.Profile))
	}
	if len(d.Prompt) > 0 {
		b.WriteString("    prompt = [\n")
		for _, p := range d.Prompt {
			switch {
			case p.Remove != "":
				fmt.Fprintf(&b, "        {\"remove\": %s},\n", q(p.Remove))
			case p.Replace != "":
				if p.Text == "" {
					return "", fmt.Errorf("prompt replace %q has no inline text to promote", p.Replace)
				}
				fmt.Fprintf(&b, "        {\"replace\": %s, \"text\": %s},\n", q(p.Replace), q(p.Text))
			case p.Name != "":
				fmt.Fprintf(&b, "        {%s: %s},\n", q(p.Name), q(p.Text))
			case p.Text != "":
				name := p.File
				if name != "" {
					if i := strings.LastIndex(name, "."); i > 0 {
						name = name[:i]
					}
					if i := strings.LastIndex(name, "/"); i >= 0 {
						name = name[i+1:]
					}
					fmt.Fprintf(&b, "        {%s: %s},\n", q(name), q(p.Text))
				} else {
					fmt.Fprintf(&b, "        %s,\n", q(p.Text))
				}
			default:
				return "", fmt.Errorf("prompt entry for file %q has no loaded text to promote", p.File)
			}
		}
		b.WriteString("    ],\n")
	}
	t := d.Tools
	if len(t.Include) > 0 || len(t.Exclude) > 0 || t.MaxEffect != "" || len(t.Allow) > 0 || !t.Inherit {
		var args []string
		if len(t.Include) > 0 {
			args = append(args, "include = "+qList(t.Include))
		}
		if len(t.Exclude) > 0 {
			args = append(args, "exclude = "+qList(t.Exclude))
		}
		if t.MaxEffect != "" {
			args = append(args, "max_effect = "+q(t.MaxEffect))
		}
		if len(t.Allow) > 0 {
			args = append(args, "allow = "+qList(t.Allow))
		}
		if !t.Inherit {
			args = append(args, "inherit = False")
		}
		fmt.Fprintf(&b, "    tools = tools_filter(%s),\n", strings.Join(args, ", "))
	}
	if len(d.Skills) > 0 {
		fmt.Fprintf(&b, "    skills = %s,\n", qList(d.Skills))
	}
	if len(d.Delegates) > 0 {
		fmt.Fprintf(&b, "    delegates = %s,\n", qList(d.Delegates))
	}
	if d.Workflows {
		b.WriteString("    workflows = True,\n")
	}
	if args := loopArgs(d.Loop); len(args) > 0 {
		fmt.Fprintf(&b, "    loop = loop_spec(%s),\n", strings.Join(args, ", "))
	}
	b.WriteString(")\n")
	return b.String(), nil
}

func loopArgs(l agentdef.LoopSpec) []string {
	var args []string
	i := func(name string, p *int) {
		if p != nil {
			args = append(args, fmt.Sprintf("%s = %d", name, *p))
		}
	}
	f := func(name string, p *float64) {
		if p != nil {
			args = append(args, fmt.Sprintf("%s = %v", name, *p))
		}
	}
	i("max_steps", l.MaxSteps)
	if l.Completion != nil {
		args = append(args, "completion = "+q(*l.Completion))
	}
	if l.Output != nil {
		args = append(args, "output = "+q(*l.Output))
	}
	i("tool_result_bytes", l.ToolResultBytes)
	if l.WaitJobs != nil {
		args = append(args, "wait_jobs = "+q(l.WaitJobs.String()))
	}
	f("compact_at", l.CompactAt)
	i("keep_recent_tokens", l.KeepRecentTokens)
	i("summary_tokens", l.SummaryTokens)
	if l.ProgressiveTools != nil {
		v := "False"
		if *l.ProgressiveTools {
			v = "True"
		}
		args = append(args, "progressive_tools = "+v)
	}
	f("clear_results_at", l.ClearResultsAt)
	i("keep_recent_results", l.KeepRecentResults)
	sort.Strings(args) // stable output for review diffs; order is irrelevant
	return args
}
