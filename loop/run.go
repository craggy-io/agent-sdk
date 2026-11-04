package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/transcript"
)

type runner struct {
	deps   Deps
	in     Input
	policy Policy
	thread *transcript.Transcript
	budget Budget

	window     int     // served model's context window (0 = unknown)
	inputPrice float64 // per-token input price (0 = unknown)

	usage           model.Usage
	lastText        string
	output          json.RawMessage
	spillMu         sync.Mutex
	spills          map[string]string // result_read handle -> full text
	spillBlobHashes map[string]trace.Hash
	abortedNoticed  bool
	callHashes      []string
	loopNoticed     map[string]bool
	delegateTools   []model.Tool // the Delegate's built-ins, fixed per run

	// Progressive tool loading (§4.3, tools.go): decided once per run.
	progressive bool
	toolsMu     sync.Mutex
	loaded      []string // on-demand groups loaded, in load order

	traceMu  sync.Mutex
	traceErr error // first fatal trace-commit failure (rule 5)

	lengthStreak     int
	emptyStreak      int
	structReminded   bool
	beforeStopUsed   int
	outputCheckUsed  int
	lastPromptTokens int // provider prompt_tokens baseline, 0 = none (§5.6)
	baselineSeq      int // thread length when baseline was sampled
	clearedResults   int // tool results the projection clears (clear.go)

	noticeAt    []float64 // budget runway thresholds (runway.go)
	runwayGiven int       // thresholds already noticed
}

// Run executes the agent loop (§5.2). Ordinary stops are encoded in
// Result.Status; err is returned only for setup faults (no provider, a
// trace write-before-send failure) where no honest status exists.
func Run(ctx context.Context, deps Deps, in Input) (Result, error) {
	if deps.Provider == nil {
		return Result{}, fmt.Errorf("loop: nil Provider")
	}
	r := &runner{
		deps: deps, in: in, policy: in.Policy.withDefaults(),
		thread: in.Thread, budget: deps.Budget,
		window: in.ContextWindow,
		spills: map[string]string{}, loopNoticed: map[string]bool{},
	}
	r.noticeAt = noticeFractions(r.policy.BudgetNoticeAt)
	if r.thread == nil {
		r.thread = transcript.New()
	}
	if deps.Delegate != nil {
		r.delegateTools = deps.Delegate.Tools()
	}
	if r.budget == nil {
		r.budget = NewBudget()
	}
	if r.in.NodeID == "" {
		r.in.NodeID = model.NewID()
	}
	if r.in.RunID == "" {
		r.in.RunID = r.in.NodeID
	}
	r.seedThread()
	r.initTools()
	r.lookupModel(ctx)
	r.emit(ctx, "loop.start", "", map[string]any{
		"def": r.in.DefName, "profile": r.in.ProfileName, "model": r.in.Profile.Model,
	})

	status := ""
	var runErr error
	step := 0
loop:
	for {
		// PRECHECK (§5.2): budget / cancel / max_steps before any work.
		if ctx.Err() != nil {
			status = StatusCancelled
			r.appendAbortedNotice()
			break
		}
		if err := r.budget.DeadlineOK(); err != nil {
			status = statusBudget(budgetWhich(err, "wall_clock"))
			break
		}
		if step >= r.policy.MaxSteps {
			status = statusBudget("steps")
			break
		}
		r.deliverPending()
		if r.policy.Hooks.BeforeStep != nil {
			items, err := r.policy.Hooks.BeforeStep(ctx, r.stepView(step))
			r.appendNotices(items)
			if err != nil {
				status = statusFailed("hook")
				break
			}
		}
		r.maybeRunwayNotice(ctx, step)
		// POSTCHECK of the previous step: compaction trigger (§5.6).
		if st, err := r.maybeCompact(ctx); err != nil {
			runErr = err
			break loop
		} else if st != "" {
			status = st
			break
		}
		step++
		out, err := r.step(ctx, step)
		if err != nil {
			runErr = err
			break
		}
		switch out.action {
		case actContinue:
			continue
		case actStop:
			status = out.status
			if status == StatusCancelled {
				// The step's deferred settle has run by now.
				r.appendAbortedNotice()
			}
			break loop
		}
	}
	if runErr != nil {
		return Result{}, runErr
	}
	res := Result{
		Status: status, Text: r.lastText, Output: r.output,
		Usage: r.usage, Steps: step, StopReason: strings.TrimPrefix(status, "stopped: "),
		OutputCheckRetriesUsed: r.outputCheckUsed,
	}
	if status == StatusDone {
		res.StopReason = "done"
	}
	r.emit(context.WithoutCancel(ctx), "loop.end", "", map[string]any{"status": status, "steps": step})
	return res, nil
}

func (r *runner) seedThread() {
	if r.thread.Len() == 0 {
		if r.in.System != "" {
			r.thread.Append(model.Item{Kind: model.KindSystem, Text: r.in.System})
		}
		if r.in.Task != "" {
			r.thread.Append(model.Item{Kind: model.KindUser, Text: r.in.Task})
		}
		return
	}
	if r.in.Task != "" {
		r.thread.Append(model.Item{Kind: model.KindUser, Text: r.in.Task})
	}
}

func (r *runner) lookupModel(ctx context.Context) {
	if r.in.Profile.Model == "" {
		return
	}
	info, err := r.deps.Provider.Model(ctx, r.in.Profile.Model)
	if err != nil {
		return
	}
	if r.window == 0 {
		r.window = info.ContextWindow
	}
	r.inputPrice = info.InputPricePerToken
}

func (r *runner) stepView(step int) StepView {
	return StepView{NodeID: r.in.NodeID, RunID: r.in.RunID, SessionID: r.in.SessionID,
		DefName: r.in.DefName, Step: step, Items: r.thread.Items()}
}

func (r *runner) deliverPending() int {
	if r.in.DeliverPending == nil {
		return 0
	}
	items := r.in.DeliverPending()
	for _, it := range items {
		if it.Kind == "" {
			it.Kind = model.KindUser
		}
		r.thread.Append(it)
	}
	return len(items)
}

// appendNotices appends hook items, forced to Notice kind (rule 7, §5.8).
func (r *runner) appendNotices(items []model.Item) {
	for _, it := range items {
		text := it.Text
		if !strings.HasPrefix(text, "<notice") {
			text = `<notice kind="hook">` + text + `</notice>`
		}
		r.thread.Append(model.Item{Kind: model.KindNotice, Text: text, InputID: it.InputID, Origin: it.Origin})
	}
}

// appendAbortedNotice appends the turn_aborted marker exactly once, and
// only after open calls are settled so it never splits a result block.
func (r *runner) appendAbortedNotice() {
	if r.abortedNoticed {
		return
	}
	r.abortedNoticed = true
	r.appendNotice("turn_aborted", "turn aborted; side effects may be partial")
}

func (r *runner) appendNotice(kind, msg string) {
	r.thread.Append(model.Item{Kind: model.KindNotice,
		Text: fmt.Sprintf(`<notice kind="%s">%s</notice>`, kind, msg)})
}

func budgetWhich(err error, fallback string) string {
	var be *BudgetError
	if errors.As(err, &be) && be.Which != "" {
		return be.Which
	}
	return fallback
}

// fatalEmit is emit for write-before-send commit points (step.response,
// call.start, call.end): a failure is recorded as the run's fatal trace
// error, so dispatch stops and Run returns it instead of reporting a
// success the trace cannot reconstruct (rule 5, §7.2).
func (r *runner) fatalEmit(ctx context.Context, typ, stepID string, data any) error {
	if err := r.emit(ctx, typ, stepID, data); err != nil {
		r.setTraceErr(fmt.Errorf("loop: commit %s: %w", typ, err))
		return err
	}
	return nil
}

func (r *runner) setTraceErr(err error) {
	r.traceMu.Lock()
	defer r.traceMu.Unlock()
	if r.traceErr == nil {
		r.traceErr = err
	}
}

func (r *runner) getTraceErr() error {
	r.traceMu.Lock()
	defer r.traceMu.Unlock()
	return r.traceErr
}

// emit records a trace event; nil-recorder safe. Failures are returned so
// write-before-send points can treat them as fatal (rule 5).
func (r *runner) emit(ctx context.Context, typ, stepID string, data any) error {
	if r.deps.Trace == nil {
		return nil
	}
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		raw = b
	}
	return r.deps.Trace.Emit(ctx, trace.Event{
		Type: typ, Time: time.Now().UTC(), SessionID: r.in.SessionID,
		RunID: r.in.RunID, NodeID: r.in.NodeID, StepID: stepID, Data: raw,
	})
}

// estimateTokens counts per §5.6: the provider's real prompt_tokens from
// the previous step plus bytes/4 for items appended since; without a
// baseline it estimates the whole projection plus tool schemas.
func (r *runner) estimateTokens(projected []model.Item) int {
	tools := r.toolTokens()
	if r.lastPromptTokens > 0 {
		items := r.thread.Items()
		if r.baselineSeq <= len(items) {
			return r.lastPromptTokens + transcript.EstimateTokens(items[r.baselineSeq:]) + tools
		}
	}
	return transcript.EstimateTokens(projected) + tools
}

// toolTokens estimates the advertised tool definitions at bytes/4.
func (r *runner) toolTokens() int {
	n := 0
	for _, t := range r.toolList() {
		n += (len(t.Name) + len(t.Description) + len(t.Schema)) / 4
	}
	return n
}

// maybeCompact runs compaction when the trigger fires (§5.6). It returns a
// stop status (context_unfit / compaction_failed / budget) or "".
func (r *runner) maybeCompact(ctx context.Context) (string, error) {
	if r.window <= 0 {
		return "", nil
	}
	for rounds := 0; rounds < 3; rounds++ {
		projected, _ := r.project(ctx)
		est := r.estimateTokens(projected)
		if float64(est+r.in.Profile.MaxOutputTokens) <= r.policy.CompactAt*float64(r.window) {
			return "", nil
		}
		made, st, err := r.compact(ctx)
		if err != nil {
			return "", err
		}
		if st != "" {
			return st, nil
		}
		if !made {
			// Nothing left to compact: fit check decides (§5.6).
			if est+r.in.Profile.MaxOutputTokens > r.window {
				r.emit(ctx, "context.unfit", "", r.unfitCounts(projected, est))
				return StatusContextUnfit, nil
			}
			return "", nil
		}
	}
	// Bounded rounds exhausted: final fit check.
	projected, _ := r.project(ctx)
	est := r.estimateTokens(projected)
	if est+r.in.Profile.MaxOutputTokens > r.window {
		r.emit(ctx, "context.unfit", "", r.unfitCounts(projected, est))
		return StatusContextUnfit, nil
	}
	return "", nil
}

func (r *runner) unfitCounts(projected []model.Item, est int) map[string]any {
	byKind := map[string]int{}
	for _, it := range projected {
		byKind[string(it.Kind)] += transcript.EstimateTokens([]model.Item{it})
	}
	return map[string]any{"estimate": est, "window": r.window,
		"max_output": r.in.Profile.MaxOutputTokens, "by_kind": byKind}
}
