package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/agent-sdk/internal/jschema"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

// dispatch carries one assistant item's settlement state. Results are
// collected in call order and appended contiguously, with nothing between
// them (§5.2); the step's deferred settle covers anything left open.
type dispatch struct {
	r         *runner
	ctx       context.Context
	stepID    string
	stepNum   int
	assistant model.Item
	settled   map[string]bool
	starts    *callStarts
	results   []model.Item // parallel to assistant.Calls once planned
	planned   []bool
	hookItems []model.Item // AfterCall notices, appended after the results
	hookErr   error
	mu        sync.Mutex // guards planned/outcomes written by tool goroutines
}

// CallEndData is the call.end payload for call c settled with res and
// reason; session crash recovery emits the same shape (plus
// "recovered": true) for calls a dead process left open.
func CallEndData(c model.ToolCall, res model.ToolResult, settled model.SettleReason) map[string]any {
	data := map[string]any{"call_id": c.ID, "name": c.Name, "settled": string(settled),
		"is_error": res.IsError, "code": res.Code, "bytes": len(res.Text)}
	if res.JobID != "" {
		data["job_id"] = res.JobID
	}
	return data
}

// callStarts records which calls of one step have a call.start, so
// every call.end follows one — calls settled without running
// (validation, budget, not dispatched) included. Safe for the
// concurrent read-only group.
type callStarts struct {
	mu sync.Mutex
	m  map[string]bool
}

func newCallStarts() *callStarts { return &callStarts{m: map[string]bool{}} }

// startCall commits call.start for c unless it already has one.
func (r *runner) startCall(ctx context.Context, stepID string, starts *callStarts, c model.ToolCall) error {
	starts.mu.Lock()
	seen := starts.m[c.ID]
	starts.m[c.ID] = true
	starts.mu.Unlock()
	if seen {
		return nil
	}
	return r.fatalEmit(ctx, "call.start", stepID, map[string]any{"call_id": c.ID, "name": c.Name})
}

// settleAll settles every call of the assistant item with one reason (used
// for length / complete-alone paths) and appends the results contiguously.
func (d *dispatch) settleAll(reason model.SettleReason, res model.ToolResult) {
	for _, c := range d.assistant.Calls {
		d.commit(c, res, reason)
	}
}

// commit records call.end (before the result enters the transcript, rule 5)
// then appends the result item and marks the call settled. A failed
// call.end commit is fatal to the step (recorded via fatalEmit), but the
// result still settles the call exactly once in the transcript (rule 2).
func (d *dispatch) commit(c model.ToolCall, res model.ToolResult, reason model.SettleReason) model.Item {
	_ = d.r.startCall(context.WithoutCancel(d.ctx), d.stepID, d.starts, c)
	_ = d.r.fatalEmit(context.WithoutCancel(d.ctx), "call.end", d.stepID, CallEndData(c, res, reason))
	it := model.Item{Kind: model.KindToolResult, CallID: c.ID,
		IsError: res.IsError, Settled: reason, Text: res.Text}
	stored := d.r.thread.Append(it)[0]
	d.settled[c.ID] = true
	return stored
}

// dispatchStep is DISPATCH in the §5.2 state machine.
func (r *runner) dispatchStep(d *dispatch, settleWhy *settlement) stepOutcome {
	calls := d.assistant.Calls

	// `complete` stands alone (§5.2, §4.3).
	if hasComplete(calls) {
		if len(calls) > 1 {
			d.settleAll(model.SettleNotDispatched, model.Fail(model.CodeInvalid, "not_dispatched",
				"complete must be the only call in its response; nothing ran"))
			r.appendNotice("complete_alone", "call complete on its own, with no other calls.")
			return cont()
		}
		return r.runComplete(d, calls[0])
	}

	d.results = make([]model.Item, len(calls))
	d.planned = make([]bool, len(calls))
	tools := toolIndex(r.toolList())

	type plan struct {
		idx     int
		call    model.ToolCall
		args    json.RawMessage // decoded, validated arguments
		effect  string
		blocked *settledResult // pre-settled (validation/unknown/budget)
	}
	plans := make([]plan, len(calls))
	budgetStop := ""
	for i, c := range calls {
		p := plan{idx: i, call: c}
		tool, known := tools[c.Name]
		// Arguments are checked per call, not per batch (§5.2).
		args, ok := jschema.DecodeArgs(nonempty(c.Args))
		switch {
		case !known:
			p.blocked = &settledResult{res: r.unknownTool(c.Name), reason: model.SettleUnknownTool}
		case !ok:
			p.blocked = &settledResult{res: model.Fail(model.CodeInvalid, "invalid_args",
				"invalid arguments: not valid JSON (even after one level of double-encoding)"),
				reason: model.SettleInvalidArgs}
		default:
			if err := jschema.Validate(tool.Schema, args); err != nil {
				p.blocked = &settledResult{res: invalidArgs(err), reason: model.SettleInvalidArgs}
			}
		}
		if p.blocked == nil {
			// Budgets are charged before the work (§5.7).
			if err := r.budget.ChargeToolCall(); err != nil {
				budgetStop = budgetWhich(err, "tool_calls")
				p.blocked = &settledResult{res: model.Fail(model.CodeDenied, "budget",
					"tool-call budget exhausted: no more tools can run in this run"), reason: model.SettleBudget}
			} else {
				p.args = args
				p.effect = tool.Effect
			}
		}
		plans[i] = p
	}

	outcomes := make([]settledResult, len(plans))
	for i, p := range plans {
		if p.blocked != nil {
			outcomes[i] = *p.blocked
			d.planned[i] = true
		}
	}
	// Effects lock (§5.2): read-only calls share, everything else runs alone
	// in order. Consecutive read-only calls run concurrently; results are
	// still appended in call order below.
	cancelled := false
	for i := 0; i < len(plans); {
		if d.planned[i] {
			i++
			continue
		}
		if plans[i].effect == "read-only" {
			j := i
			var group []plan
			for j < len(plans) && !d.planned[j] && plans[j].effect == "read-only" {
				group = append(group, plans[j])
				j++
			}
			var wg sync.WaitGroup
			for _, p := range group {
				wg.Add(1)
				go func(p plan) {
					defer wg.Done()
					res := d.execute(p.call, p.args)
					d.mu.Lock()
					outcomes[p.idx] = res
					d.planned[p.idx] = true
					d.mu.Unlock()
				}(p)
			}
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-d.ctx.Done():
				cancelled = true
				// Outstanding calls are settled `aborted`; tool goroutines
				// that ignore ctx are abandoned (§5.2 deferred text).
			}
			i = j
		} else {
			p := plans[i]
			doneCh := make(chan settledResult, 1)
			go func() { doneCh <- d.execute(p.call, p.args) }()
			select {
			case outcomes[i] = <-doneCh:
				d.planned[i] = true
			case <-d.ctx.Done():
				cancelled = true
			}
			i++
		}
		if cancelled {
			break
		}
	}

	// Append results in call order, contiguous (§5.2). Unfinished calls
	// (cancel) stay open for the deferred settle.
	d.mu.Lock()
	planned := append([]bool(nil), d.planned...)
	outcomesCopy := append([]settledResult(nil), outcomes...)
	d.mu.Unlock()
	for i, c := range calls {
		if planned[i] {
			d.results[i] = d.commit(c, outcomesCopy[i].res, outcomesCopy[i].reason)
		}
	}
	outcomes, d.planned = outcomesCopy, planned
	// Loop detector and AfterCall hooks run after every result is in, so
	// their notices never split a result block.
	if st := r.noteCalls(calls, outcomes, d.planned, d); st != "" && !cancelled && budgetStop == "" && d.hookErr == nil {
		*settleWhy = settlement{reason: model.SettleAborted,
			text: model.Fail(model.CodeCancelled, "loop_detected", "aborted: loop detected").Text}
		r.appendNotices(d.hookItems)
		return stop(st)
	}
	r.appendNotices(d.hookItems)
	switch {
	case cancelled || d.ctx.Err() != nil:
		*settleWhy = settlement{reason: model.SettleAborted,
			text: model.Fail(model.CodeCancelled, "aborted", "aborted; side effects may be partial").Text}
		// The deferred settle runs first (defer order is the step's), so
		// append the notice here would split results — it is appended by
		// returning cancelled through a wrapper below instead.
		return stop(StatusCancelled)
	case d.hookErr != nil:
		return stop(statusFailed("hook"))
	case budgetStop != "":
		*settleWhy = settlement{reason: model.SettleBudget,
			text: model.Fail(model.CodeDenied, "budget", "budget exhausted: the run is stopping").Text}
		return stop(statusBudget(budgetStop))
	}
	return cont()
}

type settledResult struct {
	res    model.ToolResult
	reason model.SettleReason
}

// execute runs one validated call (built-in or runner) with panic recovery.
func (d *dispatch) execute(c model.ToolCall, args json.RawMessage) settledResult {
	if err := d.r.startCall(d.ctx, d.stepID, d.starts, c); err != nil {
		// Nothing runs against an unrecorded call.start (rule 5).
		res := model.Fail(model.CodeInternal, "trace_error", "not dispatched: trace commit of call.start failed")
		res.Code = "trace_error"
		return settledResult{res: res, reason: model.SettleAborted}
	}
	start := time.Now()
	running := false
	res := runToolSafely(d.ctx, func(ctx context.Context) (model.ToolResult, error) {
		res, err := d.r.invoke(ctx, c.Name, args)
		if err == nil && res.JobID != "" {
			res, running = d.r.waitJob(ctx, res)
		}
		return res, err
	}, func(stack string) {
		_ = d.r.emit(context.WithoutCancel(d.ctx), "call.panic", d.stepID,
			map[string]any{"call_id": c.ID, "name": c.Name, "stack": stack})
	})
	if res.JobID != "" && res.Text == "" {
		// A long job that outlived loop.wait_jobs hands back its handle
		// (§5.5).
		res.Text = fmt.Sprintf("job %s is still running; poll it with its job handle", res.JobID)
	}
	if c.Name != "result_read" {
		res.Text = d.r.truncateAndSpill(d.ctx, c, res.Text)
	}
	res.Text = model.AppendNotes(res.Text, timingNotes(res.JobID, running, time.Since(start))...)
	return settledResult{res: res, reason: model.SettleRan}
}

// timingNotes tell the model what a call cost in time (§5.5): that a
// job is still running after the loop waited for it, or that a call
// took long enough to weigh before repeating it. Quick calls get none.
func timingNotes(jobID string, running bool, elapsed time.Duration) []string {
	if running {
		return []string{fmt.Sprintf("job %s is still running after %s; wait for it with its job handle",
			jobID, elapsed.Round(100*time.Millisecond))}
	}
	if n := model.ElapsedNote(elapsed); n != "" {
		return []string{n}
	}
	return nil
}

// waitJob gives a call's job up to Policy.WaitJobs to settle (§5.5). A
// job that finishes in time hands the model its result, exactly like a
// synchronous call, so short jobs never cost a jobs_wait step; one
// still running keeps its handle, and running reports that it was
// waited for and did not settle. JobID stays set either way so
// call.end links the call to its job.
func (r *runner) waitJob(ctx context.Context, res model.ToolResult) (_ model.ToolResult, running bool) {
	if r.policy.WaitJobs <= 0 || r.deps.Tools == nil {
		return res, false
	}
	wctx, cancel := context.WithTimeout(ctx, r.policy.WaitJobs)
	defer cancel()
	settled, done := r.deps.Tools.WaitJob(wctx, res.JobID)
	if !done {
		return res, true
	}
	settled.JobID = res.JobID
	return settled, false
}

// invoke routes a validated call to a built-in or the tool runner.
func (r *runner) invoke(ctx context.Context, name string, args json.RawMessage) (model.ToolResult, error) {
	switch {
	case name == "skill":
		var a struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return invalidArgs(err), nil
		}
		body, ok := r.in.Skills[a.Name]
		if !ok {
			return model.Fail(model.CodeNotFound, "unknown_skill", "unknown skill: %s", a.Name), nil
		}
		return model.ToolResult{Text: body}, nil
	case name == "result_read":
		var a struct {
			Handle string `json:"handle"`
			CallID string `json:"call_id"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return invalidArgs(err), nil
		}
		if (a.Handle == "") == (a.CallID == "") {
			return model.Fail(model.CodeInvalid, "invalid_args", "invalid arguments: pass exactly one of handle and call_id"), nil
		}
		if a.CallID != "" {
			full, ok := r.storedResult(a.CallID)
			if !ok {
				return model.Fail(model.CodeNotFound, "unknown_call_id", "unknown call_id: %s", a.CallID), nil
			}
			return r.resultPage(full, a.Offset, a.Limit), nil
		}
		r.spillMu.Lock()
		full, ok := r.spills[a.Handle]
		r.spillMu.Unlock()
		if !ok && r.deps.Trace != nil {
			if h, has := r.spillBlobHashes[a.Handle]; has {
				if b, err := r.deps.Trace.Sink.GetBlob(ctx, h); err == nil {
					full, ok = string(b), true
				}
			}
		}
		if !ok {
			return model.Fail(model.CodeNotFound, "unknown_handle", "unknown result handle: %s", a.Handle), nil
		}
		return r.resultPage(full, a.Offset, a.Limit), nil
	case name == toolsLoadName && r.progressive:
		return r.runToolsLoad(args), nil
	case r.isDelegateTool(name):
		return r.deps.Delegate.Call(ctx, name, args)
	}
	if r.deps.Tools == nil {
		return r.unknownTool(name), nil
	}
	res, err := r.deps.Tools.Run(ctx, name, args)
	if err != nil {
		if res.Text == "" {
			res.Text = model.Failure(err).Text
		}
		res.IsError = true
	}
	return res, nil
}

// runToolSafely converts a tool panic into an error result (§5.2) and
// reports the stack via onPanic.
func runToolSafely(ctx context.Context, fn func(context.Context) (model.ToolResult, error), onPanic func(string)) (res model.ToolResult) {
	defer func() {
		if rec := recover(); rec != nil {
			if onPanic != nil {
				onPanic(string(debug.Stack()))
			}
			res = model.Fail(model.CodeInternal, "tool_panicked", "tool panicked: %v", rec)
			res.Code = "panic"
		}
	}()
	res, err := fn(ctx)
	if err != nil && !res.IsError {
		res.IsError = true
		if res.Text == "" {
			res.Text = model.Failure(err).Text
		}
	}
	return res
}

// resultPage is one result_read page of full: limit bytes from offset,
// tool_result_bytes when limit is unset.
func (r *runner) resultPage(full string, offset, limit int) model.ToolResult {
	if limit <= 0 {
		limit = r.policy.ToolResultBytes
	}
	if offset < 0 || offset > len(full) {
		return model.Fail(model.CodeInvalid, "invalid_args", "offset %d out of range 0..%d", offset, len(full))
	}
	end := offset + limit
	if end > len(full) {
		end = len(full)
	}
	return model.ToolResult{Text: full[offset:end]}
}

// truncateAndSpill cuts an oversized result to head+tail and spills the
// full bytes to the trace blob store (§5.5).
func (r *runner) truncateAndSpill(ctx context.Context, c model.ToolCall, text string) string {
	limit := r.policy.ToolResultBytes
	if limit <= 0 || len(text) <= limit {
		return text
	}
	handle := spillHandle(c.ID, text)
	r.spillMu.Lock()
	r.spills[handle] = text
	r.spillMu.Unlock()
	if r.deps.Trace != nil {
		if h, err := r.deps.Trace.PutBlob(ctx, []byte(text)); err == nil {
			r.spillMu.Lock()
			r.spillHashes()[handle] = h
			r.spillMu.Unlock()
		}
	}
	head := limit / 2
	tail := limit - head
	elided := len(text) - head - tail
	return validHead(text, head) +
		fmt.Sprintf("\n[… %d bytes elided; result_read(handle=%q) pages the rest]\n", elided, handle) +
		validTail(text, tail)
}

func spillHandle(callID, text string) string {
	sum := sha256.Sum256([]byte(callID + "|" + text))
	return "r_" + hex.EncodeToString(sum[:])[:16]
}

func (r *runner) spillHashes() map[string]trace.Hash {
	if r.spillBlobHashes == nil {
		r.spillBlobHashes = map[string]trace.Hash{}
	}
	return r.spillBlobHashes
}

func validHead(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func validTail(s string, n int) string {
	if n >= len(s) {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

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

// noteCalls feeds the loop detector (§5.7) and runs AfterCall hooks. It
// returns a stop status when the loop detector trips.
func (r *runner) noteCalls(calls []model.ToolCall, outcomes []settledResult, planned []bool, d *dispatch) string {
	status := ""
	for i, c := range calls {
		if !planned[i] {
			continue
		}
		if r.policy.Hooks.AfterCall != nil && d.hookErr == nil {
			items, err := r.policy.Hooks.AfterCall(d.ctx,
				CallView{Call: c, Step: d.stepNum},
				ResultView{Item: d.results[i], Result: outcomes[i].res})
			d.hookItems = append(d.hookItems, items...)
			if err != nil {
				d.hookErr = err
			}
		}
		if outcomes[i].reason != model.SettleRan {
			continue
		}
		sum := sha256.Sum256([]byte(c.Name + "|" + string(nonempty(c.Args)) + "|" + outcomes[i].res.Text))
		h := hex.EncodeToString(sum[:])[:16]
		r.callHashes = append(r.callHashes, h)
		window := r.callHashes
		if len(window) > r.policy.LoopWindow {
			window = window[len(window)-r.policy.LoopWindow:]
		}
		count := 0
		for _, x := range window {
			if x == h {
				count++
			}
		}
		if count > r.policy.LoopRepeat {
			status = StatusLoop
		} else if count == 3 && !r.loopNoticed[h] {
			r.loopNoticed[h] = true
			d.hookItems = append(d.hookItems, model.Item{Kind: model.KindNotice,
				Text: `<notice kind="loop">you have made the same call with the same result 3 times in the last 10 calls; change approach or finish</notice>`})
		}
	}
	return status
}

func hasComplete(calls []model.ToolCall) bool {
	for _, c := range calls {
		if c.Name == "complete" {
			return true
		}
	}
	return false
}

func toolIndex(tools []model.Tool) map[string]model.Tool {
	m := make(map[string]model.Tool, len(tools))
	for _, t := range tools {
		m[t.Name] = t
	}
	return m
}

func nonempty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}
