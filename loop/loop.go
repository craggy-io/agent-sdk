// Package loop is the one agent loop (§5, rule 6): the step state machine,
// retry, dispatch with an effects lock, settlement on every exit path,
// result truncation/spill, compaction, stop conditions and hooks.
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/transcript"
)

// ToolRunner is the loop's view of tools (a session ToolView satisfies it).
// Groups describes the groups its tools name (model.Tool.Group) for the
// tool catalogue (§4.3); a runner without groups returns nil.
//
// WaitJob blocks until the job a Run handed back (ToolResult.JobID)
// settles or ctx ends (§5.5). done reports a settled job; res is then
// what a synchronous call would have returned — the result, or the
// failure as an error result. A runner without jobs returns done=false.
type ToolRunner interface {
	Tools() []model.Tool
	Groups() []model.ToolGroup
	Run(ctx context.Context, name string, args json.RawMessage) (model.ToolResult, error)
	WaitJob(ctx context.Context, jobID string) (res model.ToolResult, done bool)
}

// Budget is charged before work (§5.7). ChargeStep takes the pre-step token
// estimate and the input price per token (0 when unknown).
type Budget interface {
	ChargeStep(estInputTokens int, price float64) error
	ChargeToolCall() error
	ChargeUSD(float64)
	RetriesLeft() bool
	NoteRetry()
	DeadlineOK() error
	// Runway reports consumption so far against the limits, for the
	// budget runway notices (runway.go).
	Runway() Runway
}

// Runway is a budget's consumption so far against its limits (§5.7). A
// zero limit is uncapped and gets no runway notice.
type Runway struct {
	Limits    config.Limits
	SpentUSD  float64
	Elapsed   time.Duration
	Steps     int
	ToolCalls int
}

// Delegate serves the session's agent built-ins (§4.3, §6.5, §6.7):
// delegation, workflows and agent-to-agent messaging. Tools lists the
// built-ins it serves for this node — fixed for the run, so the tool
// list stays cache-stable — and the loop advertises exactly those and
// routes their calls to Call. A nil Delegate advertises none.
type Delegate interface {
	Tools() []model.Tool
	Call(ctx context.Context, name string, args json.RawMessage) (model.ToolResult, error)
}

// StepView is what BeforeStep sees (§5.8).
type StepView struct {
	NodeID, RunID, SessionID, DefName string
	Step                              int
	Items                             []model.Item
}

// CallView is what AfterCall sees (§5.8).
type CallView struct {
	Call model.ToolCall
	Step int
}

// ResultView is the settled result AfterCall sees (§5.8).
type ResultView struct {
	Item   model.Item
	Result model.ToolResult
}

// StopView is what BeforeStop sees (§5.8).
type StopView struct {
	Status string
	Reason string
	Text   string
	Step   int
	Items  []model.Item
}

// Hooks are application policy (§5.8). Returned items are appended as
// Notices; a hook can never edit existing items. BeforeStop returning items
// continues the loop, at most twice per node.
type Hooks struct {
	BeforeStep func(ctx context.Context, s StepView) ([]model.Item, error)
	AfterCall  func(ctx context.Context, c CallView, r ResultView) ([]model.Item, error)
	BeforeStop func(ctx context.Context, s StopView) ([]model.Item, error)
}

// OutputCheck is a per-run validator for structured completions: it
// runs after complete's output passes schema validation. A non-empty
// feedback string rejects the completion — the feedback reaches the
// model as a notice and the run continues — up to
// Policy.OutputCheckRetries times; after that the next completion is
// accepted unchecked. A non-nil error fails the run, the same way a
// BeforeStop hook error does.
type OutputCheck func(ctx context.Context, output json.RawMessage) (feedback string, err error)

// Policy sets loop behaviour (§5.8). Zero fields take the §3.1 defaults.
type Policy struct {
	MaxSteps   int
	Completion string // text | structured
	Output     json.RawMessage
	// OutputCheck, when set with OutputCheckRetries > 0, validates
	// each structured completion in-run (see OutputCheck). Zero
	// retries disables it. Its rejection counter is separate from
	// the BeforeStop cap.
	OutputCheck        OutputCheck
	OutputCheckRetries int
	ToolResultBytes    int
	// WaitJobs bounds how long a call that returned a job handle
	// waits for the job (ToolRunner.WaitJob) before the model is
	// handed the handle instead of the result. Zero never waits.
	WaitJobs         time.Duration
	CompactAt        float64
	KeepRecentTokens int
	SummaryTokens    int
	// ClearResultsAt is the fraction of the window past which old tool
	// results are sent as a marker that result_read(call_id) reverses
	// (§5.6, clear.go); KeepRecentResults of the newest stay intact.
	// A value at or above CompactAt leaves it to compaction.
	ClearResultsAt    float64
	KeepRecentResults int
	LoopWindow        int
	LoopRepeat        int
	// ProgressiveTools advertises only LoadAlways and ungrouped tools
	// plus tools_load, which adds an on-demand group's tools (§4.3).
	// Off by default: the whole view is advertised.
	ProgressiveTools bool
	// BudgetNoticeAt are the fractions of a limit at which the agent is
	// told how much budget remains and to wrap up (runway.go): once per
	// fraction, the first a wrap-up notice, later ones urgent. Each
	// dimension counts: the run tree's USD, wall clock, steps and tool
	// calls (Budget.Runway) and this agent's MaxSteps. Nil takes the
	// default {0.75, 0.9}; a non-nil list with no fraction in (0, 1)
	// disables the notices.
	BudgetNoticeAt []float64
	Hooks          Hooks
}

func (p Policy) withDefaults() Policy {
	if p.MaxSteps <= 0 {
		p.MaxSteps = 80
	}
	if p.Completion == "" {
		p.Completion = "text"
	}
	if p.ToolResultBytes <= 0 {
		p.ToolResultBytes = 32 * 1024
	}
	if p.CompactAt <= 0 {
		p.CompactAt = 0.8
	}
	if p.KeepRecentTokens <= 0 {
		p.KeepRecentTokens = 20000
	}
	if p.SummaryTokens <= 0 {
		p.SummaryTokens = 4000
	}
	if p.ClearResultsAt <= 0 {
		p.ClearResultsAt = 0.5
	}
	if p.KeepRecentResults <= 0 {
		p.KeepRecentResults = 4
	}
	if p.LoopWindow <= 0 {
		p.LoopWindow = 10
	}
	if p.LoopRepeat <= 0 {
		p.LoopRepeat = 4
	}
	if p.BudgetNoticeAt == nil {
		p.BudgetNoticeAt = []float64{0.75, 0.9}
	}
	return p
}

// Input is one loop invocation (rule 6: every entry point builds this).
type Input struct {
	NodeID, RunID, SessionID, DefName string
	System                            string
	Thread                            *transcript.Transcript
	Task                              string
	Policy                            Policy
	Profile                           config.Profile
	ProfileName                       string
	ContextWindow                     int
	OnDelta                           func(model.Delta)
	// DeliverPending returns mid-run inputs to append at a step boundary
	// (§6.6); HasFollowUp reports queued follow-ups at a would-be stop.
	DeliverPending func() []model.Item
	HasFollowUp    func() bool
	// Join is consulted at a would-be stop after pending input: while
	// agents this node started or woke are still running it blocks
	// until one settles or input arrives (§6.7). more reports the run
	// should continue; items are notices to append (settled agents'
	// results). Pending input is delivered at the next step boundary.
	Join func(ctx context.Context) (items []model.Item, more bool)
	// Skills maps skill name to body for the skill built-in (§4.3).
	Skills map[string]string
}

// Result is the loop outcome. Any stop other than done still returns the
// last assistant text (§5.7).
type Result struct {
	Status     string
	Text       string
	Output     json.RawMessage
	Usage      model.Usage
	Steps      int
	StopReason string
	// OutputCheckRetriesUsed counts completions the output check
	// rejected during this run.
	OutputCheckRetriesUsed int
}

// Deps are the loop's collaborators. Tools, Trace, Budget and Delegate may
// be nil (no tools, no recording, unlimited budget, no delegation).
type Deps struct {
	Provider model.Provider
	Tools    ToolRunner
	Trace    *trace.Recorder
	Budget   Budget
	Delegate Delegate
}

// Status strings (§5.7). Failures are "failed: <code>".
const (
	StatusDone         = "done"
	StatusCancelled    = "cancelled"
	StatusLoop         = "stopped: loop"
	StatusContextUnfit = "stopped: context_unfit"
	StatusRecovery     = "blocked: recovery_required"
)

func statusBudget(which string) string {
	if which == "" {
		which = "unknown"
	}
	return fmt.Sprintf("stopped: budget(%s)", which)
}

func statusFailed(code string) string {
	if code == "" {
		code = "error"
	}
	return "failed: " + code
}

// BudgetError is returned by Budget implementations to name the exhausted
// budget; the loop maps it to "stopped: budget(<Which>)".
type BudgetError struct{ Which string }

func (e *BudgetError) Error() string { return "budget exhausted: " + e.Which }
