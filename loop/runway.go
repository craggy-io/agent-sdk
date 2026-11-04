package loop

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Budget runway notices (§5.7): an agent that cannot see its budget runs
// until it is cut off mid-task. At PRECHECK the loop measures every capped
// dimension — the run tree's USD, wall clock, steps and tool calls
// (Budget.Runway, shared with every node) and this agent's own max_steps —
// and when the most consumed one first reaches a Policy.BudgetNoticeAt
// fraction, appends one notice saying what is used and what remains. The
// first fraction asks the agent to wrap up; later ones are urgent. Each
// node gives each notice at most once per run.

// runwayDim is one capped budget dimension.
type runwayDim struct {
	name string  // trace key: usd, wall_clock, steps, tool_calls, max_steps
	frac float64 // share used
	text string  // "$9.40 of $13.00 spent"
}

// noticeFractions is the usable thresholds: in (0, 1), ascending, unique.
func noticeFractions(at []float64) []float64 {
	var out []float64
	for _, f := range at {
		if f > 0 && f < 1 && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}

// runwayDims lists the capped dimensions of rw plus this agent's steps
// (agentSteps of maxSteps).
func runwayDims(rw Runway, agentSteps, maxSteps int) []runwayDim {
	var dims []runwayDim
	l := rw.Limits
	if l.USD > 0 {
		dims = append(dims, runwayDim{"usd", rw.SpentUSD / l.USD,
			fmt.Sprintf("$%.2f of $%.2f spent", rw.SpentUSD, l.USD)})
	}
	if l.WallClock > 0 {
		dims = append(dims, runwayDim{"wall_clock", float64(rw.Elapsed) / float64(l.WallClock),
			fmt.Sprintf("%s of %s elapsed", fmtRunwayDuration(rw.Elapsed), fmtRunwayDuration(l.WallClock))})
	}
	if l.Steps > 0 {
		dims = append(dims, runwayDim{"steps", float64(rw.Steps) / float64(l.Steps),
			fmt.Sprintf("%d/%d steps across the run", rw.Steps, l.Steps)})
	}
	if l.ToolCalls > 0 {
		dims = append(dims, runwayDim{"tool_calls", float64(rw.ToolCalls) / float64(l.ToolCalls),
			fmt.Sprintf("%d/%d tool calls", rw.ToolCalls, l.ToolCalls)})
	}
	if maxSteps > 0 {
		dims = append(dims, runwayDim{"max_steps", float64(agentSteps) / float64(maxSteps),
			fmt.Sprintf("%d/%d of your own steps", agentSteps, maxSteps)})
	}
	return dims
}

// fmtRunwayDuration renders whole minutes ("46m"), or seconds under a
// minute ("40s").
func fmtRunwayDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

var runwayLabels = map[string]string{
	"usd": "USD", "wall_clock": "wall-clock", "steps": "run step",
	"tool_calls": "tool-call", "max_steps": "agent step",
}

// maybeRunwayNotice appends the budget notice for the highest threshold
// the most consumed dimension has reached, if this node has not given it
// yet. agentSteps is the steps this run has taken.
func (r *runner) maybeRunwayNotice(ctx context.Context, agentSteps int) {
	if r.runwayGiven >= len(r.noticeAt) {
		return
	}
	dims := runwayDims(r.budget.Runway(), agentSteps, r.policy.MaxSteps)
	if len(dims) == 0 {
		return
	}
	top := dims[0]
	for _, d := range dims[1:] {
		if d.frac > top.frac {
			top = d
		}
	}
	level := -1
	for i, at := range r.noticeAt {
		if top.frac >= at {
			level = i
		}
	}
	if level < r.runwayGiven {
		return
	}
	r.runwayGiven = level + 1
	r.appendNotice("budget", r.runwayText(level, top, dims))
	used := map[string]float64{}
	for _, d := range dims {
		used[d.name] = d.frac
	}
	_ = r.emit(ctx, "budget.notice", "", map[string]any{
		"threshold": r.noticeAt[level], "urgent": level > 0,
		"limit": top.name, "used": used,
	})
}

func (r *runner) runwayText(level int, top runwayDim, dims []runwayDim) string {
	parts := make([]string, len(dims))
	for i, d := range dims {
		parts[i] = d.text
	}
	finish := "reply with your best partial result as your final answer"
	if r.policy.Completion == "structured" {
		finish = "call complete(summary, output) with your best partial result"
	}
	pct := int(top.frac * 100)
	used := "Budget used so far: " + strings.Join(parts, ", ") + "."
	if level == 0 {
		return fmt.Sprintf("Budget check: %d%% of the %s limit is used. %s "+
			"Start wrapping up: record durable notes or a handoff for the next session with whatever memory or notes tools you have "+
			"(what you established, what is left, where to resume), then %s rather than being cut off when a limit is reached.",
			pct, runwayLabels[top.name], used, finish)
	}
	return fmt.Sprintf("Budget almost exhausted: %d%% of the %s limit is used. %s "+
		"The run stops when any limit is reached. Start no new work: save your notes or handoff now if you have not, "+
		"then %s now.",
		pct, runwayLabels[top.name], used, finish)
}
