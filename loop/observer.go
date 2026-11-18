package loop

import (
	"context"

	"github.com/craggy-io/agent-sdk/model"
)

// CallObserver observes a requested call and receives its single settlement.
type CallObserver func(context.Context, model.ToolCall) func(model.ToolResult)

func (d *dispatch) observeCalls() {
	if d.r.in.OnLocalCall == nil {
		return
	}
	d.finished = make(map[string]func(model.ToolResult), len(d.assistant.Calls))
	for _, c := range d.assistant.Calls {
		d.finished[c.ID] = d.r.in.OnLocalCall(d.ctx, c)
	}
}

func (d *dispatch) settleRemaining(why settlement) {
	for _, c := range d.assistant.Calls {
		if !d.settled[c.ID] {
			d.commit(c, model.ToolResult{Text: why.text, IsError: true}, why.reason)
		}
	}
}
