package loop

import (
	"encoding/json"
	"github.com/craggy-io/agent-sdk/internal/jschema"
	"github.com/craggy-io/agent-sdk/model"
	"strings"
)

func (r *runner) completeBatch(d *dispatch) stepOutcome {
	calls := d.assistant.Calls
	if r.intermediate {
		d.settleAll(model.SettleNotDispatched, model.Fail(model.CodeInvalid, "intermediate", "this round needs an intermediate text response"))
		return cont()
	}
	if len(calls) > 1 {
		d.settleAll(model.SettleNotDispatched, model.Fail(model.CodeInvalid, "not_dispatched",
			"complete must be the only call in its response; nothing ran"))
		r.appendNotice("complete_alone", "call complete on its own, with no other calls.")
		return cont()
	}
	return r.runComplete(d, calls[0])

}

// runComplete validates and settles a lone `complete` call (§4.3).
func (r *runner) runComplete(d *dispatch, c model.ToolCall) stepOutcome {
	args, ok := jschema.DecodeArgs(nonempty(c.Args))
	fail := func(msg string) stepOutcome {
		d.commit(c, model.Fail(model.CodeInvalid, "invalid_args", "%s", msg), model.SettleInvalidArgs)
		return cont()
	}
	if !ok {
		return fail("complete: arguments are not valid JSON")
	}
	var a struct {
		Summary string          `json:"summary"`
		Output  json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return fail("complete: " + err.Error())
	}
	if err := jschema.Validate(r.policy.Output, nonempty(a.Output)); err != nil {
		// A failed check is returned to the model (§4.3).
		return fail("complete: output does not match the schema: " + err.Error())
	}
	if out, done := r.outputCheckContinue(d, c, a.Output); done {
		return out
	}
	if items, more := r.join(d.ctx); more {
		d.commit(c, model.Fail(model.CodeConflict, "pending_input",
			"complete not accepted yet: an agent you started settled or a message arrived; see what follows and continue"), model.SettleRan)
		r.appendNotices(items)
		return cont()
	}
	if items, contd, err := r.beforeStopContinue(d.ctx); err != nil {
		d.commit(c, model.Fail(model.CodeInternal, "hook_error", "complete rejected: hook error"), model.SettleRan)
		return stop(statusFailed("hook"))
	} else if contd {
		d.commit(c, model.Fail(model.CodeConflict, "stop_deferred",
			"complete not accepted yet; see the notice that follows and continue"), model.SettleRan)
		r.appendNotices(items)
		return cont()
	}
	d.commit(c, model.ToolResult{Text: "completed"}, model.SettleRan)
	r.output = a.Output
	if a.Summary != "" {
		r.lastText = a.Summary
	}
	return stop(StatusDone)
}

// outputCheckContinue runs the per-run output check against a
// schema-valid complete output. done=false means the completion may
// proceed (no check, retries disabled or exhausted, or the check
// passed). A rejection settles the complete call, appends the
// feedback as a notice and continues the run; a check error fails
// the run like a BeforeStop hook error.
func (r *runner) outputCheckContinue(d *dispatch, c model.ToolCall, output json.RawMessage) (stepOutcome, bool) {
	if r.policy.OutputCheck == nil || r.policy.OutputCheckRetries <= 0 ||
		r.outputCheckUsed >= r.policy.OutputCheckRetries {
		return stepOutcome{}, false
	}
	feedback, err := r.policy.OutputCheck(d.ctx, nonempty(output))
	if err != nil {
		d.commit(c, model.Fail(model.CodeInternal, "hook_error", "complete rejected: output check error: %v", err), model.SettleRan)
		return stop(statusFailed("hook")), true
	}
	if strings.TrimSpace(feedback) == "" {
		return stepOutcome{}, false
	}
	r.outputCheckUsed++
	d.commit(c, model.Fail(model.CodeInvalid, "output_rejected",
		"complete not accepted yet; see the notice that follows and continue"), model.SettleRan)
	r.appendNotice("output_check", feedback)
	return cont(), true
}
