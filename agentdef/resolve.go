package agentdef

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"text/template"
	"time"

	"github.com/craggy-io/foundation-kit/bundle"

	"github.com/craggy-io/agent-sdk/model"
)

// Resolve resolves a catalogued definition by name. vars is exposed to
// prompt templates as .Vars; a string vars["Date"] overrides .Date
// (otherwise today, UTC). The chain and skills come from one catalog, so
// a reload in progress never mixes two versions.
func (l *Loader) Resolve(name string, vars map[string]any) (*Resolved, error) {
	cat := l.Catalog()
	d, ok := lookup(cat, AgentKind, name)
	if !ok {
		return nil, fmt.Errorf("agentdef: unknown agent %q", name)
	}
	chain, err := chainOf(cat, d)
	if err != nil {
		return nil, err
	}
	return resolveChain(cat, chain, vars)
}

// ResolveDynamic resolves a runtime-built definition. It must extends an
// existing catalogued definition — a variant, not a blank slate.
func (l *Loader) ResolveDynamic(d Definition, vars map[string]any) (*Resolved, error) {
	if d.Name == "" {
		return nil, fmt.Errorf("agentdef: dynamic definition needs a name")
	}
	if d.Extends == "" {
		return nil, fmt.Errorf("agentdef: dynamic definition %q must extends an existing definition", d.Name)
	}
	cat := l.Catalog()
	if _, ok := lookup(cat, AgentKind, d.Extends); !ok {
		return nil, fmt.Errorf("agentdef: dynamic definition %q extends unknown definition %q", d.Name, d.Extends)
	}
	if err := ValidateMaxEffect(d.Name, d.Tools.MaxEffect); err != nil {
		return nil, err
	}
	leaf := d
	chain, err := chainOf(cat, &leaf)
	if err != nil {
		return nil, err
	}
	return resolveChain(cat, chain, vars)
}

// chainOf returns the inheritance chain root-first, detecting cycles and
// unknown parents.
func chainOf(cat *bundle.Catalog, leaf *Definition) ([]*Definition, error) {
	var rev []*Definition
	seen := map[string]int{}
	cur := leaf
	for cur != nil {
		if at, dup := seen[cur.Name]; dup {
			names := make([]string, 0, len(rev)-at+1)
			for i := len(rev) - 1; i >= at; i-- {
				names = append(names, rev[i].Name)
			}
			return nil, fmt.Errorf("agentdef: extends cycle: %s -> %s", strings.Join(names, " -> "), cur.Name)
		}
		seen[cur.Name] = len(rev)
		rev = append(rev, cur)
		if cur.Extends == "" {
			break
		}
		parent, ok := lookup(cat, AgentKind, cur.Extends)
		if !ok {
			return nil, fmt.Errorf("agentdef: %q extends unknown definition %q", cur.Name, cur.Extends)
		}
		cur = parent
	}
	chain := make([]*Definition, len(rev))
	for i, d := range rev {
		chain[len(rev)-1-i] = d
	}
	return chain, nil
}

type fragment struct {
	name, text, source string // source = supplying definition name
}

func fragmentName(file string) string {
	base := path.Base(file)
	return strings.TrimSuffix(base, path.Ext(base))
}

func unionStrings(dst, add []string) []string {
	for _, s := range add {
		found := false
		for _, e := range dst {
			if e == s {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, s)
		}
	}
	return dst
}

// maxEffectStrictness ranks a max_effect cap: smaller is stricter. ""
// and "any" are the loosest. Inputs are validated before this is used.
func maxEffectStrictness(eff string) int {
	switch eff {
	case "read-only":
		return 0
	case "idempotent":
		return 1
	case "mutating":
		return 2
	default: // "", "any"
		return 3
	}
}

func stricterMaxEffect(a, b string) string {
	if maxEffectStrictness(a) <= maxEffectStrictness(b) {
		return a
	}
	return b
}

// toolsInheritParent applies the package-doc rule: catalogued
// definitions are normalised at load; a completely zero Tools value
// always inherits.
func toolsInheritParent(d *Definition, isRoot bool) bool {
	if isRoot {
		return false
	}
	t := d.Tools
	if t.Inherit {
		return true
	}
	return len(t.Include) == 0 && len(t.Exclude) == 0 && t.MaxEffect == "" && len(t.Allow) == 0
}

func resolveChain(cat *bundle.Catalog, chain []*Definition, vars map[string]any) (*Resolved, error) {
	if vars == nil {
		vars = map[string]any{}
	}
	leaf := chain[len(chain)-1]
	r := &Resolved{
		Name: leaf.Name, Extends: leaf.Extends,
		Provenance:  map[string]string{},
		SkillBodies: map[string]string{},
	}
	for _, d := range chain {
		r.Chain = append(r.Chain, d.Name)
	}
	var frags []fragment
	var outputRef, outputFrom string

	for ci, d := range chain {
		isRoot := ci == 0
		if d.Description != "" {
			r.Description = d.Description
			r.Provenance["description"] = d.Name
		}
		if d.Profile != "" {
			r.Profile = d.Profile
			r.Provenance["profile"] = d.Name
		}
		if d.Workflows {
			r.Workflows = true
			r.Provenance["workflows"] = d.Name
		}
		// Tools: union include/exclude/allow, stricter max_effect,
		// inherit=false starts from nothing.
		if isRoot || !toolsInheritParent(d, isRoot) {
			r.Tools = model.FilterSpec{Inherit: true,
				Include:   append([]string(nil), d.Tools.Include...),
				Exclude:   append([]string(nil), d.Tools.Exclude...),
				MaxEffect: d.Tools.MaxEffect,
				Allow:     append([]string(nil), d.Tools.Allow...)}
			if len(d.Tools.Include) > 0 {
				r.Provenance["tools.include"] = d.Name
			}
			if len(d.Tools.Exclude) > 0 {
				r.Provenance["tools.exclude"] = d.Name
			}
			if d.Tools.MaxEffect != "" {
				r.Provenance["tools.max_effect"] = d.Name
			}
			if len(d.Tools.Allow) > 0 {
				r.Provenance["tools.allow"] = d.Name
			}
		} else {
			if len(d.Tools.Include) > 0 {
				r.Tools.Include = unionStrings(r.Tools.Include, d.Tools.Include)
				r.Provenance["tools.include"] = d.Name
			}
			if len(d.Tools.Exclude) > 0 {
				r.Tools.Exclude = unionStrings(r.Tools.Exclude, d.Tools.Exclude)
				r.Provenance["tools.exclude"] = d.Name
			}
			if d.Tools.MaxEffect != "" {
				stricter := stricterMaxEffect(r.Tools.MaxEffect, d.Tools.MaxEffect)
				if stricter == d.Tools.MaxEffect && stricter != r.Tools.MaxEffect {
					r.Provenance["tools.max_effect"] = d.Name
				}
				r.Tools.MaxEffect = stricter
			}
			if len(d.Tools.Allow) > 0 {
				r.Tools.Allow = unionStrings(r.Tools.Allow, d.Tools.Allow)
				r.Provenance["tools.allow"] = d.Name
			}
		}
		if len(d.Skills) > 0 {
			r.Skills = unionStrings(r.Skills, d.Skills)
			r.Provenance["skills"] = d.Name
		}
		if len(d.Delegates) > 0 {
			r.Delegates = unionStrings(r.Delegates, d.Delegates)
			r.Provenance["delegates"] = d.Name
		}
		// Loop: field by field; output replaced.
		mergeLoop(&r.Loop, d.Loop, d.Name, r.Provenance)
		if d.Loop.Output != nil {
			outputRef, outputFrom = *d.Loop.Output, d.Name
		}
		// Prompt fragments.
		var err error
		frags, err = applyPromptEntries(frags, d)
		if err != nil {
			return nil, err
		}
		for _, f := range frags {
			r.Provenance["prompt."+f.name] = f.source
		}
	}

	if outputRef != "" {
		if !json.Valid([]byte(outputRef)) {
			return nil, fmt.Errorf("agentdef: %s: loop.output is not valid JSON", outputFrom)
		}
		r.OutputSchema = json.RawMessage(outputRef)
	}

	// Render: one system message, skills catalogue last.
	date := time.Now().UTC().Format("2006-01-02")
	if s, ok := vars["Date"].(string); ok && s != "" {
		date = s
	}
	data := map[string]any{
		"Agent": map[string]any{"Name": r.Name, "Description": r.Description},
		"Date":  date,
		"Vars":  vars,
	}
	parts := make([]string, 0, len(frags))
	for _, f := range frags {
		r.Fragments = append(r.Fragments, f.name)
		tpl, err := template.New(f.name).Option("missingkey=error").Parse(f.text)
		if err != nil {
			return nil, fmt.Errorf("agentdef: %s: prompt fragment %q: %w", r.Name, f.name, err)
		}
		var buf bytes.Buffer
		if err := tpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("agentdef: %s: prompt fragment %q: %w", r.Name, f.name, err)
		}
		if s := strings.TrimSpace(buf.String()); s != "" {
			parts = append(parts, s)
		}
	}
	r.SystemPrompt = strings.Join(parts, "\n\n")

	if len(r.Skills) > 0 {
		var list strings.Builder
		list.WriteString("## Skills\n\nThe following skills are available via the skill tool:\n")
		for _, name := range r.Skills {
			sk, ok := lookup(cat, SkillKind, name)
			if !ok {
				return nil, fmt.Errorf("agentdef: %s: unknown skill %q", r.Name, name)
			}
			r.SkillBodies[name] = sk.Body
			fmt.Fprintf(&list, "- `%s`: %s\n", sk.Name, sk.Description)
		}
		if r.SystemPrompt != "" {
			r.SystemPrompt += "\n\n"
		}
		r.SystemPrompt += strings.TrimRight(list.String(), "\n")
	}
	return r, nil
}

func mergeLoop(dst *LoopSpec, src LoopSpec, from string, prov map[string]string) {
	set := func(field string, present bool) {
		if present {
			prov["loop."+field] = from
		}
	}
	if src.MaxSteps != nil {
		dst.MaxSteps = src.MaxSteps
	}
	set("max_steps", src.MaxSteps != nil)
	if src.Completion != nil {
		dst.Completion = src.Completion
	}
	set("completion", src.Completion != nil)
	if src.Output != nil {
		dst.Output = src.Output
	}
	set("output", src.Output != nil)
	if src.ToolResultBytes != nil {
		dst.ToolResultBytes = src.ToolResultBytes
	}
	set("tool_result_bytes", src.ToolResultBytes != nil)
	if src.WaitJobs != nil {
		dst.WaitJobs = src.WaitJobs
	}
	set("wait_jobs", src.WaitJobs != nil)
	if src.CompactAt != nil {
		dst.CompactAt = src.CompactAt
	}
	set("compact_at", src.CompactAt != nil)
	if src.KeepRecentTokens != nil {
		dst.KeepRecentTokens = src.KeepRecentTokens
	}
	set("keep_recent_tokens", src.KeepRecentTokens != nil)
	if src.SummaryTokens != nil {
		dst.SummaryTokens = src.SummaryTokens
	}
	set("summary_tokens", src.SummaryTokens != nil)
	if src.ProgressiveTools != nil {
		dst.ProgressiveTools = src.ProgressiveTools
	}
	set("progressive_tools", src.ProgressiveTools != nil)
	if src.ClearResultsAt != nil {
		dst.ClearResultsAt = src.ClearResultsAt
	}
	set("clear_results_at", src.ClearResultsAt != nil)
	if src.KeepRecentResults != nil {
		dst.KeepRecentResults = src.KeepRecentResults
	}
	set("keep_recent_results", src.KeepRecentResults != nil)
}

func applyPromptEntries(frags []fragment, d *Definition) ([]fragment, error) {
	find := func(name string) int {
		for i, f := range frags {
			if f.name == name {
				return i
			}
		}
		return -1
	}
	for i, e := range d.Prompt {
		switch {
		case e.Remove != "":
			at := find(e.Remove)
			if at < 0 {
				return nil, fmt.Errorf("agentdef: %s: prompt remove %q: unknown fragment", d.Name, e.Remove)
			}
			frags = append(frags[:at], frags[at+1:]...)
		case e.Replace != "":
			at := find(e.Replace)
			if at < 0 {
				return nil, fmt.Errorf("agentdef: %s: prompt replace %q: unknown fragment", d.Name, e.Replace)
			}
			if e.Text == "" {
				return nil, fmt.Errorf("agentdef: %s: prompt replace %q: no text (file contents are loaded by the catalogue)", d.Name, e.Replace)
			}
			frags[at] = fragment{name: e.Replace, text: e.Text, source: d.Name}
		case e.File != "" || e.Name != "" || e.Text != "":
			name := e.Name
			if name == "" && e.File != "" {
				name = fragmentName(e.File)
			}
			if name == "" {
				name = fmt.Sprintf("text-%s-%d", d.Name, i)
			}
			if find(name) >= 0 {
				return nil, fmt.Errorf("agentdef: %s: duplicate prompt fragment %q (use replace)", d.Name, name)
			}
			if e.Text == "" {
				return nil, fmt.Errorf("agentdef: %s: prompt fragment %q: no text (file contents are loaded by the catalogue)", d.Name, name)
			}
			frags = append(frags, fragment{name: name, text: e.Text, source: d.Name})
		default:
			return nil, fmt.Errorf("agentdef: %s: empty prompt entry %d", d.Name, i)
		}
	}
	return frags, nil
}
