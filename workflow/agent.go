package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/craggy-io/agent-sdk/host"
)

// runSpec executes one spec to completion (no journal; callers journal).
// Agent-level failures come back as Results, never errors (§6.2).
func (e *exec) runSpec(ctx context.Context, sv *specValue) map[string]any {
	name := sv.agentName()
	if !e.allowed(name) {
		return map[string]any{"status": "failed: not_allowed", "text": "",
			"def": name, "node": "", "run": "",
			"error": fmt.Sprintf("agent %q is not in the workflow agents allowlist", name)}
	}
	if e.chat != nil {
		return e.chat.run(ctx, sv, e.invoke, e.layers())
	}
	return e.runWorkflowAgent(ctx, sv, name)
}

func (e *exec) runWorkflowAgent(ctx context.Context, sv *specValue, name string) map[string]any {
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
	res, err := e.invoke(ctx, spec)
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
