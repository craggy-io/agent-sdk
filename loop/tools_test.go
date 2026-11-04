package loop_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
)

// groupedTools is a view with an always-loaded group (jobs), two
// on-demand groups (web, apk) and one ungrouped tool.
func groupedTools() *fakeTools {
	ft := newFakeTools().echoTools("jobs_wait", "web_fetch", "web_search", "apk_decompile", "loose")
	group := map[string]string{"jobs_wait": "jobs", "web_fetch": "web", "web_search": "web", "apk_decompile": "apk"}
	for i := range ft.defs {
		ft.defs[i].Group = group[ft.defs[i].Name]
	}
	ft.groups = []model.ToolGroup{
		{Name: "web", Description: "Search the public web and capture pages."},
		{Name: "jobs", Description: "Follow long-running jobs.", LoadAlways: true},
		{Name: "apk", Description: "Android APK decompilation."},
	}
	return ft
}

func toolNames(tools []model.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

func systemText(req model.Request) string {
	for _, it := range req.Items {
		if it.Kind == model.KindSystem {
			return it.Text
		}
	}
	return ""
}

// Without progressive loading the whole view is advertised and the system
// prompt gains a deterministic, name-sorted catalogue (§4.3).
func TestToolCatalogueInSystemPrompt(t *testing.T) {
	prov := agenttest.NewScripted(agenttest.TextResponse("done"))
	h, _ := runLoop(t, prov, groupedTools(), loop.Policy{}, nil)
	req := prov.Requests[0]
	want := "you are a test agent\n\n## Tool groups\n\n" +
		"Your tools come in groups. Each line names a group, how many tools it has, and what they are for; use it to find the right tools for a task.\n\n" +
		"- `apk` (1 tool): Android APK decompilation.\n" +
		"- `jobs` (1 tool): Follow long-running jobs.\n" +
		"- `web` (2 tools): Search the public web and capture pages."
	if got := systemText(req); got != want {
		t.Fatalf("system prompt:\n%s\nwant:\n%s", got, want)
	}
	if got := strings.Join(toolNames(req.Tools), ","); !strings.HasPrefix(got, "jobs_wait,web_fetch,web_search,apk_decompile,loose,") || strings.Contains(got, "tools_load") {
		t.Fatalf("tools = %s", got)
	}
	// The catalogue is projected per request, never stored in the thread.
	if strings.Contains(h.items()[0].Text, "Tool groups") {
		t.Fatal("catalogue written into the transcript")
	}
}

// Progressive loading advertises LoadAlways and ungrouped tools plus
// tools_load; loading appends a group's tools after the built-ins and a
// call to an unloaded tool names the group to load.
func TestProgressiveToolsLoadOnDemand(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "web_fetch", `{}`)),
		agenttest.CallsResponse(call("b", "tools_load", `{"groups":["web","nosuch"]}`)),
		agenttest.CallsResponse(call("c", "tools_load", `{"groups":["web","jobs"]}`)),
		agenttest.CallsResponse(call("d", "web_fetch", `{}`)),
		agenttest.TextResponse("done"),
	)
	ft := groupedTools()
	h, res := runLoop(t, prov, ft, loop.Policy{ProgressiveTools: true}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	first := strings.Join(toolNames(prov.Requests[0].Tools), ",")
	if strings.Contains(first, "web_") || strings.Contains(first, "apk_") ||
		!strings.HasPrefix(first, "jobs_wait,loose,") || !strings.HasSuffix(first, ",tools_load") {
		t.Fatalf("initial tools = %s", first)
	}
	if sys := systemText(prov.Requests[0]); !strings.Contains(sys, "- `web` (2 tools, on demand): ") ||
		!strings.Contains(sys, "- `jobs` (1 tool): ") || !strings.Contains(sys, "call `tools_load`") {
		t.Fatalf("progressive catalogue:\n%s", sys)
	}
	rs := resultsOf(h)
	var env struct{ Error model.ToolError }
	if err := json.Unmarshal([]byte(rs[0].Text), &env); err != nil || env.Error.Code != model.CodeNotFound ||
		env.Error.Reason != "tool_not_loaded" || !strings.Contains(env.Error.Message, `tool web_fetch is in tool group "web", which is not loaded`) {
		t.Fatalf("unloaded call result = %q", rs[0].Text)
	}
	if !rs[1].IsError || !strings.Contains(rs[1].Text, "nosuch") || !strings.Contains(rs[1].Text, "On-demand groups: web, apk") {
		t.Fatalf("bad load result = %+v", rs[1])
	}
	if rs[2].IsError || rs[2].Text != "web: loaded: web_fetch, web_search\njobs: always available: jobs_wait" {
		t.Fatalf("load result = %+v", rs[2])
	}
	if rs[3].IsError || rs[3].Text != "ok:web_fetch" {
		t.Fatalf("loaded call result = %+v", rs[3])
	}
	// The failed load added nothing; the good one appended after built-ins,
	// leaving the earlier prefix unchanged.
	if got := strings.Join(toolNames(prov.Requests[2].Tools), ","); got != first {
		t.Fatalf("tools after failed load = %s", got)
	}
	if got := strings.Join(toolNames(prov.Requests[3].Tools), ","); got != first+",web_fetch,web_search" {
		t.Fatalf("tools after load = %s", got)
	}
	if got := ft.called(); len(got) != 1 || got[0] != "web_fetch" {
		t.Fatalf("runner calls = %v", got)
	}
}

// A later run on the same thread recovers the groups earlier runs loaded.
func TestProgressiveToolsRecoveredFromThread(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "tools_load", `{"groups":["apk"]}`)),
		agenttest.TextResponse("loaded"),
		agenttest.TextResponse("again"),
	)
	ft := groupedTools()
	h, _ := runLoop(t, prov, ft, loop.Policy{ProgressiveTools: true}, nil)
	_, res := runLoop(t, prov, ft, loop.Policy{ProgressiveTools: true}, func(in *loop.Input, _ *loop.Deps) {
		in.Thread = h.thread
		in.Task = "next turn"
	})
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	if got := toolNames(prov.Requests[2].Tools); got[len(got)-1] != "apk_decompile" {
		t.Fatalf("second run tools = %v", got)
	}
}

// Progressive loading with nothing to defer behaves as the whole view.
func TestProgressiveToolsNoOnDemandGroups(t *testing.T) {
	ft := newFakeTools().echoTools("a", "b")
	prov := agenttest.NewScripted(agenttest.TextResponse("done"))
	runLoop(t, prov, ft, loop.Policy{ProgressiveTools: true}, nil)
	if got := strings.Join(toolNames(prov.Requests[0].Tools), ","); !strings.HasPrefix(got, "a,b") || strings.Contains(got, "tools_load") {
		t.Fatalf("tools = %s", got)
	}
	if strings.Contains(systemText(prov.Requests[0]), "Tool groups") {
		t.Fatal("catalogue without groups")
	}
}

// An unknown tool name gets near-name suggestions; a suggestion in an
// unloaded group names the tools_load call that adds it.
func TestUnknownToolSuggestions(t *testing.T) {
	prov := agenttest.NewScripted(
		agenttest.CallsResponse(call("a", "jobs_wiat", `{}`)),
		agenttest.CallsResponse(call("b", "web_fech", `{}`)),
		agenttest.CallsResponse(call("c", "frobnicate", `{}`)),
		agenttest.TextResponse("done"),
	)
	h, res := runLoop(t, prov, groupedTools(), loop.Policy{ProgressiveTools: true}, nil)
	if res.Status != "done" {
		t.Fatalf("status=%q", res.Status)
	}
	rs := resultsOf(h)
	want := []string{
		"unknown tool: jobs_wiat. Did you mean jobs_wait?",
		`unknown tool: web_fech. Did you mean web_fetch? web_fetch not loaded yet: call tools_load with {"groups":["web"]} first.`,
		`unknown tool: frobnicate. Call only tools in your tool list, or load an on-demand group with tools_load (see "Tool groups" in your instructions).`,
	}
	for i, w := range want {
		if !rs[i].IsError || rs[i].Text != model.Fail(model.CodeNotFound, "unknown_tool", "%s", w).Text {
			t.Errorf("result %d = %q, want the not_found envelope with message %q", i, rs[i].Text, w)
		}
	}
}
