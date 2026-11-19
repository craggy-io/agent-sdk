package workflow

import (
	"fmt"
	scriptschema "github.com/craggy-io/script-services/schema"
	"github.com/craggy-io/script-services/triggercompile"
	"go.starlark.net/starlark"
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
	pre := starlark.StringDict{
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
	for name, value := range triggercompile.Builtins() {
		pre[name] = value
	}
	if e.chat != nil {
		// A chat turn runs its roles, one after another or side by side,
		// and composes their text.
		return starlark.StringDict{"workflow": pre["workflow"], "schema": pre["schema"], "agent": b("agent", chatAgent),
			"spec": b("spec", chatSpec), "parallel": pre["parallel"], "render": pre["render"], "json": pre["json"]}
	}
	return pre
}
