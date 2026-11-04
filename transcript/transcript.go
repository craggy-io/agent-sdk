// Package transcript is the append-only item list (rule 1), its pure
// normalisation projection (§5.4), and the invariant checker (§13.1).
package transcript

import (
	"sync"

	"github.com/craggy-io/agent-sdk/model"
)

// Transcript is a mutex-guarded append-only list of items. Items are
// immutable once appended; Append assigns Seq from 0 in append order.
type Transcript struct {
	mu    sync.Mutex
	items []model.Item
}

// New returns an empty transcript.
func New() *Transcript { return &Transcript{} }

// NewFrom returns a transcript holding copies of items, with Seq reassigned
// only where items is empty-input; supplied items keep order and are
// re-sequenced from 0 so the append-only invariant holds by construction.
// Callers restoring a recorded transcript should Append instead if they need
// original Seq values preserved verbatim in memory beyond Items().
func NewFrom(items []model.Item) *Transcript {
	t := &Transcript{}
	t.Append(items...)
	return t
}

// Append assigns Seq (overwriting any supplied value) and returns the stored copies.
func (t *Transcript) Append(items ...model.Item) []model.Item {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]model.Item, 0, len(items))
	for _, it := range items {
		c := cloneItem(it)
		c.Seq = len(t.items)
		t.items = append(t.items, c)
		out = append(out, cloneItem(c))
	}
	return out
}

// Items returns a deep copy of the transcript in Seq order.
func (t *Transcript) Items() []model.Item {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]model.Item, len(t.items))
	for i, it := range t.items {
		out[i] = cloneItem(it)
	}
	return out
}

// Len returns the number of items.
func (t *Transcript) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.items)
}

// EstimateTokens is the bytes/4 estimate over text and call args (§5.6).
func EstimateTokens(items []model.Item) int {
	bytes := 0
	for _, it := range items {
		bytes += len(it.Text)
		for _, c := range it.Calls {
			bytes += len(c.Args)
		}
	}
	return bytes / 4
}

// cloneItem deep-copies the mutable parts of an item. Shared by Transcript
// and Normalise so neither ever mutates a stored or caller's item (rule 1).
func cloneItem(it model.Item) model.Item {
	c := it
	if it.Calls != nil {
		c.Calls = make([]model.ToolCall, len(it.Calls))
		for i, call := range it.Calls {
			c.Calls[i] = call
			if call.Args != nil {
				c.Calls[i].Args = append([]byte(nil), call.Args...)
			}
		}
	}
	if it.Reasoning != nil {
		r := *it.Reasoning
		if r.Details != nil {
			r.Details = append([]byte(nil), r.Details...)
		}
		c.Reasoning = &r
	}
	if it.Origin != nil {
		o := *it.Origin
		c.Origin = &o
	}
	if it.Covers != nil {
		c.Covers = append([]model.SeqRange(nil), it.Covers...)
	}
	if it.Supersedes != nil {
		c.Supersedes = append([]int(nil), it.Supersedes...)
	}
	return c
}
