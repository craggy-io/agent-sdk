package loop

import (
	"context"
	"fmt"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/transcript"
)

// Tool-result clearing (§5.6). Once the projection passes
// loop.clear_results_at of the window, old tool results are sent as a
// short marker instead of their text; result_read(call_id=…) returns
// one again. Like summaries, clearing only shapes the request: the
// stored transcript keeps every result (rule 1). It runs well before
// compaction, which is lossy and costs a model call, so compaction
// becomes the fallback rather than the first resort.

// clearMinBytes is the smallest result clearing replaces: below it the
// marker would save next to nothing.
const clearMinBytes = 1024

// clearExempt are built-ins whose results are instructions or tool-list
// state rather than data: clearing a skill body would silently drop
// guidance the model was told to follow.
var clearExempt = map[string]bool{"skill": true, toolsLoadName: true}

// project is the request projection: normalisation (§5.4), then
// tool-result clearing. Every path that sizes or sends a request uses it.
func (r *runner) project(ctx context.Context) ([]model.Item, []string) {
	items := r.thread.Items()
	projected, repairs := transcript.Normalise(items, r.in.NodeID, r.in.Profile.Model)
	return r.clearResults(ctx, items, projected), repairs
}

// clearResults replaces the text of the oldest clearable results in
// projected with a marker, keeping at least loop.keep_recent_results.
// The cleared set is a pure function of the stored transcript, the
// policy and the window, so a thread that outlives a run projects the
// same way in the next one.
//
// The trigger measures the uncleared projection, which only grows
// between compactions, so clearing never switches off and on again as
// its own savings land. The boundary advances keep_recent_results
// results at a time: a provider prompt cache is invalidated from the
// first changed item on, so clearing in batches pays that cost once per
// batch rather than on every step.
func (r *runner) clearResults(ctx context.Context, items, projected []model.Item) []model.Item {
	if r.window <= 0 || r.policy.ClearResultsAt <= 0 {
		return projected
	}
	est := transcript.EstimateTokens(projected) + r.toolTokens() + r.in.Profile.MaxOutputTokens
	if float64(est) <= r.policy.ClearResultsAt*float64(r.window) {
		r.noteCleared(ctx, 0, 0)
		return projected
	}
	candidates := clearable(items)
	n := clearCount(len(candidates), r.policy.KeepRecentResults)
	cleared := make(map[int]model.Item, n)
	bytes := 0
	for _, it := range candidates[:n] {
		cleared[it.Seq] = it
		bytes += len(it.Text)
	}
	r.noteCleared(ctx, n, bytes)
	if n == 0 {
		return projected
	}
	out := make([]model.Item, len(projected))
	for i, it := range projected {
		if orig, ok := cleared[it.Seq]; ok && it.Kind == model.KindToolResult {
			it.Text = clearedMarker(orig.CallID, len(orig.Text))
		}
		out[i] = it
	}
	return out
}

// clearable is the stored tool results clearing may replace, oldest
// first: not pinned, not from an exempt built-in, and long enough that
// the marker saves something.
func clearable(items []model.Item) []model.Item {
	names := map[string]string{}
	var out []model.Item
	for _, it := range items {
		for _, c := range it.Calls {
			names[c.ID] = c.Name
		}
		if it.Kind != model.KindToolResult || it.Pinned || len(it.Text) < clearMinBytes || clearExempt[names[it.CallID]] {
			continue
		}
		out = append(out, it)
	}
	return out
}

// clearCount is how many of n clearable results to clear so that
// between keep and 2·keep−1 of the newest stay intact: the boundary
// moves keep results at a time.
func clearCount(n, keep int) int {
	if keep <= 0 || n <= keep {
		return 0
	}
	return (n - keep) / keep * keep
}

func clearedMarker(callID string, size int) string {
	return fmt.Sprintf("[result cleared from context (%d bytes); result_read(call_id=%q) returns it]", size, callID)
}

// noteCleared records a change in how many results the projection
// clears: it invalidates the prompt-token baseline, as any projection
// change does (§5.6), and emits context.cleared.
func (r *runner) noteCleared(ctx context.Context, results, bytes int) {
	if results == r.clearedResults {
		return
	}
	r.clearedResults = results
	r.lastPromptTokens = 0
	_ = r.emit(ctx, "context.cleared", "", map[string]any{"results": results, "bytes": bytes})
}

// storedResult is the full stored text of the result for callID, for
// result_read(call_id=…).
func (r *runner) storedResult(callID string) (string, bool) {
	for _, it := range r.thread.Items() {
		if it.Kind == model.KindToolResult && it.CallID == callID {
			return it.Text, true
		}
	}
	return "", false
}
