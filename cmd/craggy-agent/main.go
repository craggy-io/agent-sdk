// Command craggy-agent is the trace debugging CLI of design §7.3, plus a
// live `run` subcommand (the key-handoff smoke path: set
// OPENROUTER_API_KEY and `craggy-agent run assistant "hello"`).
//
// Query commands are read-only over a trace SQLite file (--db). IDs may
// be given as unique prefixes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/craggy-io/agent-sdk/adapters/configyaml"
	"github.com/craggy-io/agent-sdk/adapters/tracesqlite"
	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/defs"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/openrouter"
	"github.com/craggy-io/agent-sdk/trace"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/trace/query"
	"github.com/craggy-io/agent-sdk/workflow"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type globals struct {
	db   string
	root string
	mem  bool
}

// run is the in-process entry (tests drive it directly).
func run(args []string, stdout, stderr io.Writer) int {
	g, rest, err := takeGlobals(args)
	if err != nil {
		fmt.Fprintln(stderr, "craggy-agent:", err)
		return 2
	}
	if len(rest) == 0 {
		usage(stdout)
		return 2
	}
	cmd, cargs := rest[0], rest[1:]
	ctx := context.Background()
	var runErr error
	switch cmd {
	case "run":
		runErr = cmdRun(ctx, g, cargs, stdout)
	case "resume":
		runErr = cmdResume(ctx, g, cargs, stdout)
	case "runs", "tree", "step", "tools", "usage", "replay", "grep", "inputs", "recovery", "tail", "agents":
		runErr = withSink(g, func(sink trace.Sink) error {
			return queryCmd(ctx, sink, g, cmd, cargs, stdout)
		})
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "craggy-agent: unknown command %q\n", cmd)
		usage(stderr)
		return 2
	}
	if runErr != nil {
		fmt.Fprintln(stderr, "craggy-agent:", runErr)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `craggy-agent - trace debugging for agent-sdk (docs/cli.md)

Usage: craggy-agent [--db trace.db] <command> [args]

Live:
  run [--root res] [--db trace.db|--mem] [--config host.yaml]
      (--agent <name>|--workflow <name>) <task...>
      Runs an agent or workflow via OpenRouter (needs OPENROUTER_API_KEY).
  resume [--root res] --db trace.db <workflow-run-id>
      Resumes a journaled workflow run.
  --root (default: a res/ directory here or up to 3 levels above) is the
  project definition layer: a tree of independently identified bundles. Each
  bundle has a bundle.yaml declaring its agents, skills or workflows. Higher
  layers replace lower definitions by kind and name; same-layer conflicts fail.

Queries (--db <sqlite trace>):
  runs                         list runs
  tree <run>                   node tree of a run
  step <step> --context        exact request context of a step
  step <step> --diff           what changed from the node's previous step
  tools <node>                 advertised tools and out-of-view attempts
  usage [<run>] [--node <id>] [--session <id>] [--by model|run|node|tool] [--json]
                               token/cost rollups (steps, cached, reasoning, $)
  replay <step> [--model m] [--print]   resend a step's exact request
  grep <run> [kind...]         anomalies: repair|retry|loop|fallback|<type>
  inputs <thread>              mailbox receipts for a thread/handle
  recovery [<run>]             operations with unknown outcomes
  recovery resolve <op> --decision success|failure|retry [--evidence ..]
  tail [<session>] [--after N] [--once] follow events
  agents [<session>]           agent handles and state
`)
}

func takeGlobals(args []string) (globals, []string, error) {
	g := globals{db: "trace.db"}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--db" && i+1 < len(args):
			g.db, i = args[i+1], i+1
		case strings.HasPrefix(a, "--db="):
			g.db = strings.TrimPrefix(a, "--db=")
		case a == "--root" && i+1 < len(args):
			g.root, i = args[i+1], i+1
		case strings.HasPrefix(a, "--root="):
			g.root = strings.TrimPrefix(a, "--root=")
		case a == "--mem":
			g.mem = true
		default:
			rest = append(rest, a)
		}
	}
	return g, rest, nil
}

func withSink(g globals, fn func(trace.Sink) error) error {
	if g.mem {
		return fmt.Errorf("--mem is only meaningful for `run` (queries need --db)")
	}
	store, err := tracesqlite.Open(g.db)
	if err != nil {
		return err
	}
	defer store.Close()
	return fn(store)
}

func allEvents(ctx context.Context, sink trace.Sink) ([]trace.Event, error) {
	return query.Events(ctx, sink, query.Filter{})
}

// resolveID maps a unique ID prefix to the full ID among candidates.
func resolveID(want string, candidates []string) (string, error) {
	seen := map[string]bool{}
	var matches []string
	for _, c := range candidates {
		if c == want {
			return c, nil
		}
		if strings.HasPrefix(c, want) && !seen[c] {
			seen[c] = true
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no id matches %q", want)
	default:
		return "", fmt.Errorf("id prefix %q is ambiguous (%d matches)", want, len(matches))
	}
}

func runIDs(evs []trace.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.RunID)
	}
	return out
}

func nodeIDs(evs []trace.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.NodeID)
	}
	return out
}

func stepIDs(evs []trace.Event) []string {
	var out []string
	for _, e := range evs {
		if e.StepID != "" {
			out = append(out, e.StepID)
		}
	}
	return out
}

func queryCmd(ctx context.Context, sink trace.Sink, g globals, cmd string, args []string, w io.Writer) error {
	switch cmd {
	case "runs":
		runs, err := query.Runs(ctx, sink)
		if err != nil {
			return err
		}
		for _, r := range runs {
			fmt.Fprintf(w, "%s  %-10s session=%s events=%d start=%s %s\n",
				r.RunID, r.Status, short(r.SessionID), r.Events,
				r.Start.Format(time.RFC3339), oneLine(string(r.Data), 90))
		}
		if len(runs) == 0 {
			fmt.Fprintln(w, "no runs")
		}
		return nil
	case "tree":
		if len(args) < 1 {
			return fmt.Errorf("tree needs a run id")
		}
		evs, err := allEvents(ctx, sink)
		if err != nil {
			return err
		}
		rid, err := resolveID(args[0], runIDs(evs))
		if err != nil {
			return err
		}
		nodes, err := query.Tree(ctx, sink, rid)
		if err != nil {
			return err
		}
		printTree(w, nodes, evs, rid)
		return nil
	case "step":
		return cmdStep(ctx, sink, args, w)
	case "tools":
		return cmdTools(ctx, sink, args, w)
	case "usage":
		return cmdUsage(ctx, sink, args, w)
	case "replay":
		return cmdReplay(ctx, sink, args, w)
	case "grep":
		return cmdGrep(ctx, sink, args, w)
	case "inputs":
		return cmdInputs(ctx, sink, args, w)
	case "recovery":
		return cmdRecovery(ctx, sink, g, args, w)
	case "tail":
		return cmdTail(ctx, sink, args, w)
	case "agents":
		return cmdAgents(ctx, sink, args, w)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// printTree renders nodes indented by parentage with per-node step/cost
// counts folded from the event stream (§7.3).
func printTree(w io.Writer, nodes []query.NodeInfo, evs []trace.Event, runID string) {
	steps := map[string]int{}
	cost := map[string]float64{}
	for _, e := range evs {
		if e.RunID != runID {
			continue
		}
		if e.Type == "step.response" {
			steps[e.NodeID]++
			var d struct {
				Usage model.Usage `json:"usage"`
			}
			if json.Unmarshal(e.Data, &d) == nil {
				cost[e.NodeID] += d.Usage.CostUSD
			}
		}
	}
	byParent := map[string][]query.NodeInfo{}
	var roots []query.NodeInfo
	known := map[string]bool{}
	for _, n := range nodes {
		known[n.NodeID] = true
	}
	for _, n := range nodes {
		if n.Parent == "" || !known[n.Parent] {
			roots = append(roots, n)
		} else {
			byParent[n.Parent] = append(byParent[n.Parent], n)
		}
	}
	var walk func(n query.NodeInfo, depth int)
	walk = func(n query.NodeInfo, depth int) {
		var dd struct {
			Def   string `json:"def"`
			Model string `json:"model"`
		}
		_ = json.Unmarshal(n.Data, &dd)
		fmt.Fprintf(w, "%s%s %s def=%s model=%s steps=%d cost=%.4f status=%s\n",
			strings.Repeat("  ", depth), n.NodeID, n.Kind, dd.Def, dd.Model,
			steps[n.NodeID], cost[n.NodeID], n.Status)
		for _, c := range byParent[n.NodeID] {
			walk(c, depth+1)
		}
	}
	if len(nodes) == 0 {
		fmt.Fprintln(w, "no nodes for run", runID)
	}
	for _, r := range roots {
		walk(r, 0)
	}
}

// stepRequest loads one step's items and params from the trace (§7.1).
func stepRequest(ctx context.Context, sink trace.Sink, evs []trace.Event, stepID string) ([]model.Item, trace.RequestParams, trace.StepRequestData, error) {
	var reqEv *trace.Event
	for i := range evs {
		if evs[i].Type == "step.request" && evs[i].StepID == stepID {
			reqEv = &evs[i]
		}
	}
	if reqEv == nil {
		return nil, trace.RequestParams{}, trace.StepRequestData{},
			fmt.Errorf("no step.request for step %s", stepID)
	}
	var data trace.StepRequestData
	if err := json.Unmarshal(reqEv.Data, &data); err != nil {
		return nil, trace.RequestParams{}, data, err
	}
	items := make([]model.Item, 0, len(data.Items))
	for _, h := range data.Items {
		it, err := trace.GetItem(ctx, sink, h)
		if err != nil {
			return nil, trace.RequestParams{}, data, err
		}
		items = append(items, it)
	}
	var params trace.RequestParams
	if data.Params != "" {
		b, err := sink.GetBlob(ctx, data.Params)
		if err != nil {
			return nil, params, data, err
		}
		if err := json.Unmarshal(b, &params); err != nil {
			return nil, params, data, err
		}
	}
	return items, params, data, nil
}

func roleOf(it model.Item) string {
	switch it.Kind {
	case model.KindSystem:
		return "system"
	case model.KindAssistant:
		return "assistant"
	case model.KindToolResult:
		return "tool"
	default:
		return "user"
	}
}

func printItem(w io.Writer, it model.Item) {
	fmt.Fprintf(w, "%4d %-9s (%s)", it.Seq, roleOf(it), it.Kind)
	if it.ServedBy != "" {
		fmt.Fprintf(w, " served_by=%s finish=%s", it.ServedBy, it.Finish)
	}
	if it.CallID != "" {
		fmt.Fprintf(w, " call=%s settled=%s error=%v", it.CallID, it.Settled, it.IsError)
	}
	fmt.Fprintln(w)
	if it.Text != "" {
		for _, line := range strings.Split(it.Text, "\n") {
			fmt.Fprintf(w, "     | %s\n", line)
		}
	}
	for _, c := range it.Calls {
		fmt.Fprintf(w, "     | -> %s(%s) id=%s args_ok=%v\n", c.Name, string(c.Args), c.ID, c.ArgsOK)
	}
	if it.Reasoning != nil && it.Reasoning.Text != "" {
		fmt.Fprintf(w, "     | reasoning: %s\n", oneLine(it.Reasoning.Text, 200))
	}
	if len(it.Covers) > 0 {
		fmt.Fprintf(w, "     | covers=%v supersedes=%v\n", it.Covers, it.Supersedes)
	}
}

func cmdStep(ctx context.Context, sink trace.Sink, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("step", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	showCtx := fs.Bool("context", false, "print the exact request context")
	showDiff := fs.Bool("diff", false, "diff against the node's previous step")
	var positional []string
	// flag parses after the first positional in Go; accept both orders.
	for _, a := range args {
		switch a {
		case "--context", "-context":
			*showCtx = true
		case "--diff", "-diff":
			*showDiff = true
		default:
			if !strings.HasPrefix(a, "-") {
				positional = append(positional, a)
			}
		}
	}
	if len(positional) < 1 {
		return fmt.Errorf("step needs a step id")
	}
	evs, err := allEvents(ctx, sink)
	if err != nil {
		return err
	}
	sid, err := resolveID(positional[0], stepIDs(evs))
	if err != nil {
		return err
	}
	items, params, data, err := stepRequest(ctx, sink, evs, sid)
	if err != nil {
		return err
	}
	if *showDiff {
		return printStepDiff(ctx, sink, evs, sid, data, w)
	}
	// Default and --context: the exact request (§7.3).
	_ = *showCtx
	fmt.Fprintf(w, "step %s\nmodel: %s fallbacks: %v\n", sid, params.Model, params.Fallbacks)
	fmt.Fprintf(w, "reasoning: effort=%s max_tokens=%d  max_output_tokens: %d  tool_choice: %s parallel: %v cache: %s\n",
		params.Reasoning.Effort, params.Reasoning.MaxTokens, params.MaxOutputTokens,
		params.ToolChoice, params.ParallelToolCalls, params.Cache)
	fmt.Fprintf(w, "provider: require_parameters=%v data_collection=%s sort=%s\n",
		params.Provider.RequireParameters, params.Provider.DataCollection, params.Provider.Sort)
	if len(data.Repairs) > 0 {
		fmt.Fprintf(w, "repairs: %s\n", strings.Join(data.Repairs, "; "))
	}
	fmt.Fprintf(w, "tools (%d):\n", len(params.Tools))
	for _, t := range params.Tools {
		fmt.Fprintf(w, "  - %s [%s] %s\n", t.Name, t.Effect, oneLine(t.Description, 80))
	}
	fmt.Fprintf(w, "messages (%d):\n", len(items))
	for _, it := range items {
		printItem(w, it)
	}
	// Response summary, when recorded.
	for _, e := range evs {
		if e.StepID == sid && e.Type == "step.response" {
			fmt.Fprintf(w, "response: %s\n", oneLine(string(e.Data), 300))
		}
	}
	return nil
}

func printStepDiff(ctx context.Context, sink trace.Sink, evs []trace.Event, stepID string, data trace.StepRequestData, w io.Writer) error {
	// Node of this step, then the previous step.request on the same node.
	node := ""
	cursor := int64(0)
	for _, e := range evs {
		if e.Type == "step.request" && e.StepID == stepID {
			node, cursor = e.NodeID, e.Cursor
		}
	}
	var prev *trace.Event
	for i := range evs {
		e := &evs[i]
		if e.Type == "step.request" && e.NodeID == node && e.Cursor < cursor {
			prev = e
		}
	}
	if prev == nil {
		fmt.Fprintln(w, "no previous step on this node; the whole context is new:")
		items, _, _, err := stepRequest(ctx, sink, evs, stepID)
		if err != nil {
			return err
		}
		for _, it := range items {
			printItem(w, it)
		}
		return nil
	}
	var prevData trace.StepRequestData
	if err := json.Unmarshal(prev.Data, &prevData); err != nil {
		return err
	}
	prevSet := map[trace.Hash]bool{}
	for _, h := range prevData.Items {
		prevSet[h] = true
	}
	curSet := map[trace.Hash]bool{}
	for _, h := range data.Items {
		curSet[h] = true
	}
	fmt.Fprintf(w, "diff %s -> %s (node %s)\nnew items:\n", prev.StepID, stepID, node)
	for _, h := range data.Items {
		if prevSet[h] {
			continue
		}
		it, err := trace.GetItem(ctx, sink, h)
		if err != nil {
			return err
		}
		printItem(w, it)
	}
	var removed []string
	for _, h := range prevData.Items {
		if !curSet[h] {
			removed = append(removed, string(h)[:12])
		}
	}
	fmt.Fprintf(w, "removed items (%d, e.g. covered by a summary): %s\n",
		len(removed), strings.Join(removed, " "))
	return nil
}

var loopBuiltins = map[string]bool{
	"complete": true, "agent": true, "workflow": true, "skill": true,
	"result_read": true, "agent_spawn": true, "agent_send": true,
	"agent_interrupt": true, "agent_resume": true, "agent_close": true,
	"agent_status": true, "agent_list": true, "agent_wait": true,
}

func cmdTools(ctx context.Context, sink trace.Sink, args []string, w io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("tools needs a node id")
	}
	evs, err := allEvents(ctx, sink)
	if err != nil {
		return err
	}
	nid, err := resolveID(args[0], nodeIDs(evs))
	if err != nil {
		return err
	}
	// Advertised = node.start view tools ∪ step.request tools metadata.
	// The per-request metadata is authoritative (it includes loop
	// built-ins the node summary omits) and carries effect + schema
	// hash; node.start covers traces recorded before that metadata.
	advertised := map[string]bool{}
	meta := map[string]trace.ToolMeta{}
	var advOrder []string
	addTool := func(m trace.ToolMeta) {
		if m.Name == "" {
			return
		}
		if !advertised[m.Name] {
			advertised[m.Name] = true
			advOrder = append(advOrder, m.Name)
		}
		if m.SchemaHash != "" {
			meta[m.Name] = m
		}
	}
	nodeSteps := map[string]bool{}
	for _, e := range evs {
		if e.NodeID != nid {
			continue
		}
		if e.StepID != "" {
			nodeSteps[e.StepID] = true
		}
		switch e.Type {
		case "node.start":
			var d struct {
				Tools  []string          `json:"tools"`
				Hashes map[string]string `json:"tool_hashes"`
			}
			if json.Unmarshal(e.Data, &d) == nil {
				for _, t := range d.Tools {
					addTool(trace.ToolMeta{Name: t, SchemaHash: d.Hashes[t]})
				}
			}
		case "step.request":
			var d trace.StepRequestData
			if json.Unmarshal(e.Data, &d) == nil {
				for _, m := range d.Tools {
					addTool(m)
				}
			}
		}
	}
	fmt.Fprintf(w, "node %s\nadvertised tools (%d):\n", nid, len(advOrder))
	for _, t := range advOrder {
		if m, ok := meta[t]; ok {
			fmt.Fprintf(w, "  - %s [%s] #%s\n", t, m.Effect, m.SchemaHash)
		} else {
			fmt.Fprintf(w, "  - %s\n", t)
		}
	}
	fmt.Fprintln(w, "attempted calls outside the advertised view:")
	// Every call has a call.start and a call.end; report each call
	// once, from its call.end: unknown_tool when the view refused it,
	// else "not advertised" when it ran outside the advertised list.
	found := false
	for _, e := range evs {
		if e.Type != "call.end" {
			continue
		}
		if !nodeSteps[e.StepID] && e.NodeID != nid {
			continue
		}
		var d struct {
			Name    string `json:"name"`
			Settled string `json:"settled"`
		}
		if json.Unmarshal(e.Data, &d) != nil || d.Name == "" {
			continue
		}
		if d.Settled == "unknown_tool" {
			fmt.Fprintf(w, "  ! %s (unknown_tool) step=%s\n", d.Name, e.StepID)
			found = true
		} else if !advertised[d.Name] && !loopBuiltins[d.Name] {
			fmt.Fprintf(w, "  ! %s (not advertised) step=%s\n", d.Name, e.StepID)
			found = true
		}
	}
	if !found {
		fmt.Fprintln(w, "  (none)")
	}
	return nil
}

// cmdUsage reports token/cost monitoring rollups (trace/query.Usage).
// Scope with --run/--node/--session (unique prefixes ok); --by picks
// the breakdown (default: all); --json emits the raw report.
func cmdUsage(ctx context.Context, sink trace.Sink, args []string, w io.Writer) error {
	var runID, nodeID, sessionID, by string
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--json":
			asJSON = true
		case args[i] == "--run" && i+1 < len(args):
			runID, i = args[i+1], i+1
		case args[i] == "--node" && i+1 < len(args):
			nodeID, i = args[i+1], i+1
		case args[i] == "--session" && i+1 < len(args):
			sessionID, i = args[i+1], i+1
		case args[i] == "--by" && i+1 < len(args):
			by, i = args[i+1], i+1
		case strings.HasPrefix(args[i], "--run="):
			runID = strings.TrimPrefix(args[i], "--run=")
		case strings.HasPrefix(args[i], "--node="):
			nodeID = strings.TrimPrefix(args[i], "--node=")
		case strings.HasPrefix(args[i], "--session="):
			sessionID = strings.TrimPrefix(args[i], "--session=")
		case strings.HasPrefix(args[i], "--by="):
			by = strings.TrimPrefix(args[i], "--by=")
		case !strings.HasPrefix(args[i], "-"):
			runID = args[i] // positional: a run
		}
	}
	evs, err := allEvents(ctx, sink)
	if err != nil {
		return err
	}
	var sessionIDs []string
	seen := map[string]bool{}
	for _, e := range evs {
		if e.SessionID != "" && !seen[e.SessionID] {
			seen[e.SessionID] = true
			sessionIDs = append(sessionIDs, e.SessionID)
		}
	}
	resolve := func(id string, pool []string) (string, error) {
		if id == "" {
			return "", nil
		}
		return resolveID(id, pool)
	}
	if runID, err = resolve(runID, runIDs(evs)); err != nil {
		return err
	}
	if nodeID, err = resolve(nodeID, nodeIDs(evs)); err != nil {
		return err
	}
	if sessionID, err = resolve(sessionID, sessionIDs); err != nil {
		return err
	}
	rep, err := query.Usage(ctx, sink, query.Filter{RunID: runID, NodeID: nodeID, SessionID: sessionID})
	if err != nil {
		return err
	}
	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}
	printUsageTotals := func(label string, t query.UsageTotals) {
		fmt.Fprintf(w, "%s: steps=%d calls=%d prompt=%d cached=%d completion=%d reasoning=%d cost=$%.6f\n",
			label, t.Steps, t.Calls, t.PromptTokens, t.CachedTokens,
			t.CompletionTokens, t.ReasoningTokens, t.CostUSD)
	}
	printUsageTotals("total", rep.Total)
	groups := []struct {
		name string
		rows []query.UsageRow
	}{
		{"model", rep.ByModel}, {"run", rep.ByRun}, {"node", rep.ByNode}, {"tool", rep.ByTool},
	}
	for _, g := range groups {
		if by != "" && by != g.name {
			continue
		}
		if len(g.rows) == 0 {
			continue
		}
		fmt.Fprintf(w, "by %s:\n", g.name)
		for _, r := range g.rows {
			label := "  " + r.Key
			if r.Errors > 0 {
				label += fmt.Sprintf(" (errors=%d)", r.Errors)
			}
			printUsageTotals(label, r.UsageTotals)
		}
	}
	return nil
}

func cmdReplay(ctx context.Context, sink trace.Sink, args []string, w io.Writer) error {
	var step, modelOverride, profileName string
	printOnly := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--model" && i+1 < len(args):
			modelOverride, i = args[i+1], i+1
		case strings.HasPrefix(args[i], "--model="):
			modelOverride = strings.TrimPrefix(args[i], "--model=")
		case args[i] == "--profile" && i+1 < len(args):
			profileName, i = args[i+1], i+1
		case args[i] == "--print":
			printOnly = true
		case !strings.HasPrefix(args[i], "-"):
			step = args[i]
		}
	}
	_ = profileName
	if step == "" {
		return fmt.Errorf("replay needs a step id")
	}
	evs, err := allEvents(ctx, sink)
	if err != nil {
		return err
	}
	sid, err := resolveID(step, stepIDs(evs))
	if err != nil {
		return err
	}
	body, err := query.RebuildRequest(ctx, sink, sid)
	if err != nil {
		return err
	}
	if modelOverride != "" {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return err
		}
		m["model"] = modelOverride
		delete(m, "models")
		body, _ = json.Marshal(m)
	}
	if printOnly {
		fmt.Fprintln(w, string(body))
		return nil
	}
	// Resend detached: a debugging probe, not a traced run (§7.3). It is
	// labelled as such in the output.
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return fmt.Errorf("replay: OPENROUTER_API_KEY is not set (use --print to see the exact request)")
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return err
	}
	m["stream"] = false
	body, _ = json.Marshal(m)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://openrouter.ai/api/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Fprintf(w, "detached replay (not written to the trace) HTTP %d:\n%s\n", resp.StatusCode, string(raw))
	return nil
}

// grepKinds maps §7.3 anomaly names onto event matchers.
func grepMatch(kind string, e trace.Event) bool {
	switch kind {
	case "repair":
		if e.Type == "repair" {
			return true
		}
		if e.Type == "step.request" {
			var d trace.StepRequestData
			return json.Unmarshal(e.Data, &d) == nil && len(d.Repairs) > 0
		}
		return false
	case "retry":
		return e.Type == "step.attempt_failed" || e.Type == "retry"
	case "fallback":
		return e.Type == "config.fallback"
	case "loop":
		return e.Type == "loop.end" && strings.Contains(string(e.Data), "stopped: loop")
	default:
		return e.Type == kind
	}
}

func cmdGrep(ctx context.Context, sink trace.Sink, args []string, w io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("grep needs a run id [kind...]")
	}
	evs, err := allEvents(ctx, sink)
	if err != nil {
		return err
	}
	rid, err := resolveID(args[0], runIDs(evs))
	if err != nil {
		return err
	}
	kinds := args[1:]
	if len(kinds) == 0 {
		kinds = []string{"repair", "retry", "loop", "fallback"}
	}
	n := 0
	for _, e := range evs {
		if e.RunID != rid {
			continue
		}
		for _, k := range kinds {
			if grepMatch(k, e) {
				fmt.Fprintf(w, "#%d %s node=%s step=%s %s\n", e.Cursor, e.Type,
					short(e.NodeID), short(e.StepID), oneLine(string(e.Data), 220))
				n++
				break
			}
		}
	}
	if n == 0 {
		fmt.Fprintln(w, "no anomalies matched")
	}
	return nil
}

func cmdInputs(ctx context.Context, sink trace.Sink, args []string, w io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("inputs needs a thread or handle id")
	}
	target := args[0]
	evs, err := allEvents(ctx, sink)
	if err != nil {
		return err
	}
	n := 0
	for _, e := range evs {
		if !strings.HasPrefix(e.Type, "input.") {
			continue
		}
		var d struct {
			Receipt host.Receipt    `json:"receipt"`
			Origin  json.RawMessage `json:"origin"`
		}
		if json.Unmarshal(e.Data, &d) != nil {
			continue
		}
		if d.Receipt.Target != target && !strings.HasSuffix(d.Receipt.Target, ":"+target) &&
			!strings.Contains(d.Receipt.Target, target) {
			continue
		}
		fmt.Fprintf(w, "#%d %-16s input=%s request=%s mode=%s state=%s seq=%d origin=%s\n",
			e.Cursor, e.Type, short(d.Receipt.InputID), short(d.Receipt.RequestID),
			d.Receipt.Mode, d.Receipt.State, d.Receipt.Seq, oneLine(string(d.Origin), 80))
		n++
	}
	if n == 0 {
		fmt.Fprintf(w, "no inputs for %q (targets are thread keys like thread:<name>, primary, or handle:<id>)\n", target)
	}
	return nil
}

func cmdRecovery(ctx context.Context, sink trace.Sink, g globals, args []string, w io.Writer) error {
	if len(args) > 0 && args[0] == "resolve" {
		return cmdRecoveryResolve(ctx, g, args[1:], w)
	}
	runID := ""
	if len(args) > 0 {
		evs, err := allEvents(ctx, sink)
		if err != nil {
			return err
		}
		rid, err := resolveID(args[0], runIDs(evs))
		if err != nil {
			return err
		}
		runID = rid
	}
	ops, err := workflow.ListRecovery(ctx, sink, runID)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		fmt.Fprintln(w, "no unresolved operations")
		return nil
	}
	for _, op := range ops {
		fmt.Fprintf(w, "%s tool=%s effect=%s outcome=%s args=%s job=%s\n",
			op.Key, op.Tool, op.Effect, op.Outcome, short(op.ArgsHash), op.JobID)
	}
	return nil
}

// cmdRecoveryResolve appends a recovery.resolve through a fresh lease:
// it is the same authorized service the API uses (§7.3), workflow.
func cmdRecoveryResolve(ctx context.Context, g globals, args []string, w io.Writer) error {
	var op, runID, decision, evidence, resultText string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--run" && i+1 < len(args):
			runID, i = args[i+1], i+1
		case args[i] == "--decision" && i+1 < len(args):
			decision, i = args[i+1], i+1
		case args[i] == "--evidence" && i+1 < len(args):
			evidence, i = args[i+1], i+1
		case args[i] == "--result" && i+1 < len(args):
			resultText, i = args[i+1], i+1
		case !strings.HasPrefix(args[i], "-"):
			op = args[i]
		}
	}
	if op == "" || decision == "" {
		return fmt.Errorf("recovery resolve needs <op> and --decision success|failure|retry")
	}
	store, err := tracesqlite.Open(g.db)
	if err != nil {
		return err
	}
	defer store.Close()
	// Find the session that owns the write lease namespace.
	evs, err := allEvents(ctx, store)
	if err != nil {
		return err
	}
	sessID := "craggy-agent-recovery"
	for _, e := range evs {
		if e.Type == "host.start" {
			sessID = e.SessionID
		}
	}
	lease, err := store.AcquireWriter(ctx, sessID)
	if err != nil {
		return fmt.Errorf("acquire writer lease (is the session still running?): %w", err)
	}
	defer lease.Release(ctx)
	rec := trace.NewRecorder(store, lease)
	caller := "user:craggy-agent"
	if err := workflow.ResolveRecovery(ctx, rec, runID, op, decision, evidence, resultText, caller); err != nil {
		return err
	}
	fmt.Fprintf(w, "resolved %s with decision %s\n", op, decision)
	return nil
}

func cmdTail(ctx context.Context, sink trace.Sink, args []string, w io.Writer) error {
	sess := ""
	after := int64(0)
	once := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--after" && i+1 < len(args):
			var n int64
			if _, err := fmt.Sscan(args[i+1], &n); err == nil {
				after = n
			}
			i++
		case args[i] == "--once":
			once = true
		case !strings.HasPrefix(args[i], "-"):
			sess = args[i]
		}
	}
	for {
		evs, next, err := sink.ReadEvents(ctx, after, 256)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if sess != "" && e.SessionID != sess && !strings.HasPrefix(e.SessionID, sess) {
				continue
			}
			fmt.Fprintf(w, "#%d %s run=%s node=%s step=%s %s\n", e.Cursor, e.Type,
				short(e.RunID), short(e.NodeID), short(e.StepID), oneLine(string(e.Data), 160))
		}
		if len(evs) > 0 {
			after = next
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func cmdAgents(ctx context.Context, sink trace.Sink, args []string, w io.Writer) error {
	sess := ""
	if len(args) > 0 {
		sess = args[0]
	}
	evs, err := allEvents(ctx, sink)
	if err != nil {
		return err
	}
	latest := map[string]host.HandleInfo{}
	order := []string{}
	for _, e := range evs {
		if e.Type != "agent.state" {
			continue
		}
		if sess != "" && e.SessionID != sess && !strings.HasPrefix(e.SessionID, sess) {
			continue
		}
		var d struct {
			Op     string          `json:"op"`
			Handle host.HandleInfo `json:"handle"`
		}
		if json.Unmarshal(e.Data, &d) != nil || d.Handle.ID == "" {
			continue
		}
		if _, ok := latest[d.Handle.ID]; !ok {
			order = append(order, d.Handle.ID)
		}
		latest[d.Handle.ID] = d.Handle
	}
	sort.Strings(order)
	if len(order) == 0 {
		fmt.Fprintln(w, "no handles recorded")
		return nil
	}
	for _, id := range order {
		h := latest[id]
		last := ""
		if h.LastResult != nil {
			last = h.LastResult.Status
		}
		fmt.Fprintf(w, "%s def=%s state=%s owner=%s activation=%s previous=%s pending=%d last=%s\n",
			h.ID, h.Def, h.State, h.Owner, short(h.Activation), short(h.PreviousNode),
			h.PendingInputs, last)
	}
	return nil
}

// --- live run / resume --------------------------------------------------------

func findRoot(flagged string) string {
	if flagged != "" {
		return flagged
	}
	// Walk up a few levels looking for a res/ directory.
	dir, _ := os.Getwd()
	for i := 0; i < 4 && dir != ""; i++ {
		cand := filepath.Join(dir, "res")
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "res"
}

func liveSession(ctx context.Context, g globals, cfgPath, sessionID string) (*host.Host, func(), error) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return nil, nil, fmt.Errorf("OPENROUTER_API_KEY is not set; live runs need it (see README quickstart)")
	}
	cfg := config.Defaults()
	if cfgPath != "" {
		loaded, err := configyaml.Load(cfgPath)
		if err != nil {
			return nil, nil, err
		}
		cfg = *loaded
	}
	var sink trace.Sink
	var cleanup = func() {}
	if g.mem {
		sink = tracemem.New()
	} else {
		store, err := tracesqlite.Open(g.db)
		if err != nil {
			return nil, nil, err
		}
		sink = store
		cleanup = func() { store.Close() }
	}
	// The built-in catalogue is the shipped layer; --root (or a nearby
	// res/ directory) is the project layer, which replaces it by name.
	loader, err := agentdef.Open(ctx, agentdef.Options{Shipped: defs.Sources(), Project: findRoot(g.root)})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	s, err := host.Open(ctx, host.Options{
		ID:       sessionID,
		Config:   cfg,
		View:     host.NewMapView(nil, nil),
		Provider: openrouter.New(key),
		Defs:     loader,
		Trace:    sink,
	})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return s, func() { _ = s.Close(); cleanup() }, nil
}

func cmdRun(ctx context.Context, g globals, args []string, w io.Writer) error {
	var agent, wf, cfgPath string
	var taskParts []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--agent" && i+1 < len(args):
			agent, i = args[i+1], i+1
		case strings.HasPrefix(args[i], "--agent="):
			agent = strings.TrimPrefix(args[i], "--agent=")
		case args[i] == "--workflow" && i+1 < len(args):
			wf, i = args[i+1], i+1
		case strings.HasPrefix(args[i], "--workflow="):
			wf = strings.TrimPrefix(args[i], "--workflow=")
		case args[i] == "--config" && i+1 < len(args):
			cfgPath, i = args[i+1], i+1
		case !strings.HasPrefix(args[i], "-"):
			taskParts = append(taskParts, args[i])
		}
	}
	// Positional form: run <agent> <task...>
	if agent == "" && wf == "" && len(taskParts) >= 2 {
		agent, taskParts = taskParts[0], taskParts[1:]
	}
	task := strings.Join(taskParts, " ")
	if task == "" || (agent == "" && wf == "") {
		return fmt.Errorf("usage: run (--agent <name>|--workflow <name>) <task...>")
	}
	s, done, err := liveSession(ctx, g, cfgPath, "")
	if err != nil {
		return err
	}
	defer done()
	if wf != "" {
		// Go through host.RunWorkflow (not workflow.RunEx directly) so
		// the workflow owns one session run: run.start/run.end bracket it
		// and every agent() node joins that run tree (§2).
		res, err := s.RunWorkflow(ctx, wf, map[string]any{"message": task})
		out := map[string]any{"status": res.Status, "value": res.Value, "run_id": res.RunID, "session_id": s.ID()}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(w, string(b))
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "trace session %s; inspect with: craggy-agent --db %s runs\n", s.ID(), g.db)
		return nil
	}
	res, err := s.RunAgent(ctx, agent, task)
	out := map[string]any{"status": res.Status, "text": res.Text, "run_id": res.RunID,
		"node_id": res.NodeID, "session_id": s.ID(), "steps": res.Steps, "usage": res.Usage}
	if len(res.Output) > 0 {
		out["output"] = json.RawMessage(res.Output)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(w, string(b))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "trace session %s run %s; inspect with: craggy-agent --db %s tree %s\n",
		s.ID(), res.RunID, g.db, res.RunID)
	return nil
}

func cmdResume(ctx context.Context, g globals, args []string, w io.Writer) error {
	var runID string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			runID = a
		}
	}
	if runID == "" {
		return fmt.Errorf("usage: resume <workflow-run-id>")
	}
	// Resolve the prefix and owning session from the trace first.
	store, err := tracesqlite.Open(g.db)
	if err != nil {
		return err
	}
	evs, err := allEvents(ctx, store)
	if err != nil {
		store.Close()
		return err
	}
	full, err := resolveID(runID, runIDs(evs))
	if err != nil {
		store.Close()
		return err
	}
	sessID := ""
	for _, e := range evs {
		if e.RunID == full && e.SessionID != "" {
			sessID = e.SessionID
		}
	}
	store.Close()
	s, done, err := liveSession(ctx, g, "", sessID)
	if err != nil {
		return err
	}
	defer done()
	res, err := workflow.Resume(ctx, s, full)
	out := map[string]any{"status": res.Status, "value": res.Value, "run_id": res.RunID}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(w, string(b))
	return err
}
