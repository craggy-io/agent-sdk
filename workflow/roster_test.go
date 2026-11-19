package workflow_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace/memory"
)

// rosterWorkflow runs the roster's first agent on the message, then its own
// coordinator on what came back.
const rosterWorkflow = `WORKFLOW = workflow(
    chat = True,
    roles = "roster",
    input = {
        "message": schema.string(required = True),
        "roster": schema.list(schema.object({"name": schema.string(), "description": schema.string()})),
    },
)

def run(input):
    first = agent(input.roster[0]["name"], task = input.message, phase = "answer")
    return agent("coordinator", task = render("Sum up: {t}", t = first.text), phase = "reply")
`

// reactLoop samples, runs what was asked, and samples again until done.
const reactLoop = `def run(sample, max_steps):
    for _ in range(max_steps):
        outcome, act = sample()
        if outcome == "actions":
            outcome = act()
        if outcome != "continue":
            return
`

// writeRosterBundles writes two bundles under one folder: local.team, whose
// agents know nothing of any workflow, and local.group, a roster workflow
// with its own coordinator. workflow is the group's workflow.star.
func writeRosterBundles(t *testing.T, workflow string) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("team/bundle.yaml", `apiVersion: craggy.io/bundle/v1
metadata: {id: local.team, version: 1.0.0}
contributions:
  - {id: analyst, kind: agentsdk.agent@1, path: analyst.yaml}
  - {id: fixer, kind: agentsdk.agent@1, path: fixer.yaml}
  - {id: outsider, kind: agentsdk.agent@1, path: outsider.yaml}
`)
	write("team/analyst.yaml", "name: analyst\ndescription: Reads the code\nloop: loop.star\n")
	write("team/fixer.yaml", "name: fixer\ndescription: Writes the patch\nloop: loop.star\n")
	write("team/outsider.yaml", "name: outsider\ndescription: Not asked\nloop: loop.star\n")
	write("team/loop.star", reactLoop)
	write("group/bundle.yaml", `apiVersion: craggy.io/bundle/v1
metadata: {id: local.group, version: 1.0.0}
contributions:
  - {id: coordinator, kind: agentsdk.agent@1, path: coordinator.yaml}
  - {id: group-chat, kind: agentsdk.workflow@1, path: workflow.star}
`)
	write("group/coordinator.yaml", "name: coordinator\nloop: loop.star\n")
	write("group/loop.star", reactLoop)
	write("group/workflow.star", workflow)
	return root
}

func openRosterHost(t *testing.T, root string, provider model.Provider) *host.Host {
	t.Helper()
	h, err := host.Open(t.Context(), host.Options{Config: config.Defaults(), Defs: openDefs(t, root), Provider: provider, Trace: memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// toolDescription is what the tool name offered in req says of its name
// argument, or "" when it isn't offered.
func toolDescription(req model.Request, name string) string {
	for _, tool := range req.Tools {
		if tool.Name == name {
			var schema struct {
				Properties map[string]struct{ Description string } `json:"properties"`
			}
			_ = json.Unmarshal(tool.Schema, &schema)
			return schema.Properties["name"].Description
		}
	}
	return ""
}

// A roster workflow runs agents of another bundle, named by the turn, and
// each of them may call the others in the roster as subagents.
func TestRosterChatRunsAndDelegates(t *testing.T) {
	root := writeRosterBundles(t, rosterWorkflow)
	provider := agenttest.NewScripted(
		agenttest.CallsResponse(model.ToolCall{ID: "d1", Name: "agent", ArgsOK: true, Args: json.RawMessage(`{"name":"fixer","task":"Patch it."}`)}),
		agenttest.TextResponse("patched"),
		agenttest.TextResponse("The fixer patched it."),
		agenttest.TextResponse("Patched."),
	)
	h := openRosterHost(t, root, provider)
	result, err := h.RunChat(t.Context(), "group-chat", "Fix the crash.", host.WithThread("chat-1"), host.WithGroup("analyst", "fixer"))
	if err != nil || result.Status != "done" || result.Text != "Patched." {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(provider.Requests) != 4 {
		t.Fatalf("requests=%d", len(provider.Requests))
	}
	// The analyst, then the fixer it started, are told whom they may call.
	for i, who := range []string{"analyst", "fixer"} {
		desc := toolDescription(provider.Requests[i], "agent")
		if !strings.Contains(desc, "analyst") || !strings.Contains(desc, "fixer") || strings.Contains(desc, "outsider") {
			t.Errorf("%s was offered agent with name %q", who, desc)
		}
	}
	// The workflow's own coordinator runs in the group too.
	if desc := toolDescription(provider.Requests[3], "agent"); !strings.Contains(desc, "fixer") {
		t.Errorf("coordinator was offered agent with name %q", desc)
	}
}

// A role completes against the output a chat workflow asks of it, and the
// workflow reads it back.
func TestChatRoleOutput(t *testing.T) {
	root := writeRosterBundles(t, `WORKFLOW = workflow(
    chat = True,
    roles = "roster",
    input = {
        "message": schema.string(required = True),
        "roster": schema.list(schema.object({"name": schema.string(), "description": schema.string()})),
    },
)

def run(input):
    plan = agent("coordinator", task = input.message, output = schema.object({"agents": schema.list(schema.string())}))
    return agent(plan.output["agents"][0], task = input.message)
`)
	provider := agenttest.NewScripted(
		agenttest.CallsResponse(model.ToolCall{ID: "c1", Name: "complete", ArgsOK: true, Args: json.RawMessage(`{"output":{"agents":["fixer"]}}`)}),
		agenttest.TextResponse("fixed"),
	)
	h := openRosterHost(t, root, provider)
	result, err := h.RunChat(t.Context(), "group-chat", "Fix it.", host.WithGroup("analyst", "fixer"))
	if err != nil || result.Status != "done" || result.Text != "fixed" || result.DefName != "fixer" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// Agents of the roster answer side by side with parallel and spec.
func TestRosterChatParallel(t *testing.T) {
	root := writeRosterBundles(t, `WORKFLOW = workflow(
    chat = True,
    roles = "roster",
    input = {
        "message": schema.string(required = True),
        "roster": schema.list(schema.object({"name": schema.string(), "description": schema.string()})),
    },
)

def run(input):
    answers = parallel([spec(a["name"], task = input.message) for a in input.roster], phase = "answer")
    return agent("coordinator", task = "\n".join([r.text for r in answers]))
`)
	provider := agenttest.NewScripted(agenttest.TextResponse("seen"), agenttest.TextResponse("seen"), agenttest.TextResponse("Both saw it."))
	h := openRosterHost(t, root, provider)
	result, err := h.RunChat(t.Context(), "group-chat", "Look.", host.WithThread("chat-2"), host.WithGroup("analyst", "fixer"))
	if err != nil || result.Status != "done" || result.Text != "Both saw it." {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := provider.Requests[2].Items; !strings.Contains(lastText(got), "seen\nseen") {
		t.Errorf("coordinator was given %q", lastText(got))
	}
}

// allText is every text of items, one per line.
func allText(items []model.Item) string {
	var out []string
	for _, item := range items {
		if item.Text != "" {
			out = append(out, item.Text)
		}
	}
	return strings.Join(out, "\n")
}

// Each agent keeps its own thread in the chat from turn to turn, and an
// agent of the group started as a subagent carries on in that thread too.
func TestRosterAgentsKeepTheirHistory(t *testing.T) {
	root := writeRosterBundles(t, rosterWorkflow)
	delegate := func(id, task string) model.Response {
		return agenttest.CallsResponse(model.ToolCall{ID: id, Name: "agent", ArgsOK: true, Args: json.RawMessage(`{"name":"fixer","task":"` + task + `"}`)})
	}
	provider := agenttest.NewScripted(
		delegate("d1", "Patch the parser."), agenttest.TextResponse("patched"), agenttest.TextResponse("done 1"), agenttest.TextResponse("reply 1"),
		delegate("d2", "Patch it again."), agenttest.TextResponse("patched again"), agenttest.TextResponse("done 2"), agenttest.TextResponse("reply 2"),
	)
	h := openRosterHost(t, root, provider)
	for _, message := range []string{"Fix the crash.", "And the other one."} {
		result, err := h.RunChat(t.Context(), "group-chat", message, host.WithThread("chat-3"), host.WithGroup("analyst", "fixer"))
		if err != nil || result.Status != "done" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	if len(provider.Requests) != 8 {
		t.Fatalf("requests=%d", len(provider.Requests))
	}
	if seen := allText(provider.Requests[4].Items); !strings.Contains(seen, "Fix the crash.") {
		t.Errorf("the analyst forgot the first turn: %q", seen)
	}
	if seen := allText(provider.Requests[5].Items); !strings.Contains(seen, "Patch the parser.") || !strings.Contains(seen, "patched") {
		t.Errorf("the fixer forgot what it was asked before: %q", seen)
	}
}

func lastText(items []model.Item) string {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Text != "" {
			return items[i].Text
		}
	}
	return ""
}

func TestRosterChatRefusals(t *testing.T) {
	outsider := strings.Replace(rosterWorkflow, `agent(input.roster[0]["name"]`, `agent("outsider"`, 1)
	cases := []struct {
		name, workflow string
		group          []string
		want           string
	}{
		{"no roster", rosterWorkflow, nil, "names none"},
		{"not installed", rosterWorkflow, []string{"ghost"}, `"ghost"`},
		{"not in the roster", outsider, []string{"analyst"}, `roster does not name it`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := openRosterHost(t, writeRosterBundles(t, c.workflow), agenttest.NewScripted(agenttest.TextResponse("x"), agenttest.TextResponse("x")))
			_, err := h.RunChat(t.Context(), "group-chat", "Hi.", host.WithGroup(c.group...))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err=%v, want %q", err, c.want)
			}
		})
	}
	// A workflow that names its own roles takes no roster.
	h := openRosterHost(t, writeChatBundle(t, "def run(step, max_steps):\n    step()\n"), agenttest.NewScripted())
	if _, err := h.RunChat(t.Context(), "test-chat", "Hi.", host.WithGroup("writer")); err == nil || !strings.Contains(err.Error(), "does not take a roster") {
		t.Fatalf("err=%v", err)
	}
}

func TestRolesDeclaration(t *testing.T) {
	for _, wf := range []string{
		"WORKFLOW = workflow(roles = \"roster\")\ndef run(input):\n    pass\n",
		"WORKFLOW = workflow(chat = True, roles = \"all\")\ndef run(input):\n    pass\n",
	} {
		root := writeRosterBundles(t, wf)
		if _, err := agentdef.Open(t.Context(), agentdef.Options{Project: root}); err == nil || !strings.Contains(err.Error(), "roles") {
			t.Errorf("%q: err=%v", wf, err)
		}
	}
}
