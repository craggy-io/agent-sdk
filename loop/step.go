package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/craggy-io/agent-sdk/internal/jschema"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/transcript"
)

const (
	actContinue = iota
	actStop
)

type stepOutcome struct {
	action int
	status string
}

func stop(status string) stepOutcome { return stepOutcome{action: actStop, status: status} }
func cont() stepOutcome              { return stepOutcome{action: actContinue} }

// step runs PROJECT -> SAMPLE (retry) -> CLASSIFY (§5.2). Once an assistant
// item is appended, a deferred settle guarantees every call in it gets
// exactly one result on every exit path (rule 2).
func (r *runner) step(ctx context.Context, stepNum int) (out stepOutcome, err error) {
	stepID := model.NewID()
	var (
		assistant *model.Item // stored assistant item, once appended
		settled   = map[string]bool{}
		starts    = newCallStarts()
		settleWhy = settlement{
			reason: model.SettleAborted,
			text:   model.Fail(model.CodeCancelled, "aborted", "aborted before dispatch; side effects may be partial").Text,
		}
	)
	defer func() {
		if assistant == nil {
			return
		}
		bg := context.WithoutCancel(ctx)
		for _, c := range assistant.Calls {
			if settled[c.ID] {
				continue
			}
			it := model.Item{Kind: model.KindToolResult, CallID: c.ID,
				IsError: true, Settled: settleWhy.reason, Text: settleWhy.text}
			// Settle-once (rule 2) outranks trace fatality here: the
			// result still enters the transcript, but a failed
			// call.end commit makes the step return an error (rule 5).
			_ = r.startCall(bg, stepID, starts, c)
			_ = r.fatalEmit(bg, "call.end", stepID, CallEndData(c, model.ToolResult{Text: it.Text, IsError: true}, it.Settled))
			r.thread.Append(it)
			settled[c.ID] = true
		}
		if terr := r.getTraceErr(); terr != nil && err == nil {
			out, err = stepOutcome{}, terr
		}
	}()

	charged := false
	overflowUsed := false
	retries := 0
	var resp model.Response
	for {
		// PROJECT: pure normalisation of the transcript (§5.4).
		projected, repairs := r.project(ctx)
		projected = r.withCatalogue(projected)
		req := r.buildRequest(projected)
		if !charged {
			// Budgets are charged before the work (§5.7).
			if err := r.budget.ChargeStep(r.estimateTokens(projected), r.inputPrice); err != nil {
				return stop(statusBudget(budgetWhich(err, "steps"))), nil
			}
			charged = true
		}
		// Write before send (rule 5): the request is committed first.
		if r.deps.Trace != nil {
			safe, safeRepairs := traceSafe(projected)
			meta := trace.Event{SessionID: r.in.SessionID, RunID: r.in.RunID, NodeID: r.in.NodeID, StepID: stepID}
			if err := r.deps.Trace.RecordStepRequest(ctx, meta, safe, trace.ParamsOf(req), append(repairs, safeRepairs...)); err != nil {
				return stepOutcome{}, fmt.Errorf("loop: commit step.request: %w", err)
			}
		}
		resp, err = safeSample(ctx, r.deps.Provider, req, r.in.OnDelta)
		if err == nil {
			break
		}
		_ = r.emit(ctx, "step.attempt_failed", stepID, attemptFailedData(err, retries, req.Model))
		r.chargeFailedAttempt(err)
		if ctxErr := ctx.Err(); ctxErr != nil || errors.Is(err, context.Canceled) {
			return stop(StatusCancelled), nil
		}
		var merr *model.Error
		isModelErr := errors.As(err, &merr)
		// One overflow recovery per step: compact, then retry (§5.3, §5.6).
		if isModelErr && merr.ContextExceeded && !overflowUsed {
			overflowUsed = true
			made, st, cerr := r.compact(ctx)
			if cerr != nil {
				return stepOutcome{}, cerr
			}
			if st != "" {
				return stop(st), nil
			}
			if !made {
				return stop(StatusContextUnfit), nil
			}
			continue
		}
		// Transient retry: 3 per step, run budget via Budget (§5.3).
		// Nothing has been dispatched, so a retry repeats no side effect.
		if isTransient(err) && retries < 3 && r.budget.RetriesLeft() {
			r.budget.NoteRetry()
			retries++
			if werr := waitRetry(ctx, err, retries); werr != nil {
				return stop(StatusCancelled), nil
			}
			continue
		}
		code := "provider_error"
		if isModelErr && merr.Code != "" {
			code = merr.Code
		}
		return stop(statusFailed(code)), nil
	}

	// Commit the response before any tool runs (rule 5).
	item := resp.Item
	item.Kind = model.KindAssistant
	if resp.ServedModel != "" {
		item.ServedBy = resp.ServedModel
	}
	sanitiseCallIDs(&item, r.in.NodeID, r.thread.Len(), r.thread.Items())
	stored := r.thread.Append(item)[0]
	assistant = &stored
	r.budget.ChargeUSD(resp.Usage.CostUSD)
	r.usage.Add(resp.Usage)
	if resp.Usage.PromptTokens > 0 {
		r.lastPromptTokens = resp.Usage.PromptTokens
		r.baselineSeq = r.thread.Len()
	}
	if item.Text != "" {
		r.lastText = item.Text
	}
	if r.deps.Trace != nil {
		safeStored, _ := traceSafe([]model.Item{stored})
		h, herr := r.deps.Trace.PutItem(ctx, safeStored[0])
		if herr != nil {
			return stepOutcome{}, fmt.Errorf("loop: commit step.response item: %w", herr)
		}
		if err := r.fatalEmit(ctx, "step.response", stepID, map[string]any{
			"item": h, "served_model": resp.ServedModel, "generation_id": resp.GenerationID, "provider": resp.Provider,
			"usage": resp.Usage, "finish": string(item.Finish),
		}); err != nil {
			// No tool may run against an unrecorded response (rule 5).
			return stepOutcome{}, r.getTraceErr()
		}
	}

	// CLASSIFY on (finish, calls, text) — §5.2 table.
	d := &dispatch{r: r, ctx: ctx, stepID: stepID, stepNum: stepNum,
		assistant: stored, settled: settled, starts: starts}
	outcome := r.classify(ctx, d, stored, &settleWhy)
	if terr := r.getTraceErr(); terr != nil {
		return stepOutcome{}, terr
	}
	return outcome, nil
}

func (r *runner) classify(ctx context.Context, d *dispatch, item model.Item, settleWhy *settlement) stepOutcome {
	finish := item.Finish
	text := strings.TrimSpace(item.Text)
	hasCalls := len(item.Calls) > 0
	if finish != model.FinishLength {
		r.lengthStreak = 0
	}
	if text != "" || hasCalls {
		r.emptyStreak = 0
	}

	switch finish {
	case model.FinishError, model.FinishContentFilter:
		// Calls are never run; they are settled by the defer (§5.2).
		*settleWhy = settlement{reason: model.SettleNotDispatched,
			text: model.Fail(model.CodeCancelled, "not_dispatched", "not dispatched: response finished with %s", finish).Text}
		code := string(finish)
		return stop(statusFailed(code))

	case model.FinishLength:
		r.lengthStreak++
		d.settleAll(model.SettleTruncatedOutput, model.Fail(model.CodeInvalid, "truncated_output",
			"not dispatched: the response was cut off by the output limit (length), so the call may be truncated. Retry with smaller arguments."))
		if r.lengthStreak >= 3 {
			return stop(statusFailed("output_limit"))
		}
		if hasCalls {
			r.appendNotice("output_truncated", "your previous response hit the output limit mid-call; no tool ran. Continue with smaller calls.")
		} else {
			r.appendNotice("continue", "your previous response hit the output limit. Continue.")
		}
		return cont()

	case model.FinishMissing, "":
		if hasCalls && allArgsParse(item.Calls) {
			return r.dispatchStep(d, settleWhy)
		}
		*settleWhy = settlement{reason: model.SettleNotDispatched,
			text: model.Fail(model.CodeInvalid, "not_dispatched", "not dispatched: the response had no finish reason and the call arguments do not parse").Text}
		return stop(statusFailed("missing_finish"))

	default: // stop, tool_calls, and anything else with content
		if hasCalls {
			return r.dispatchStep(d, settleWhy)
		}
		if text == "" {
			// An empty response is never success (§5.2).
			r.emptyStreak++
			if r.emptyStreak >= 2 {
				return stop(statusFailed("empty_response"))
			}
			r.appendNotice("continue", "your previous response was empty. Continue, or finish with a final answer.")
			return cont()
		}
		// Text final.
		if r.policy.Completion == "structured" {
			if !r.structReminded {
				r.structReminded = true
				r.appendNotice("completion", "this agent must finish by calling complete(summary, output) — plain text does not complete the task.")
				return cont()
			}
			return stop(statusFailed("no_completion"))
		}
		return r.finishDone(ctx)
	}
}

// finishDone handles a would-be `done` stop: queued follow-ups continue the
// run, then BeforeStop may continue it at most twice (§5.8).
func (r *runner) finishDone(ctx context.Context) stepOutcome {
	if (r.in.HasFollowUp != nil && r.in.HasFollowUp()) || r.deliverPending() > 0 {
		return cont()
	}
	if items, more := r.join(ctx); more {
		r.appendNotices(items)
		return cont()
	}
	items, contd, err := r.beforeStopContinue(ctx)
	if err != nil {
		return stop(statusFailed("hook"))
	}
	if contd {
		r.appendNotices(items)
		return cont()
	}
	return stop(StatusDone)
}

// join waits out agents this node started or woke before it may stop
// (Input.Join); nil-safe.
func (r *runner) join(ctx context.Context) ([]model.Item, bool) {
	if r.in.Join == nil {
		return nil, false
	}
	return r.in.Join(ctx)
}

// beforeStopContinue consults BeforeStop before accepting a stop. It
// returns the hook's items (not yet appended: the caller appends them once
// any open calls are settled, keeping results contiguous) when the hook
// asks to continue, at most twice per node (§5.8).
func (r *runner) beforeStopContinue(ctx context.Context) ([]model.Item, bool, error) {
	if r.policy.Hooks.BeforeStop == nil {
		return nil, false, nil
	}
	items, err := r.policy.Hooks.BeforeStop(ctx,
		StopView{Status: StatusDone, Text: r.lastText, Items: r.thread.Items()})
	if err != nil {
		return nil, false, err
	}
	if len(items) > 0 && r.beforeStopUsed < 2 {
		r.beforeStopUsed++
		return items, true, nil
	}
	return nil, false, nil
}

func (r *runner) buildRequest(projected []model.Item) model.Request {
	p := r.in.Profile
	return model.Request{
		Model: p.Model, Fallbacks: p.Fallbacks, Items: projected,
		Tools: r.toolList(), Reasoning: p.Reasoning,
		MaxOutputTokens: p.MaxOutputTokens, ParallelToolCalls: p.ParallelToolCalls,
		Provider: p.Provider, Cache: p.Cache, SessionID: r.in.SessionID,
		IdleTimeout: p.IdleTimeout,
	}
}

// toolList is the advertised view, the applicable built-ins, then any
// tools loaded on demand (§4.3, tools.go).
func (r *runner) toolList() []model.Tool {
	tools := r.viewTools()
	tools = append(tools, r.builtinTools()...)
	return append(tools, r.loadedTools()...)
}

func (r *runner) builtinTools() []model.Tool {
	var out []model.Tool
	if r.policy.Completion == "structured" {
		out = append(out, model.Tool{Name: "complete", Effect: "read-only",
			Description: "Finish with a summary and structured output. Must be the only call in its response.",
			Schema:      completeSchema(r.policy.Output)})
	}
	if len(r.in.Skills) > 0 {
		out = append(out, model.Tool{Name: "skill", Effect: "read-only",
			Description: "Load a skill body by name.",
			Schema:      json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"}},"additionalProperties":false}`)})
	}
	// result_read is advertised from the first step, spill or not: a
	// tool list that grows mid-run invalidates the provider's prompt
	// cache (tools precede the conversation in the request).
	if r.policy.ToolResultBytes > 0 {
		out = append(out, model.Tool{Name: "result_read", Effect: "read-only",
			Description: "Page a truncated result by its marker's handle, or a cleared one by its call_id.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"handle":{"type":"string"},"call_id":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"additionalProperties":false}`)})
	}
	if r.progressive {
		out = append(out, toolsLoadTool())
	}
	return append(out, r.delegateTools...)
}

// isDelegateTool reports whether name is a built-in the Delegate serves.
func (r *runner) isDelegateTool(name string) bool {
	for _, t := range r.delegateTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func completeSchema(output json.RawMessage) json.RawMessage {
	if len(output) == 0 {
		output = json.RawMessage(`{}`)
	}
	s := map[string]any{
		"type":     "object",
		"required": []string{"summary", "output"},
		"properties": map[string]any{
			"summary": map[string]any{"type": "string"},
			"output":  json.RawMessage(output),
		},
		"additionalProperties": false,
	}
	b, _ := json.Marshal(s)
	return b
}

// sanitiseCallIDs gives every call a valid unique ID before the assistant
// item enters the transcript (§5.4 rule 3), so the raw transcript itself
// satisfies the invariant checker. Replacement is deterministic.
func sanitiseCallIDs(item *model.Item, nodeID string, nextSeq int, existing []model.Item) {
	seen := map[string]bool{}
	for _, it := range existing {
		for _, c := range it.Calls {
			seen[c.ID] = true
		}
	}
	for i := range item.Calls {
		c := &item.Calls[i]
		if !validID(c.ID) || seen[c.ID] {
			c.ID = transcript.ReplacementID(nodeID, nextSeq, i)
			for salt := 1; seen[c.ID]; salt++ {
				c.ID = transcript.ReplacementID(nodeID, nextSeq, i+1000*salt)
			}
		}
		seen[c.ID] = true
		if _, ok := jschema.DecodeArgs(c.Args); ok {
			c.ArgsOK = true
		} else {
			c.ArgsOK = false
		}
		if len(c.Args) == 0 {
			c.Args = json.RawMessage(`{}`)
			c.ArgsOK = true
		}
	}
}

func validID(id string) bool {
	if id == "" || len(id) > 40 {
		return false
	}
	for _, ch := range id {
		ok := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' ||
			ch >= '0' && ch <= '9' || ch == '_' || ch == '-'
		if !ok {
			return false
		}
	}
	return true
}

func allArgsParse(calls []model.ToolCall) bool {
	for _, c := range calls {
		args := c.Args
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		if !json.Valid(args) {
			return false
		}
	}
	return true
}

func isTransient(err error) bool {
	var merr *model.Error
	return errors.As(err, &merr) && merr.Retryable()
}

// attemptFailedData is the step.attempt_failed payload: the error text
// and retry count, the stable failure class (model.FailureCode), the
// requested model, and — when the error says — the HTTP status, the
// provider's own error code, the upstream provider and any usage the
// provider reported for the failed attempt.
func attemptFailedData(err error, retries int, requested string) map[string]any {
	data := map[string]any{"error": err.Error(), "retries": retries,
		"code": model.FailureCode(err)}
	if requested != "" {
		data["model"] = requested
	}
	var merr *model.Error
	if errors.As(err, &merr) && merr != nil {
		if merr.Status != 0 {
			data["status"] = merr.Status
		}
		if merr.Code != "" {
			data["error_code"] = merr.Code
		}
		if merr.Provider != "" {
			data["provider"] = merr.Provider
		}
		if merr.Usage != nil {
			data["usage"] = *merr.Usage
		}
	}
	return data
}

// chargeFailedAttempt counts usage the provider reported for a failed
// attempt: its cost is charged to the budget and its tokens join the
// node's usage, as for a successful sample (§5.7).
func (r *runner) chargeFailedAttempt(err error) {
	var merr *model.Error
	if !errors.As(err, &merr) || merr == nil || merr.Usage == nil {
		return
	}
	r.budget.ChargeUSD(merr.Usage.CostUSD)
	r.usage.Add(*merr.Usage)
}

// waitRetry sleeps with exponential backoff honouring Retry-After (§5.3).
func waitRetry(ctx context.Context, err error, attempt int) error {
	d := time.Duration(1<<uint(attempt-1)) * 5 * time.Millisecond
	var merr *model.Error
	if errors.As(err, &merr) && merr.RetryAfter > 0 {
		d = merr.RetryAfter
	}
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// safeSample turns a provider panic into an error.
func safeSample(ctx context.Context, p model.Provider, req model.Request, onDelta func(model.Delta)) (resp model.Response, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			resp = model.Response{}
			err = &model.Error{Code: "provider_panic", Message: fmt.Sprintf("provider panicked: %v", rec)}
		}
	}()
	return p.Sample(ctx, req, onDelta)
}

type settlement struct {
	reason model.SettleReason
	text   string
}

// traceSafe copies items for trace blobs. An item's Args are stored exactly
// as received even when invalid (§5.1), but invalid raw JSON cannot be
// marshalled into the canonical item blob; the trace copy wraps such bytes
// as a JSON string and the substitution is logged as a repair. The
// transcript itself is never altered.
func traceSafe(items []model.Item) ([]model.Item, []string) {
	var repairs []string
	out := make([]model.Item, len(items))
	for i, it := range items {
		out[i] = it
		if len(it.Calls) > 0 {
			calls := make([]model.ToolCall, len(it.Calls))
			for j, c := range it.Calls {
				calls[j] = c
				if len(c.Args) > 0 && !json.Valid(c.Args) {
					wrapped, _ := json.Marshal(string(c.Args))
					calls[j].Args = wrapped
					repairs = append(repairs, fmt.Sprintf("seq %d call %q: invalid args stored as string in trace blob", it.Seq, c.ID))
				}
			}
			out[i].Calls = calls
		}
		if it.Reasoning != nil && len(it.Reasoning.Details) > 0 && !json.Valid(it.Reasoning.Details) {
			rr := *it.Reasoning
			wrapped, _ := json.Marshal(string(rr.Details))
			rr.Details = wrapped
			out[i].Reasoning = &rr
			repairs = append(repairs, fmt.Sprintf("seq %d: invalid reasoning details stored as string in trace blob", it.Seq))
		}
	}
	return out, repairs
}
