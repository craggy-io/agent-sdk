package loop

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/craggy-io/agent-sdk/model"
)

// toolsLoadName is the progressive-loading built-in (§4.3).
const toolsLoadName = "tools_load"

// initTools decides, once per run, whether progressive tool loading
// applies (§4.3): loop.progressive_tools is on and the view has at least
// one tool in a group that is not LoadAlways. It then recovers the groups
// earlier runs on this thread loaded, so a conversation keeps its tools.
func (r *runner) initTools() {
	if !r.policy.ProgressiveTools || r.deps.Tools == nil {
		return
	}
	onDemand := onDemandGroups(r.deps.Tools.Groups())
	for _, t := range r.deps.Tools.Tools() {
		if onDemand[t.Group] {
			r.progressive = true
			break
		}
	}
	if !r.progressive {
		return
	}
	calls := map[string]model.ToolCall{}
	for _, it := range r.thread.Items() {
		for _, c := range it.Calls {
			if c.Name == toolsLoadName {
				calls[c.ID] = c
			}
		}
		if c, ok := calls[it.CallID]; ok && it.Kind == model.KindToolResult && !it.IsError {
			if names, err := toolsLoadArgs(c.Args); err == nil {
				for _, n := range names {
					if onDemand[n] {
						r.load(n)
					}
				}
			}
		}
	}
}

// onDemandGroups is the set of groups progressive loading defers: every
// described group that is not LoadAlways. A tool naming an undescribed
// group is never deferred.
func onDemandGroups(groups []model.ToolGroup) map[string]bool {
	out := map[string]bool{}
	for _, g := range groups {
		if !g.LoadAlways {
			out[g.Name] = true
		}
	}
	return out
}

// load marks group loaded; it reports false when it already was.
func (r *runner) load(group string) bool {
	r.toolsMu.Lock()
	defer r.toolsMu.Unlock()
	for _, g := range r.loaded {
		if g == group {
			return false
		}
	}
	r.loaded = append(r.loaded, group)
	return true
}

func (r *runner) loadedGroups() []string {
	r.toolsMu.Lock()
	defer r.toolsMu.Unlock()
	return append([]string(nil), r.loaded...)
}

// viewTools is the runner's tools as advertised (§4.3). Without
// progressive loading that is the whole view. With it, the tools of
// LoadAlways and ungrouped tools come first, then the built-ins, then
// each loaded group's tools in load order — loading only ever appends,
// so the advertised prefix stays stable for the provider's cache.
func (r *runner) viewTools() []model.Tool {
	if r.deps.Tools == nil {
		return nil
	}
	all := r.deps.Tools.Tools()
	if !r.progressive {
		return all
	}
	onDemand := onDemandGroups(r.deps.Tools.Groups())
	var out []model.Tool
	for _, t := range all {
		if !onDemand[t.Group] {
			out = append(out, t)
		}
	}
	return out
}

// loadedTools is the tools of the loaded on-demand groups, in load order.
func (r *runner) loadedTools() []model.Tool {
	if !r.progressive || r.deps.Tools == nil {
		return nil
	}
	all := r.deps.Tools.Tools()
	onDemand := onDemandGroups(r.deps.Tools.Groups())
	var out []model.Tool
	for _, g := range r.loadedGroups() {
		if !onDemand[g] {
			continue
		}
		for _, t := range all {
			if t.Group == g {
				out = append(out, t)
			}
		}
	}
	return out
}

func toolsLoadTool() model.Tool {
	return model.Tool{Name: toolsLoadName, Effect: "read-only",
		Description: "Add the tools of on-demand tool groups (listed under \"Tool groups\" in your instructions) to your tool list. Loaded tools stay available for the rest of the conversation.",
		Schema:      json.RawMessage(`{"type":"object","required":["groups"],"properties":{"groups":{"type":"array","minItems":1,"items":{"type":"string"},"description":"group names, e.g. [\"web\"]"}},"additionalProperties":false}`)}
}

func toolsLoadArgs(args json.RawMessage) ([]string, error) {
	var a struct {
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	return a.Groups, nil
}

// runToolsLoad serves tools_load: every named group must be a group of
// this view, or nothing loads and the error lists the on-demand groups.
func (r *runner) runToolsLoad(args json.RawMessage) model.ToolResult {
	names, err := toolsLoadArgs(args)
	if err != nil {
		return invalidArgs(err)
	}
	groups := r.deps.Tools.Groups()
	known := map[string]model.ToolGroup{}
	for _, g := range groups {
		known[g.Name] = g
	}
	members := map[string][]string{}
	for _, t := range r.deps.Tools.Tools() {
		members[t.Group] = append(members[t.Group], t.Name)
	}
	var unknown []string
	for _, n := range names {
		if _, ok := known[n]; !ok || len(members[n]) == 0 {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		var avail []string
		for _, g := range groups {
			if !g.LoadAlways && len(members[g.Name]) > 0 {
				avail = append(avail, g.Name)
			}
		}
		res := model.Fail(model.CodeNotFound, "unknown_group",
			"unknown tool group(s): %s; nothing loaded. On-demand groups: %s",
			strings.Join(unknown, ", "), strings.Join(avail, ", "))
		res.Code = "unknown_group"
		return res
	}
	var b strings.Builder
	for _, n := range names {
		switch {
		case known[n].LoadAlways:
			fmt.Fprintf(&b, "%s: always available: %s\n", n, strings.Join(members[n], ", "))
		case r.load(n):
			fmt.Fprintf(&b, "%s: loaded: %s\n", n, strings.Join(members[n], ", "))
		default:
			fmt.Fprintf(&b, "%s: already loaded: %s\n", n, strings.Join(members[n], ", "))
		}
	}
	return model.ToolResult{Text: strings.TrimRight(b.String(), "\n")}
}

// unknownTool is the failure for a call to a tool that is not advertised
// (not_found, in the error envelope). Under progressive loading a tool of
// an unloaded group names the group to load (reason tool_not_loaded).
// Otherwise it suggests up to maxSuggestions near names (suggestTools)
// among the advertised tools and those of unloaded groups, naming the
// tools_load call any unloaded suggestion needs.
func (r *runner) unknownTool(name string) model.ToolResult {
	unloaded := r.unloadedTools()
	if g, ok := unloaded[name]; ok {
		return model.Fail(model.CodeNotFound, "tool_not_loaded",
			"tool %s is in tool group %q, which is not loaded: call %s with {\"groups\":[%q]} first", name, g, toolsLoadName, g)
	}
	return model.Fail(model.CodeNotFound, "unknown_tool", "%s", r.unknownToolMessage(name, unloaded))
}

// unknownToolMessage is the unknown-tool failure's message: near names to
// call instead, and the tools_load call any of them needs.
func (r *runner) unknownToolMessage(name string, unloaded map[string]string) string {
	var names []string
	for _, t := range r.toolList() {
		names = append(names, t.Name)
	}
	for n := range unloaded {
		names = append(names, n)
	}
	sort.Strings(names) // unloaded is a map: keep ties deterministic
	sugg := suggestTools(name, names)
	if len(sugg) == 0 {
		msg := "unknown tool: " + name + ". Call only tools in your tool list"
		if r.progressive {
			msg += ", or load an on-demand group with " + toolsLoadName + " (see \"Tool groups\" in your instructions)"
		}
		return msg + "."
	}
	msg := "unknown tool: " + name + ". Did you mean "
	if len(sugg) > 1 {
		msg += "one of: "
	}
	msg += strings.Join(sugg, ", ") + "?"
	var need, groups []string
	for _, s := range sugg {
		if g, ok := unloaded[s]; ok {
			need = append(need, s)
			if !slices.Contains(groups, g) {
				groups = append(groups, g)
			}
		}
	}
	if len(need) > 0 {
		quoted, _ := json.Marshal(groups)
		msg += fmt.Sprintf(" %s not loaded yet: call %s with {\"groups\":%s} first.", strings.Join(need, ", "), toolsLoadName, quoted)
	}
	return msg
}

// unloadedTools maps each tool of an on-demand group not yet loaded to
// its group. Empty without progressive loading.
func (r *runner) unloadedTools() map[string]string {
	out := map[string]string{}
	if !r.progressive || r.deps.Tools == nil {
		return out
	}
	onDemand := onDemandGroups(r.deps.Tools.Groups())
	for _, g := range r.loadedGroups() {
		delete(onDemand, g)
	}
	for _, t := range r.deps.Tools.Tools() {
		if onDemand[t.Group] {
			out[t.Name] = t.Group
		}
	}
	return out
}

// invalidArgs is the failure for arguments a built-in cannot decode.
func invalidArgs(err error) model.ToolResult {
	return model.Fail(model.CodeInvalid, "invalid_args", "invalid arguments: %v", err)
}

// toolCatalogue renders the tool-group catalogue (§4.3): one line per
// group with tools in the view — name, tool count, description — sorted
// by name, so the same view always yields the same text. Under
// progressive loading it marks the groups that need tools_load. Empty
// when no tool names a described group.
func toolCatalogue(tools []model.Tool, groups []model.ToolGroup, progressive bool) string {
	count := map[string]int{}
	for _, t := range tools {
		count[t.Group]++
	}
	sorted := append([]model.ToolGroup(nil), groups...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var lines []string
	for _, g := range sorted {
		n := count[g.Name]
		if n == 0 {
			continue
		}
		unit := "tools"
		if n == 1 {
			unit = "tool"
		}
		mark := ""
		if progressive && !g.LoadAlways {
			mark = ", on demand"
		}
		lines = append(lines, fmt.Sprintf("- `%s` (%d %s%s): %s", g.Name, n, unit, mark, g.Description))
	}
	if len(lines) == 0 {
		return ""
	}
	intro := "Your tools come in groups. Each line names a group, how many tools it has, and what they are for; use it to find the right tools for a task."
	if progressive {
		intro += " Groups marked \"on demand\" are not in your tool list yet: call `" + toolsLoadName + "` with the groups the task needs, and their tools stay available for the rest of the conversation."
	}
	return "## Tool groups\n\n" + intro + "\n\n" + strings.Join(lines, "\n")
}

// withCatalogue appends the tool catalogue to the projection's system
// item (or adds one) for this request only: the stored transcript is
// never edited (rule 1), and a thread that outlives a change to the
// view gets the current catalogue on its next step.
func (r *runner) withCatalogue(projected []model.Item) []model.Item {
	if r.deps.Tools == nil {
		return projected
	}
	cat := toolCatalogue(r.deps.Tools.Tools(), r.deps.Tools.Groups(), r.progressive)
	if cat == "" {
		return projected
	}
	out := append([]model.Item(nil), projected...)
	for i := range out {
		if out[i].Kind == model.KindSystem {
			out[i].Text = strings.TrimRight(out[i].Text, "\n") + "\n\n" + cat
			return out
		}
	}
	return append([]model.Item{{Kind: model.KindSystem, Text: cat}}, out...)
}
