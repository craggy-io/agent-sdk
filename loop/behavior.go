package loop

import (
	"context"
	_ "embed"
	"fmt"
	"sync"

	"github.com/craggy-io/script-services/dialect"
	"github.com/craggy-io/script-services/script"
	"go.starlark.net/starlark"
)

//go:embed default.star
var defaultSource []byte

type pendingCalls struct {
	dispatch *dispatch
	why      *settlement
	settle   func() error
}

// action returns authority for exactly this sample's calls. It expires
// after one use and cannot accept a tool name or arguments from the script.
func (r *runner) action(status *string) starlark.Value {
	batch := r.pending
	return starlark.NewBuiltin("act", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
			return nil, err
		}
		if r.pending != batch {
			return nil, fmt.Errorf("these calls have already been settled")
		}
		if err := batch.dispatch.ctx.Err(); err != nil {
			return nil, err
		}
		out := r.dispatchStep(batch.dispatch, batch.why)
		r.pending = nil
		if err := batch.settle(); err != nil {
			return nil, err
		}
		if out.action == actStop {
			*status = out.status
			return starlark.String(*status), nil
		}
		return starlark.String("continue"), nil
	})
}

var defaultBehavior = sync.OnceValues(func() (*starlark.Function, error) {
	program, err := dialect.Compile("default.star", defaultSource, func(string) bool { return false })
	if err != nil {
		return nil, err
	}
	globals, err := program.Init(script.NewThread("default", script.DefaultLimits, nil), nil)
	if err != nil {
		return nil, err
	}
	globals.Freeze()
	return globals["run"].(*starlark.Function), nil
})

type behaviorTurn struct {
	*runner
	ctx    context.Context
	steps  int
	status string
	fatal  error
}

// drive invokes the frozen entry point with one bounded operation. Scripts
// determine the next round, while only the host can accept a final response.
func (r *runner) drive(ctx context.Context) (string, int, error) {
	fn := r.in.Behavior
	if fn == nil {
		var err error
		fn, err = defaultBehavior()
		if err != nil {
			return "", 0, err
		}
	}
	limits := script.DefaultLimits
	if budget := r.budget.Runway(); budget.Limits.WallClock > 0 {
		limits.Timeout = budget.Limits.WallClock - budget.Elapsed
		if limits.Timeout <= 0 {
			return statusBudget("wall_clock"), 0, nil
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()
	turn := &behaviorTurn{runner: r, ctx: runCtx}
	advance := starlark.NewBuiltin("step", turn.advance)
	_, _, err := script.Call(runCtx, limits, "agent loop", fn, starlark.Tuple{advance, starlark.MakeInt(r.policy.MaxSteps)}, nil)
	return turn.finish(ctx, err)
}

func (turn *behaviorTurn) advance(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var instruction string
	var intermediate bool
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "instruction?", &instruction, "intermediate?", &intermediate); err != nil {
		return nil, err
	}
	if turn.status != "" {
		return nil, fmt.Errorf("step called after terminal status %s", turn.status)
	}
	if turn.pending != nil {
		return nil, fmt.Errorf("settle this sample's calls before sampling again")
	}
	if stop, err := turn.prepare(turn.ctx, turn.steps); stop != "" || err != nil {
		turn.status, turn.fatal = stop, err
	} else {
		if instruction != "" {
			turn.appendNotice("instruction", instruction)
		}
		turn.intermediate = intermediate
		turn.steps++
		var outcome stepOutcome
		outcome, turn.fatal = turn.step(turn.ctx, turn.steps)
		if turn.fatal == nil && outcome.action == actCalls {
			return starlark.Tuple{starlark.String("actions"), turn.action(&turn.status)}, nil
		}
		if outcome.action == actStop {
			if outcome.status == "intermediate" {
				return starlark.Tuple{starlark.String("answer"), starlark.String(turn.lastText)}, turn.fatal
			}
			turn.status = outcome.status
		}
	}
	if turn.fatal != nil {
		return nil, turn.fatal
	}
	if turn.status != "" {
		return starlark.Tuple{starlark.String(turn.status), starlark.String(turn.lastText)}, nil
	}
	return starlark.Tuple{starlark.String("continue"), starlark.String("")}, nil
}

func (turn *behaviorTurn) finish(ctx context.Context, err error) (string, int, error) {
	err = turn.settlePending(err)
	if terr := turn.getTraceErr(); terr != nil {
		return "", turn.steps, terr
	}
	switch {
	case turn.fatal != nil:
		return "", turn.steps, turn.fatal
	case ctx.Err() == nil && turn.ctx.Err() != nil && turn.budget.DeadlineOK() != nil:
		turn.status = statusBudget("wall_clock")
	case turn.ctx.Err() != nil || turn.status == StatusCancelled:
		turn.appendAbortedNotice()
		turn.status = StatusCancelled
	case err != nil:
		return "", turn.steps, fmt.Errorf("agent loop: %w", err)
	case turn.status == "" && turn.steps >= turn.policy.MaxSteps:
		turn.status = statusBudget("steps")
	case turn.status == "":
		return "", turn.steps, fmt.Errorf("agent loop returned before a final response")
	}
	return turn.status, turn.steps, nil
}

func (r *runner) prepare(ctx context.Context, step int) (string, error) {
	if ctx.Err() != nil {
		return StatusCancelled, nil
	}
	if err := r.budget.DeadlineOK(); err != nil {
		return statusBudget(budgetWhich(err, "wall_clock")), nil
	}
	if step >= r.policy.MaxSteps {
		return statusBudget("steps"), nil
	}
	r.deliverPending()
	if r.policy.Hooks.BeforeStep != nil {
		items, err := r.policy.Hooks.BeforeStep(ctx, r.stepView(step))
		r.appendNotices(items)
		if err != nil {
			return statusFailed("hook"), nil
		}
	}
	r.maybeRunwayNotice(ctx, step)
	return r.maybeCompact(ctx)
}

func (turn *behaviorTurn) settlePending(err error) error {
	if batch := turn.pending; batch != nil {
		turn.pending = nil
		if settleErr := batch.settle(); settleErr != nil {
			turn.fatal = settleErr
		} else if err == nil {
			return fmt.Errorf("agent loop returned with unsettled calls")
		}
	}
	return err
}
