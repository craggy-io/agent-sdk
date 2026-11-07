package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
	scriptschema "github.com/craggy-io/script-services/schema"
)

// predeclared returns the §6.2 globals for one execution. Built-ins find
// their exec on the thread ("wf"), so nested executions never share
// mutable Starlark state; agent loops run on Go goroutines and Starlark
// only waits on handles (§6.2).
func predeclared(e *exec) starlark.StringDict {
	b := func(name string, fn func(*exec, *starlark.Thread, starlark.Tuple, []starlark.Tuple) (starlark.Value, error)) *starlark.Builtin {
		return starlark.NewBuiltin(name, func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			ex, _ := thread.Local("wf").(*exec)
			if ex == nil {
				return nil, fmt.Errorf("workflow: %s called outside a workflow thread", name)
			}
			return fn(ex, thread, args, kwargs)
		})
	}
	return starlark.StringDict{
		"workflow":     starlark.NewBuiltin("workflow", workflowBuiltin),
		"schema":       scriptschema.Module(),
		"agent":        b("agent", builtinAgent),
		"spec":         b("spec", builtinSpec),
		"start":        b("start", builtinStart),
		"wait":         b("wait", builtinWait),
		"parallel":     b("parallel", builtinParallel),
		"define":       b("define", builtinDefine),
		"workflow_run": b("workflow_run", builtinWorkflowRun),
		"ask":          b("ask", builtinAsk),
		"call":         b("call", builtinCall),
		"tools":        toolsNamespace(e),
		"phase":        b("phase", builtinPhase),
		"log":          b("log", builtinLog),
		"budget":       b("budget", builtinBudget),
		"config":       b("config", builtinConfig),
		"render":       b("render", builtinRender),
		"json":         jsonModule(),
		"spawn":        b("spawn", builtinSpawn),
		"send":         b("send", builtinSend),
		"interrupt":    b("interrupt", controlBuiltin("interrupt")),
		"resume":       b("resume", controlBuiltin("resume")),
		"close":        b("close", controlBuiltin("close")),
		"status":       b("status", controlBuiltin("status")),
		"list_agents":  b("list_agents", builtinListAgents),
	}
}

func jsonModule() starlark.Value {
	return &nsValue{name: "json", children: map[string]starlark.Value{
		"encode": starlark.NewBuiltin("json.encode", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("json.encode: want 1 argument")
			}
			v, err := fromStar(args[0])
			if err != nil {
				return nil, err
			}
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			return starlark.String(string(b)), nil
		}),
		"decode": starlark.NewBuiltin("json.decode", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			var s string
			if err := starlark.UnpackPositionalArgs("json.decode", args, nil, 1, &s); err != nil {
				return nil, err
			}
			var v any
			if err := json.Unmarshal([]byte(s), &v); err != nil {
				return nil, fmt.Errorf("json.decode: %w", err)
			}
			return toStar(v), nil
		}),
	}}
}

// --- agent specs --------------------------------------------------------------

// specValue is an unstarted agent call (§6.2 spec()).
type specValue struct {
	name    string
	defn    *agentdef.Definition
	task    string
	profile string
	tools   *model.FilterSpec
	thread  string
	phase   string
	label   string
	layers  int // 1 when a loop= layer was supplied (kept JSON-side below)
	layer   map[string]any
	output  json.RawMessage
	// check is a tool-view capability ref run as the agent's
	// per-run output check; retries bounds its rejections.
	check     string
	checkArgs map[string]any
	retries   *int
}

func (v *specValue) String() string {
	return fmt.Sprintf("spec(%s)", v.agentName())
}
func (v *specValue) Type() string          { return "spec" }
func (v *specValue) Freeze()               {}
func (v *specValue) Truth() starlark.Bool  { return starlark.True }
func (v *specValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: spec") }

func (v *specValue) agentName() string {
	if v.defn != nil {
		return v.defn.Name
	}
	return v.name
}

func (v *specValue) json() map[string]any {
	m := map[string]any{"name": v.agentName(), "task": v.task, "profile": v.profile,
		"thread": v.thread, "phase": v.phase, "label": v.label}
	if v.defn != nil {
		m["def"] = defJSON(*v.defn)
	}
	if v.tools != nil {
		m["tools"] = v.tools
	}
	if v.layer != nil {
		m["loop"] = v.layer
	}
	if len(v.output) > 0 {
		m["output"] = json.RawMessage(v.output)
	}
	if v.check != "" {
		m["check"] = v.check
	}
	if v.checkArgs != nil {
		m["check_args"] = v.checkArgs
	}
	if v.retries != nil {
		m["retries"] = *v.retries
	}
	return m
}

func specFromJSON(m map[string]any) (*specValue, error) {
	sv := &specValue{}
	sv.name, _ = m["name"].(string)
	sv.task, _ = m["task"].(string)
	sv.profile, _ = m["profile"].(string)
	sv.thread, _ = m["thread"].(string)
	sv.phase, _ = m["phase"].(string)
	sv.label, _ = m["label"].(string)
	if dm, ok := m["def"].(map[string]any); ok {
		d, err := defFromJSON(dm)
		if err != nil {
			return nil, err
		}
		sv.defn = &d
	}
	if tm, ok := m["tools"].(map[string]any); ok {
		f, err := filterFromMap(tm)
		if err != nil {
			return nil, err
		}
		sv.tools = f
	}
	if lm, ok := m["loop"].(map[string]any); ok {
		sv.layer = lm
	}
	if raw, ok := m["output"]; ok && raw != nil {
		b, _ := json.Marshal(raw)
		sv.output = b
	}
	sv.check, _ = m["check"].(string)
	if ca, ok := m["check_args"].(map[string]any); ok {
		sv.checkArgs = ca
	}
	if rv, ok := m["retries"]; ok && rv != nil {
		if n, ok := anyInt(rv); ok {
			sv.retries = &n
		}
	}
	return sv, nil
}

// parseAgentArgs implements the shared agent()/spec() signature (§6.2):
// agent(name_or_def, task=..., profile=, tools=, loop=, output=,
// thread=, phase=, label=, check=, check_args=, retries=).
func parseAgentArgs(fn string, args starlark.Tuple, kwargs []starlark.Tuple) (*specValue, error) {
	var who starlark.Value
	var task, profile, thread, phase, label, check string
	var toolsV, loopV, outputV, checkArgsV, retriesV starlark.Value
	if err := starlark.UnpackArgs(fn, args, kwargs,
		"name_or_def", &who, "task?", &task, "profile?", &profile,
		"tools?", &toolsV, "loop?", &loopV, "output?", &outputV,
		"thread?", &thread, "phase?", &phase, "label?", &label,
		"check?", &check, "check_args?", &checkArgsV, "retries?", &retriesV); err != nil {
		return nil, err
	}
	sv := &specValue{task: task, profile: profile, thread: thread, phase: phase, label: label, check: check}
	if checkArgsV != nil && checkArgsV != starlark.None {
		m, err := dictAny(checkArgsV)
		if err != nil {
			return nil, fmt.Errorf("%s: check_args: %w", fn, err)
		}
		sv.checkArgs = m
	}
	if retriesV != nil && retriesV != starlark.None {
		n, ok := retriesV.(starlark.Int)
		if !ok {
			return nil, fmt.Errorf("%s: retries must be an int, got %s", fn, retriesV.Type())
		}
		i, ok := n.Int64()
		if !ok {
			return nil, fmt.Errorf("%s: retries out of range", fn)
		}
		v := int(i)
		sv.retries = &v
	}
	switch w := who.(type) {
	case starlark.String:
		sv.name = string(w)
	case *defValue:
		cp := w.def
		sv.defn = &cp
	default:
		return nil, fmt.Errorf("%s: name_or_def must be a string or define() value, got %s", fn, who.Type())
	}
	if toolsV != nil {
		f, err := filterFromStar(toolsV)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fn, err)
		}
		sv.tools = f
	}
	if loopV != nil && loopV != starlark.None {
		m, err := dictAny(loopV)
		if err != nil {
			return nil, fmt.Errorf("%s: loop: %w", fn, err)
		}
		if _, err := loopLayerFromMap(m); err != nil {
			return nil, fmt.Errorf("%s: %w", fn, err)
		}
		sv.layer = m
	}
	if outputV != nil && outputV != starlark.None {
		b, err := schemaJSON(outputV)
		if err != nil {
			return nil, fmt.Errorf("%s: output: %w", fn, err)
		}
		sv.output = b
	}
	return sv, nil
}

func (e *exec) allowed(name string) bool {
	if e.man == nil || len(e.man.Agents) == 0 {
		return true
	}
	for _, pat := range e.man.Agents {
		if ok, _ := path.Match(pat, name); ok || pat == name {
			return true
		}
	}
	return false
}

// runSpec executes one spec to completion (no journal; callers journal).
// Agent-level failures come back as Results, never errors (§6.2).
func (e *exec) runSpec(ctx context.Context, sv *specValue) map[string]any {
	name := sv.agentName()
	if !e.allowed(name) {
		return map[string]any{"status": "failed: not_allowed", "text": "",
			"def": name, "node": "", "run": "",
			"error": fmt.Sprintf("agent %q is not in the workflow agents allowlist", name)}
	}
	spec := host.ExecSpec{Task: sv.task, Profile: sv.profile, Thread: sv.thread}
	if sv.defn != nil {
		cp := *sv.defn
		spec.Def = &cp
	} else {
		spec.Name = sv.name
	}
	if len(sv.output) > 0 {
		// output= wraps the target in a dynamic structured variant (§4.4).
		d, err := structuredVariant(sv)
		if err != nil {
			return map[string]any{"status": "failed: output", "text": "", "def": name, "error": err.Error()}
		}
		spec.Def, spec.Name = d, ""
	}
	if sv.tools != nil {
		cp := *sv.tools
		spec.Tools = &cp
	}
	if sv.check != "" {
		retries := 2
		if sv.retries != nil {
			retries = *sv.retries
		}
		spec.OutputCheck = func(ctx context.Context, output json.RawMessage) (string, error) {
			return e.runOutputCheck(ctx, sv.check, sv.checkArgs, output)
		}
		spec.OutputCheckRetries = retries
		// The check gates the structured output, so the run needs
		// the output contract even when output= only came from the
		// definition; spec.Output carries sv.output when set.
		if len(sv.output) > 0 {
			spec.Output = append(json.RawMessage(nil), sv.output...)
		}
	}
	spec.Layers = append(spec.Layers, e.layers()...)
	if sv.layer != nil {
		if l, err := loopLayerFromMap(sv.layer); err == nil && l != nil {
			spec.Layers = append(spec.Layers, *l)
		}
	}
	res, err := e.s.ExecAgent(ctx, spec)
	m := agentResultJSON(res)
	if err != nil {
		if res.Status == "" {
			m["status"] = "failed: exec"
		}
		m["error"] = err.Error()
	}
	if m["status"] == "" || m["status"] == nil {
		m["status"] = "failed: exec"
	}
	e.noteUsage(res.Usage)
	e.spentMu.Lock()
	e.steps += res.Steps
	e.spentMu.Unlock()
	return m
}

// runOutputCheck calls the check capability through the workflow's
// tool view with {output, ...check_args} and reads {feedback} back.
// It is deliberately not journaled on its own: the agent() result it
// gates is journaled, so on replay neither the agent nor the check
// runs again (§6.3). A capability error fails the run; unparseable
// non-empty text is treated as feedback (fail closed).
func (e *exec) runOutputCheck(ctx context.Context, ref string, extra map[string]any, output json.RawMessage) (string, error) {
	if e.view == nil {
		return "", fmt.Errorf("agent: check %q: no tool view in session", ref)
	}
	args := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		args[k] = v
	}
	var out any
	if err := json.Unmarshal(output, &out); err != nil {
		return "", fmt.Errorf("agent: check %q: output: %w", ref, err)
	}
	args["output"] = out
	argsJSON, _ := json.Marshal(args)
	res, err := e.view.Run(ctx, ref, json.RawMessage(argsJSON))
	if err != nil {
		return "", fmt.Errorf("agent: check %q: %w", ref, err)
	}
	if res.IsError {
		return "", fmt.Errorf("agent: check %q failed: %s", ref, res.Text)
	}
	text := strings.TrimSpace(res.Text)
	if text == "" {
		return "", nil
	}
	var fb struct {
		Feedback string `json:"feedback"`
	}
	if err := json.Unmarshal([]byte(text), &fb); err == nil {
		return fb.Feedback, nil
	}
	return text, nil
}

func structuredVariant(sv *specValue) (*agentdef.Definition, error) {
	out := string(sv.output)
	comp := "structured"
	if !json.Valid(sv.output) {
		return nil, fmt.Errorf("output schema is not valid JSON")
	}
	if sv.defn != nil {
		d := *sv.defn
		d.Loop.Completion = &comp
		d.Loop.Output = &out
		return &d, nil
	}
	return &agentdef.Definition{
		Name: sv.name + "-structured", Extends: sv.name,
		Loop:  agentdef.LoopSpec{Completion: &comp, Output: &out},
		Tools: model.FilterSpec{Inherit: true},
	}, nil
}

func builtinAgent(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	sv, err := parseAgentArgs("agent", args, kwargs)
	if err != nil {
		return nil, err
	}
	return e.agentJournaled(sv)
}

func builtinSpec(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	sv, err := parseAgentArgs("spec", args, kwargs)
	if err != nil {
		return nil, err
	}
	return sv, nil
}

func (e *exec) agentJournaled(sv *specValue) (starlark.Value, error) {
	key := e.nextKey("agent", sv.json())
	v, _, err := e.journal.do(e.ctx, e, key, "agent", sv.json(), func() (any, error) {
		return e.runSpec(e.ctx, sv), nil
	})
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return resultObj(m), nil
}

// --- start/wait/parallel handles ----------------------------------------------

// wfHandle is a lazily-started one-shot agent under start()/wait(). It
// launches the spec in its own goroutine (the loop runs in Go; Starlark
// only waits, §6.2). On replay a recorded start recreates the handle
// unlaunched: wait() returns the recorded result when present, and only
// launches for real when the original run never completed the wait.
type wfHandle struct {
	mu      sync.Mutex
	started bool
	done    chan struct{}
	result  map[string]any
}

type handleValue struct {
	id      string // wf:<callkey hash> or a session handle ID
	session bool
	wf      *wfHandle
	spec    *specValue
}

func (v *handleValue) String() string        { return "handle(" + v.id + ")" }
func (v *handleValue) Type() string          { return "handle" }
func (v *handleValue) Freeze()               {}
func (v *handleValue) Truth() starlark.Bool  { return starlark.True }
func (v *handleValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: handle") }
func (v *handleValue) AttrNames() []string   { return []string{"id"} }
func (v *handleValue) Attr(name string) (starlark.Value, error) {
	if name == "id" {
		return starlark.String(v.id), nil
	}
	return nil, nil
}

var _ starlark.HasAttrs = (*handleValue)(nil)

func (e *exec) launch(h *wfHandle, sv *specValue) {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started = true
	if h.done == nil {
		h.done = make(chan struct{})
	}
	h.mu.Unlock()
	go func() {
		res := e.runSpec(e.ctx, sv)
		h.mu.Lock()
		h.result = res
		h.mu.Unlock()
		close(h.done)
	}()
}

func builtinStart(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 1 || len(kwargs) > 0 {
		return nil, fmt.Errorf("start: want start(spec(...))")
	}
	sv, ok := args[0].(*specValue)
	if !ok {
		return nil, fmt.Errorf("start: want a spec() value, got %s", args[0].Type())
	}
	key := e.nextKey("start", sv.json())
	hid := "wf:" + hash16(key)
	hv := &handleValue{id: hid, wf: &wfHandle{done: make(chan struct{})}, spec: sv}
	_, replayed, err := e.journal.do(e.ctx, e, key, "start", sv.json(), func() (any, error) {
		e.launch(hv.wf, sv)
		return map[string]any{"handle": hid}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = replayed // replayed starts stay unlaunched until wait needs them
	return hv, nil
}

func builtinWait(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var handlesV starlark.Value
	var timeout float64 = -1
	if err := starlark.UnpackArgs("wait", args, kwargs, "handles", &handlesV, "timeout?", &timeout); err != nil {
		return nil, err
	}
	handles, err := asHandles(handlesV)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(handles))
	for i, h := range handles {
		ids[i] = h.id
	}
	keyArgs := map[string]any{"handles": ids, "timeout": timeout}
	key := e.nextKey("wait", keyArgs)
	v, _, err := e.journal.do(e.ctx, e, key, "wait", keyArgs, func() (any, error) {
		return e.waitHandles(handles, timeout), nil
	})
	if err != nil {
		return nil, err
	}
	return toStar(v), nil
}

func asHandles(v starlark.Value) ([]*handleValue, error) {
	if h, ok := v.(*handleValue); ok {
		return []*handleValue{h}, nil
	}
	it, ok := v.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("want handle or list of handles, got %s", v.Type())
	}
	iter := it.Iterate()
	defer iter.Done()
	var out []*handleValue
	var x starlark.Value
	for iter.Next(&x) {
		h, ok := x.(*handleValue)
		if !ok {
			return nil, fmt.Errorf("want handle, got %s", x.Type())
		}
		out = append(out, h)
	}
	return out, nil
}

// waitHandles blocks in Go (not on the Starlark thread's call stack
// beyond this built-in). Timeout never cancels work (§6.5).
func (e *exec) waitHandles(handles []*handleValue, timeoutSec float64) map[string]any {
	deadline := time.Time{}
	if timeoutSec >= 0 {
		deadline = time.Now().Add(time.Duration(timeoutSec * float64(time.Second)))
	}
	results := make([]any, len(handles))
	timedOut := false
	for i, h := range handles {
		if h.session {
			results[i] = e.waitSessionHandle(h.id, deadline, &timedOut)
			continue
		}
		e.launch(h.wf, h.spec) // no-op when already running
		var timer <-chan time.Time
		if !deadline.IsZero() {
			timer = time.After(maxDur(0, time.Until(deadline)))
		}
		select {
		case <-h.wf.done:
			h.wf.mu.Lock()
			results[i] = h.wf.result
			h.wf.mu.Unlock()
		case <-timer:
			timedOut = true
			results[i] = map[string]any{"status": "running", "text": "", "handle": h.id}
		case <-e.ctx.Done():
			results[i] = map[string]any{"status": StatusCancelled, "text": "", "handle": h.id}
		}
	}
	return map[string]any{"results": results, "timeout": timedOut}
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (e *exec) waitSessionHandle(id string, deadline time.Time, timedOut *bool) any {
	to := time.Duration(0)
	if !deadline.IsZero() {
		to = maxDur(0, time.Until(deadline))
	} else {
		to = 30 * time.Second
	}
	res, err := e.s.Wait(e.ctx, []string{id}, to)
	if err != nil {
		return map[string]any{"status": "failed: wait", "text": "", "error": err.Error(), "handle": id}
	}
	if len(res) == 0 {
		return map[string]any{"status": "idle", "text": "", "handle": id}
	}
	wr := res[0]
	if wr.Timeout {
		*timedOut = true
		return map[string]any{"status": "running", "text": "", "handle": id}
	}
	if wr.Info.LastResult != nil {
		return agentResultJSON(*wr.Info.LastResult)
	}
	st := wr.Info.State
	if st == "" {
		st = "idle"
	}
	return map[string]any{"status": st, "text": "", "handle": id}
}

// builtinParallel runs specs concurrently and returns results in input
// order; a failed branch is a Result with status failed, never a raise
// (§6.2). Each branch is journaled under a deterministic branch key so
// completed branches are reused on resume (§6.3).
func builtinParallel(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var specsV starlark.Value
	var phase string
	if err := starlark.UnpackArgs("parallel", args, kwargs, "specs", &specsV, "phase?", &phase); err != nil {
		return nil, err
	}
	it, ok := specsV.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("parallel: want a list of spec() values")
	}
	iter := it.Iterate()
	var specs []*specValue
	var x starlark.Value
	for iter.Next(&x) {
		sv, ok := x.(*specValue)
		if !ok {
			iter.Done()
			return nil, fmt.Errorf("parallel: want spec() values, got %s", x.Type())
		}
		specs = append(specs, sv)
	}
	iter.Done()

	parKey := e.nextKey("parallel", map[string]any{"n": len(specs), "phase": phase})
	// Journal the parallel call itself so its branch set is pinned.
	if _, _, err := e.journal.do(e.ctx, e, parKey, "parallel",
		map[string]any{"n": len(specs), "phase": phase},
		func() (any, error) { return map[string]any{"branches": len(specs)}, nil }); err != nil {
		return nil, err
	}

	results := make([]any, len(specs))
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for i, sv := range specs {
		wg.Add(1)
		go func(i int, sv *specValue) {
			defer wg.Done()
			key := fmt.Sprintf("%s/branch:%d:%s", parKey, i, hash16(sv.json()))
			v, _, err := e.journal.do(e.ctx, e, key, "agent", sv.json(), func() (any, error) {
				return e.runSpec(e.ctx, sv), nil
			})
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			results[i] = v
		}(i, sv)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	elems := make([]starlark.Value, len(results))
	for i, r := range results {
		m, _ := r.(map[string]any)
		elems[i] = resultObj(m)
	}
	l := starlark.NewList(elems)
	l.Freeze()
	return l, nil
}

// --- define -----------------------------------------------------------------

type defValue struct{ def agentdef.Definition }

func (v *defValue) String() string        { return "define(" + v.def.Name + ")" }
func (v *defValue) Type() string          { return "definition" }
func (v *defValue) Freeze()               {}
func (v *defValue) Truth() starlark.Bool  { return starlark.True }
func (v *defValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: definition") }
func (v *defValue) AttrNames() []string   { return []string{"name", "extends"} }
func (v *defValue) Attr(name string) (starlark.Value, error) {
	switch name {
	case "name":
		return starlark.String(v.def.Name), nil
	case "extends":
		return starlark.String(v.def.Extends), nil
	}
	return nil, nil
}

var _ starlark.HasAttrs = (*defValue)(nil)

// builtinDefine implements define(name, extends, prompt, tools,
// profile, loop, skills) (§6.2/§4.4): a dynamic definition with run
// scope. Its identity is journaled and restored verbatim on replay.
func builtinDefine(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, extends, profile, description string
	var promptV, toolsV, loopV, skillsV, delegatesV starlark.Value
	var workflows bool
	if err := starlark.UnpackArgs("define", args, kwargs,
		"name", &name, "extends", &extends,
		"prompt?", &promptV, "tools?", &toolsV, "profile?", &profile,
		"loop?", &loopV, "skills?", &skillsV, "delegates?", &delegatesV,
		"description?", &description, "workflows?", &workflows); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("define: name is required")
	}
	if extends == "" {
		return nil, fmt.Errorf("define: %q must extend an existing definition (§4)", name)
	}
	d := agentdef.Definition{Name: name, Extends: extends, Profile: profile,
		Description: description, Workflows: workflows,
		Tools: model.FilterSpec{Inherit: true}}
	if promptV != nil && promptV != starlark.None {
		entries, err := promptEntries(promptV)
		if err != nil {
			return nil, fmt.Errorf("define: prompt: %w", err)
		}
		d.Prompt = entries
	}
	if toolsV != nil {
		f, err := filterFromStar(toolsV)
		if err != nil {
			return nil, fmt.Errorf("define: %w", err)
		}
		if f != nil {
			d.Tools = *f
		}
	}
	if loopV != nil && loopV != starlark.None {
		m, err := dictAny(loopV)
		if err != nil {
			return nil, fmt.Errorf("define: loop: %w", err)
		}
		ls, err := loopSpecFromMap(m)
		if err != nil {
			return nil, fmt.Errorf("define: %w", err)
		}
		d.Loop = ls
	}
	skills, err := strSlice(skillsV)
	if err != nil {
		return nil, fmt.Errorf("define: skills: %w", err)
	}
	d.Skills = skills
	delegates, err := strSlice(delegatesV)
	if err != nil {
		return nil, fmt.Errorf("define: delegates: %w", err)
	}
	d.Delegates = delegates

	key := e.nextKey("define", defJSON(d))
	v, _, err := e.journal.do(e.ctx, e, key, "define", defJSON(d), func() (any, error) {
		// Validate through the same resolution as disk definitions.
		if e.s.Defs() == nil {
			return nil, fmt.Errorf("define: no definition loader in session")
		}
		if _, err := e.s.Defs().ResolveDynamic(d, nil); err != nil {
			return nil, err
		}
		return defJSON(d), nil
	})
	if err != nil {
		return nil, err
	}
	// On replay the recorded definition wins (restored identity, §6.3).
	if m, ok := v.(map[string]any); ok {
		if rd, err := defFromJSON(m); err == nil {
			d = rd
		}
	}
	return &defValue{def: d}, nil
}

func promptEntries(v starlark.Value) ([]agentdef.PromptEntry, error) {
	if s, ok := v.(starlark.String); ok {
		return []agentdef.PromptEntry{{Text: string(s)}}, nil
	}
	it, ok := v.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("want string or list, got %s", v.Type())
	}
	iter := it.Iterate()
	defer iter.Done()
	var out []agentdef.PromptEntry
	var x starlark.Value
	for iter.Next(&x) {
		switch t := x.(type) {
		case starlark.String:
			out = append(out, agentdef.PromptEntry{Text: string(t)})
		case *starlark.Dict:
			m, err := dictAny(x)
			if err != nil {
				return nil, err
			}
			var en agentdef.PromptEntry
			en.File, _ = m["file"].(string)
			en.Replace, _ = m["replace"].(string)
			en.Remove, _ = m["remove"].(string)
			en.Text, _ = m["text"].(string)
			out = append(out, en)
		default:
			return nil, fmt.Errorf("prompt entries must be strings or dicts, got %s", x.Type())
		}
	}
	return out, nil
}

// --- nested workflows ---------------------------------------------------------

func builtinWorkflowRun(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var nameOrSource string
	var inputV starlark.Value
	if err := starlark.UnpackArgs("workflow_run", args, kwargs,
		"name_or_source", &nameOrSource, "input?", &inputV); err != nil {
		return nil, err
	}
	input, err := dictAny(inputV)
	if err != nil {
		return nil, fmt.Errorf("workflow_run: input: %w", err)
	}
	wf, source, err := resolveSource(e.ctx, e.s, nameOrSource)
	if err != nil {
		return nil, err
	}
	name := wf.Name
	childPath := name
	if childPath == "" {
		childPath = "wf:" + hashBytes([]byte(source))[:8]
	}
	ordKey := e.nextKey("workflow_run", map[string]any{"name": name, "input": input})
	_ = e.emit("wf.call", map[string]any{"key": ordKey, "builtin": "workflow_run",
		"phase": "intent", "args_hash": hash16(input)})
	// Nested run: same journal run (its calls journal under a child node
	// path), the caller's narrowed view via ctx, shared session budget.
	res, err := execute(e.ctx, e.s, wf, source, input, Options{
		RunID: e.runID, Resume: e.resume, NodePath: e.path + ">" + childPath,
		View: e.view,
	})
	if res.Pending != nil {
		// An ask inside the nested run suspends the whole run.
		e.pending = res.Pending
		return nil, errSuspended
	}
	if err != nil {
		return nil, fmt.Errorf("workflow_run %q: %w", nameOrSource, err)
	}
	return toStar(res.Value), nil
}

// --- registry-shaped tool calls (agents-local view) -----------------------------

// toolsNamespace builds tools.<ns>.<verb> from the workflow view's
// dotted tool names (§6.2). Locally this binds host.ToolView; it
// maps 1:1 onto the registry's starlark.Globals later (§9.1).
func toolsNamespace(e *exec) starlark.Value {
	root := &nsValue{name: "tools", children: map[string]starlark.Value{}}
	if e.view == nil {
		return root
	}
	for _, t := range e.view.Tools() {
		parts := strings.Split(t.Name, ".")
		cur := root
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur.children[p].(*nsValue)
			if !ok {
				next = &nsValue{name: p, children: map[string]starlark.Value{}}
				cur.children[p] = next
			}
			cur = next
		}
		leaf := parts[len(parts)-1]
		toolName := t.Name
		cur.children[leaf] = starlark.NewBuiltin(toolName, func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			ex, _ := thread.Local("wf").(*exec)
			if ex == nil {
				return nil, fmt.Errorf("tools: called outside a workflow thread")
			}
			return ex.toolCall("tools."+toolName, toolName, args, kwargs)
		})
	}
	return root
}

func builtinCall(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var ref string
	var argV starlark.Value
	if err := starlark.UnpackArgs("call", args, kwargs, "ref", &ref, "args?", &argV); err != nil {
		// Also accept call(ref, {...}) positionally.
		if len(args) == 2 {
			if s, ok := args[0].(starlark.String); ok {
				ref, argV = string(s), args[1]
			} else {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	m, err := toolArgsDict(argV, nil)
	if err != nil {
		return nil, fmt.Errorf("call: %w", err)
	}
	return e.toolCallJSON("call", ref, m)
}

func toolArgsDict(pos starlark.Value, kwargs []starlark.Tuple) (map[string]any, error) {
	out := map[string]any{}
	if pos != nil && pos != starlark.None {
		m, err := dictAny(pos)
		if err != nil {
			return nil, fmt.Errorf("tool arguments must be a dict: %w", err)
		}
		for k, v := range m {
			out[k] = v
		}
	}
	for _, kv := range kwargs {
		k, ok := kv[0].(starlark.String)
		if !ok {
			continue
		}
		v, err := fromStar(kv[1])
		if err != nil {
			return nil, err
		}
		out[string(k)] = v
	}
	return out, nil
}

func (e *exec) toolCall(builtin, toolName string, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var pos starlark.Value
	if len(args) > 0 {
		pos = args[0]
	}
	m, err := toolArgsDict(pos, kwargs)
	if err != nil {
		return nil, err
	}
	return e.toolCallJSON(builtin, toolName, m)
}

// toolCallJSON journals one tool execution with operation intent/outcome
// (§6.3 crash window). Recorded results replay; an intent with no
// outcome retries only for read-only tools (or an authorized retry
// resolution) and otherwise blocks with recovery_required.
func (e *exec) toolCallJSON(builtin, toolName string, argMap map[string]any) (starlark.Value, error) {
	if e.view == nil {
		return nil, fmt.Errorf("%s: no tool view in session", builtin)
	}
	argsJSON, _ := json.Marshal(argMap)
	key := e.nextKey(builtin, map[string]any{"tool": toolName, "args": argMap})
	op := &opState{Key: opKey(key), Tool: toolName,
		Effect: effectOf(e.view.Tools(), toolName), ArgsHash: hashBytes(argsJSON)}

	run := func() (any, error) {
		if err := e.journal.emitOp(e.ctx, e, "operation.intent", op, nil); err != nil {
			return nil, err
		}
		res, err := e.view.Run(e.ctx, toolName, json.RawMessage(argsJSON))
		outcome := "ok"
		if err != nil || res.IsError {
			outcome = "error"
		}
		_ = e.journal.emitOp(e.ctx, e, "operation.outcome", op,
			map[string]any{"outcome": outcome, "job_id": res.JobID})
		if err != nil {
			return map[string]any{"text": err.Error(), "is_error": true, "code": "tool_error"}, nil
		}
		return map[string]any{"text": res.Text, "is_error": res.IsError,
			"code": res.Code, "job_id": res.JobID}, nil
	}

	// Crash-window check (§6.3): a prior intent with no recorded wf.call
	// result means the effect may already have run; even a recorded
	// outcome cannot be replayed locally because the result text was
	// never committed. Only read-only operations may simply retry.
	if _, recorded := e.journal.lookup(key); !recorded {
		if prev, pending := e.journal.opSnapshot(op.Key); pending && prev != nil {
			switch {
			case prev.Decision == "success":
				// Authoritative evidence: persist it as the result.
				v := map[string]any{"text": prev.ResultText, "is_error": false, "code": "recovered"}
				b, _ := json.Marshal(v)
				_ = e.journal.commit(e.ctx, e, &journalEntry{Key: key, Builtin: builtin, Result: b},
					map[string]any{"tool": toolName, "args": argMap})
				return toStar(v), nil
			case prev.Decision == "failure":
				v := map[string]any{"text": prev.Evidence, "is_error": true, "code": "outcome_unknown"}
				b, _ := json.Marshal(v)
				_ = e.journal.commit(e.ctx, e, &journalEntry{Key: key, Builtin: builtin, Result: b},
					map[string]any{"tool": toolName, "args": argMap})
				return toStar(v), nil
			case prev.Decision == "retry":
				// Explicit authorization: fall through and re-run.
			case prev.Effect == "read-only" || op.Effect == "read-only":
				// Read-only may be retried; noted on the new intent below.
			default:
				_ = e.journal.emitOp(e.ctx, e, "operation.outcome", op,
					map[string]any{"outcome": string(model.SettleOutcomeUnknown)})
				return nil, &recoveryError{op: op.Key}
			}
		}
	}

	v, _, err := e.journal.do(e.ctx, e, key, builtin,
		map[string]any{"tool": toolName, "args": argMap}, run)
	if err != nil {
		return nil, err
	}
	return toStar(v), nil
}

// --- observability, pure helpers ----------------------------------------------

func builtinPhase(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackArgs("phase", args, kwargs, "name", &name); err != nil {
		return nil, err
	}
	e.phase = name
	_ = e.emit("wf.phase", map[string]any{"phase": name})
	return starlark.None, nil
}

func builtinLog(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var msg string
	if err := starlark.UnpackPositionalArgs("log", args, nil, 1, &msg); err != nil {
		return nil, err
	}
	data, err := toolArgsDict(nil, kwargs)
	if err != nil {
		return nil, err
	}
	_ = e.emit("wf.log", map[string]any{"msg": msg, "data": data, "phase": e.phase})
	return starlark.None, nil
}

// budget() is a nondeterministic observation, so it is journaled (§6.3).
func builtinBudget(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) > 0 || len(kwargs) > 0 {
		return nil, fmt.Errorf("budget: takes no arguments")
	}
	key := e.nextKey("budget", nil)
	v, _, err := e.journal.do(e.ctx, e, key, "budget", nil, func() (any, error) {
		cfg := e.s.Config()
		e.spentMu.Lock()
		spent, steps := e.spent, e.steps
		e.spentMu.Unlock()
		return map[string]any{
			"usd_spent": spent, "usd_limit": cfg.Limits.USD,
			"steps": steps, "steps_limit": cfg.Limits.Steps,
			"tool_calls_limit": cfg.Limits.ToolCalls,
			"depth_limit":      cfg.Limits.Depth,
			"concurrency":      cfg.Limits.Concurrency,
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return toStar(v), nil
}

// config() returns the effective session config summary, read-only and
// journaled like other observations.
func builtinConfig(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) > 0 || len(kwargs) > 0 {
		return nil, fmt.Errorf("config: takes no arguments")
	}
	key := e.nextKey("config", nil)
	v, _, err := e.journal.do(e.ctx, e, key, "config", nil, func() (any, error) {
		cfg := e.s.Config()
		profiles := map[string]any{}
		for name, p := range cfg.Profiles {
			profiles[name] = map[string]any{"model": p.Model, "fallbacks": p.Fallbacks}
		}
		return map[string]any{
			"profiles": profiles,
			"limits": map[string]any{"usd": cfg.Limits.USD, "steps": cfg.Limits.Steps,
				"tool_calls": cfg.Limits.ToolCalls, "depth": cfg.Limits.Depth,
				"concurrency": cfg.Limits.Concurrency},
			"loop": map[string]any{"max_steps": cfg.Loop.MaxSteps,
				"tool_result_bytes": cfg.Loop.ToolResultBytes, "compact_at": cfg.Loop.CompactAt},
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return toStar(v), nil
}

// render("...{name}...", name=...) is pure, so it is not journaled.
func builtinRender(_ *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var tmpl string
	if err := starlark.UnpackPositionalArgs("render", args, nil, 1, &tmpl); err != nil {
		return nil, err
	}
	out := tmpl
	for _, kv := range kwargs {
		k, ok := kv[0].(starlark.String)
		if !ok {
			continue
		}
		out = strings.ReplaceAll(out, "{"+string(k)+"}", starString(kv[1]))
	}
	if strings.Contains(out, "{") && strings.Contains(out, "}") {
		// Leave unknown placeholders visible rather than guessing.
	}
	return starlark.String(out), nil
}

func starString(v starlark.Value) string {
	if s, ok := v.(starlark.String); ok {
		return string(s)
	}
	return v.String()
}

// --- session control adapters (§6.5) --------------------------------------------

// controlCtx returns the context for session control calls. From an
// agent node the node's own ctx governs (delegate/ownership checks);
// from a workflow root node the session root caller governs, so a
// workflow can spawn without being an agent delegate.
func (e *exec) controlCtx() context.Context {
	if c, ok := host.CallerFromContext(e.ctx); ok && c.Kind == "agent" {
		return e.ctx
	}
	return context.Background()
}

func builtinSpawn(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var sv *specValue
	var err error
	if len(args) == 1 {
		if s, ok := args[0].(*specValue); ok {
			sv = s
		}
	}
	if sv == nil {
		sv, err = parseAgentArgs("spawn", args, kwargs)
		if err != nil {
			return nil, err
		}
	}
	key := e.nextKey("spawn", sv.json())
	v, _, err := e.journal.do(e.ctx, e, key, "spawn", sv.json(), func() (any, error) {
		spec := host.ExecSpec{Task: sv.task, Profile: sv.profile, Thread: sv.thread}
		if sv.defn != nil {
			cp := *sv.defn
			spec.Def = &cp
		} else {
			spec.Name = sv.name
		}
		if sv.tools != nil {
			cp := *sv.tools
			spec.Tools = &cp
		}
		spec.Layers = append(spec.Layers, e.layers()...)
		inf, err := e.s.Spawn(e.controlCtx(), host.SpawnSpec{ExecSpec: spec})
		if err != nil {
			return nil, err
		}
		return map[string]any{"handle": inf.ID, "def": inf.Def, "state": inf.State}, nil
	})
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	hid, _ := m["handle"].(string)
	return &handleValue{id: hid, session: true}, nil
}

func builtinSend(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var hv starlark.Value
	var text, mode, requestID string
	if err := starlark.UnpackArgs("send", args, kwargs, "handle", &hv, "input?", &text,
		"mode?", &mode, "request_id?", &requestID); err != nil {
		// Try (handle, text) under the name "text".
		if err2 := starlark.UnpackArgs("send", args, kwargs, "handle", &hv, "text?", &text,
			"mode?", &mode, "request_id?", &requestID); err2 != nil {
			return nil, err
		}
	}
	h, err := oneHandle(hv)
	if err != nil {
		return nil, err
	}
	if !h.session {
		return nil, fmt.Errorf("send: handle %s is a workflow start() handle; use spawn() for reusable agents", h.id)
	}
	if mode == "" {
		mode = host.ModeMessage
	}
	keyArgs := map[string]any{"handle": h.id, "text": text, "mode": mode, "request_id": requestID}
	key := e.nextKey("send", keyArgs)
	v, _, err := e.journal.do(e.ctx, e, key, "send", keyArgs, func() (any, error) {
		r, err := e.s.SendInput(e.controlCtx(), host.SendRequest{
			Target: h.id, Mode: mode, Text: text, RequestID: requestID})
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(r)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m, nil
	})
	if err != nil {
		return nil, err
	}
	return toStar(v), nil
}

func oneHandle(v starlark.Value) (*handleValue, error) {
	switch t := v.(type) {
	case *handleValue:
		return t, nil
	case starlark.String:
		return &handleValue{id: string(t), session: true}, nil
	default:
		return nil, fmt.Errorf("want a handle, got %s", v.Type())
	}
}

func controlBuiltin(op string) func(*exec, *starlark.Thread, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
	return func(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var hv starlark.Value
		if err := starlark.UnpackArgs(op, args, kwargs, "handle", &hv); err != nil {
			return nil, err
		}
		h, err := oneHandle(hv)
		if err != nil {
			return nil, err
		}
		if !h.session {
			return nil, fmt.Errorf("%s: handle %s is a workflow start() handle; use spawn() for reusable agents", op, h.id)
		}
		keyArgs := map[string]any{"handle": h.id}
		key := e.nextKey(op, keyArgs)
		v, _, err := e.journal.do(e.ctx, e, key, op, keyArgs, func() (any, error) {
			var inf host.HandleInfo
			var err error
			switch op {
			case "interrupt":
				inf, err = e.s.Interrupt(e.controlCtx(), h.id)
			case "resume":
				inf, err = e.s.Resume(e.controlCtx(), h.id)
			case "close":
				inf, err = e.s.CloseHandle(e.controlCtx(), h.id)
			case "status":
				inf, err = e.s.Status(e.controlCtx(), h.id)
			}
			if err != nil {
				return nil, err
			}
			b, _ := json.Marshal(inf)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			return m, nil
		})
		if err != nil {
			return nil, err
		}
		return toStar(v), nil
	}
}

func builtinListAgents(e *exec, _ *starlark.Thread, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) > 0 || len(kwargs) > 0 {
		return nil, fmt.Errorf("list_agents: takes no arguments")
	}
	key := e.nextKey("list_agents", nil)
	v, _, err := e.journal.do(e.ctx, e, key, "list_agents", nil, func() (any, error) {
		infos := e.s.ListAgents(e.controlCtx())
		out := make([]any, 0, len(infos))
		for _, inf := range infos {
			b, _ := json.Marshal(inf)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			out = append(out, m)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return toStar(v), nil
}
