package loop

import (
	"sync"
	"time"

	"github.com/craggy-io/agent-sdk/config"
)

// SimpleBudget is the standalone Budget (§5.3, §5.7): the first Limits given
// caps USD (pre-step estimate plus actual charges), steps, tool calls and
// wall clock; retries default to 10 per run. Zero caps mean unlimited.
type SimpleBudget struct {
	MaxRetries int // per run; 10 when zero

	mu        sync.Mutex
	limits    config.Limits
	now       func() time.Time
	start     time.Time
	steps     int
	toolCalls int
	retries   int
	spentUSD  float64
}

var _ Budget = (*SimpleBudget)(nil)

// NewBudget builds a SimpleBudget from optional limits.
func NewBudget(limits ...config.Limits) *SimpleBudget {
	b := &SimpleBudget{now: time.Now, start: time.Now(), MaxRetries: 10}
	if len(limits) > 0 {
		b.limits = limits[0]
	}
	return b
}

// SetClock replaces the budget's clock and restarts its wall clock at
// now(): elapsed time and the wall_clock limit are measured from there.
func (b *SimpleBudget) SetClock(now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = now
	b.start = now()
}

// Runway reports consumption so far against the limits.
func (b *SimpleBudget) Runway() Runway {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Runway{Limits: b.limits, SpentUSD: b.spentUSD, Elapsed: b.now().Sub(b.start),
		Steps: b.steps, ToolCalls: b.toolCalls}
}

// SpentUSD reports actual USD charged via ChargeUSD.
func (b *SimpleBudget) SpentUSD() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spentUSD
}

func (b *SimpleBudget) ChargeStep(estInputTokens int, price float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.deadlineLocked(); err != nil {
		return err
	}
	// Pre-step estimate: prompt x input price against the remaining budget.
	if b.limits.USD > 0 && price > 0 && estInputTokens > 0 {
		if b.spentUSD+float64(estInputTokens)*price > b.limits.USD {
			return &BudgetError{Which: "usd"}
		}
	}
	if b.limits.Steps > 0 && b.steps+1 > b.limits.Steps {
		return &BudgetError{Which: "steps"}
	}
	b.steps++
	return nil
}

func (b *SimpleBudget) ChargeToolCall() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limits.ToolCalls > 0 && b.toolCalls+1 > b.limits.ToolCalls {
		return &BudgetError{Which: "tool_calls"}
	}
	b.toolCalls++
	return nil
}

func (b *SimpleBudget) ChargeUSD(usd float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spentUSD += usd
}

func (b *SimpleBudget) RetriesLeft() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	max := b.MaxRetries
	if max <= 0 {
		max = 10
	}
	return b.retries < max
}

func (b *SimpleBudget) NoteRetry() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.retries++
}

func (b *SimpleBudget) DeadlineOK() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deadlineLocked()
}

func (b *SimpleBudget) deadlineLocked() error {
	if b.limits.WallClock > 0 && b.now().Sub(b.start) > b.limits.WallClock {
		return &BudgetError{Which: "wall_clock"}
	}
	return nil
}
