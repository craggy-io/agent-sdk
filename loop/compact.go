package loop

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/transcript"
)

const summaryPrompt = `Summarise the following agent transcript excerpt. Preserve: the current task, user constraints and corrections, completed work, outstanding work, artifact references and unresolved failures, with source item references (seq numbers). Be concise and factual. Transcript:

`

// compact runs one compaction (§5.6). It reports whether a Summary was
// appended; a non-empty status is a stop (compaction_failed, budget,
// context_unfit). Compaction never recursively triggers compaction: the
// summariser steps are sampled directly, with no tools.
func (r *runner) compact(ctx context.Context) (bool, string, error) {
	items := r.thread.Items()
	sel, ok := selectForCompaction(items, r.policy.KeepRecentTokens)
	if !ok {
		return false, "", nil
	}
	rendered := renderPlainText(sel.evicted)
	beforeTokens := transcript.EstimateTokens(sel.evicted)

	summary, st, err := r.summarise(ctx, sel.evicted, rendered)
	if err != nil || st != "" {
		return false, st, err
	}
	summary = strings.TrimSpace(summary)
	// Reject empty or non-shrinking results (§5.6).
	if summary == "" {
		_ = r.emit(ctx, "compaction.failed", "", map[string]any{"reason": "empty summary"})
		return false, statusFailed("compaction_failed"), nil
	}
	sumItem := model.Item{Kind: model.KindSummary, Text: summary,
		Covers: sel.covers, Supersedes: sel.supersedes}
	if transcript.EstimateTokens([]model.Item{sumItem}) >= beforeTokens {
		_ = r.emit(ctx, "compaction.failed", "", map[string]any{"reason": "non-shrinking summary"})
		return false, statusFailed("compaction_failed"), nil
	}
	// Validate coverage, ordering and tool pairing before committing.
	if err := validateSummary(items, sumItem, sel.mandatory); err != nil {
		_ = r.emit(ctx, "compaction.failed", "", map[string]any{"reason": err.Error()})
		return false, statusFailed("compaction_failed"), nil
	}
	stored := r.thread.Append(sumItem)[0]
	// A projection change invalidates the prompt-token baseline (§5.6).
	r.lastPromptTokens = 0
	_ = r.emit(ctx, "compaction", "", map[string]any{
		"seq": stored.Seq, "covers": stored.Covers, "supersedes": stored.Supersedes,
		"evicted_tokens": beforeTokens,
	})
	// Reserve output space and check fit again; mandatory items and tool
	// schemas that cannot fit are context_unfit, never a silent drop.
	if r.window > 0 {
		projected, _ := r.project(ctx)
		est := r.estimateTokens(projected)
		if est+r.in.Profile.MaxOutputTokens > r.window && sel.mandatoryAloneUnfit(r, projected) {
			r.emit(ctx, "context.unfit", "", r.unfitCounts(projected, est))
			return true, StatusContextUnfit, nil
		}
	}
	return true, "", nil
}

// summarise renders chunks when the input itself is too large, then merges
// (§5.6). Every request is charged to the same run budget.
func (r *runner) summarise(ctx context.Context, evicted []model.Item, rendered string) (string, string, error) {
	// Chunk when the rendered input would not fit beside the summary output.
	if r.window > 0 {
		chunkBudget := r.window - r.policy.SummaryTokens
		if chunkBudget > 0 && transcript.EstimateTokens(evicted) > chunkBudget {
			chunks := chunkItems(evicted, chunkBudget/2)
			if len(chunks) > 1 {
				var parts []string
				for _, ch := range chunks {
					s, st, err := r.summariseOnce(ctx, renderPlainText(ch))
					if err != nil || st != "" {
						return "", st, err
					}
					parts = append(parts, s)
				}
				return r.summariseOnce(ctx, strings.Join(parts, "\n\n"))
			}
		}
	}
	return r.summariseOnce(ctx, rendered)
}

// summariseOnce is one compaction step: plain text in a single user
// message, no tools, ToolChoice none (§5.6).
func (r *runner) summariseOnce(ctx context.Context, rendered string) (string, string, error) {
	user := model.Item{Seq: 1, Kind: model.KindUser, Text: summaryPrompt + rendered}
	stepID := model.NewID()
	req := model.Request{
		Model:           r.in.Profile.Model,
		Fallbacks:       r.in.Profile.Fallbacks,
		Items:           []model.Item{{Seq: 0, Kind: model.KindSystem, Text: "You summarise agent transcripts."}, user},
		Tools:           nil, // the summariser can never call a tool (§5.6)
		ToolChoice:      "none",
		Reasoning:       r.in.Profile.Reasoning,
		MaxOutputTokens: r.policy.SummaryTokens,
		Provider:        r.in.Profile.Provider,
		SessionID:       r.in.SessionID,
		IdleTimeout:     r.in.Profile.IdleTimeout,
	}
	if err := r.budget.ChargeStep(transcript.EstimateTokens(req.Items), r.inputPrice); err != nil {
		return "", statusBudget(budgetWhich(err, "steps")), nil
	}
	if r.deps.Trace != nil {
		meta := trace.Event{SessionID: r.in.SessionID, RunID: r.in.RunID, NodeID: r.in.NodeID, StepID: stepID}
		if err := r.deps.Trace.RecordStepRequest(ctx, meta, req.Items, trace.ParamsOf(req), nil); err != nil {
			return "", "", fmt.Errorf("loop: commit compaction step.request: %w", err)
		}
		_ = r.emit(ctx, "compaction.step", stepID, nil)
	}
	resp, err := safeSample(ctx, r.deps.Provider, req, nil)
	if err != nil {
		data := attemptFailedData(err, 0, req.Model)
		data["compaction"] = true
		_ = r.emit(ctx, "step.attempt_failed", stepID, data)
		r.chargeFailedAttempt(err)
		if ctx.Err() != nil {
			return "", StatusCancelled, nil
		}
		return "", statusFailed("compaction_failed"), nil
	}
	r.budget.ChargeUSD(resp.Usage.CostUSD)
	r.usage.Add(resp.Usage)
	if r.deps.Trace != nil {
		h, herr := r.deps.Trace.PutItem(ctx, resp.Item)
		if herr != nil {
			return "", "", fmt.Errorf("loop: commit compaction step.response item: %w", herr)
		}
		if err := r.fatalEmit(ctx, "step.response", stepID, map[string]any{
			"item": h, "served_model": resp.ServedModel, "provider": resp.Provider, "usage": resp.Usage, "compaction": true}); err != nil {
			return "", "", r.getTraceErr()
		}
	}
	if resp.Item.Finish == model.FinishError || resp.Item.Finish == model.FinishContentFilter {
		return "", statusFailed("compaction_failed"), nil
	}
	return resp.Item.Text, "", nil
}

type selection struct {
	evicted    []model.Item // items rendered into the summariser input
	covers     []model.SeqRange
	supersedes []int
	mandatory  map[int]bool // seqs that must never be covered
}

// mandatoryAloneUnfit reports whether the mandatory projection alone
// (system, newest user, pins, active summaries, tools) cannot fit.
func (s *selection) mandatoryAloneUnfit(r *runner, projected []model.Item) bool {
	var mandatory []model.Item
	for _, it := range projected {
		if s.mandatory[it.Seq] || it.Kind == model.KindSummary {
			mandatory = append(mandatory, it)
		}
	}
	est := r.estimateTokens(mandatory)
	return est+r.in.Profile.MaxOutputTokens > r.window
}

// selectForCompaction picks what to evict (§5.6): keep the system item, the
// newest user instruction and pinned items; retain recent complete
// exchanges within keepRecentTokens; never split a call from its results.
// Active summaries in the evicted region are consolidated iteratively:
// their coverage is unioned and they are superseded.
func selectForCompaction(items []model.Item, keepRecentTokens int) (*selection, bool) {
	if len(items) == 0 {
		return nil, false
	}
	mandatory := map[int]bool{}
	newestUser := -1
	for i, it := range items {
		if it.Kind == model.KindSystem || it.Pinned {
			mandatory[it.Seq] = true
		}
		if it.Kind == model.KindUser {
			newestUser = i
		}
	}
	if newestUser >= 0 {
		mandatory[items[newestUser].Seq] = true
	}

	// Retain a recent tail of complete exchanges within the token budget.
	keepFrom := len(items)
	acc := 0
	for i := len(items) - 1; i >= 0; i-- {
		n := transcript.EstimateTokens(items[i : i+1])
		if keepFrom < len(items) && acc+n > keepRecentTokens {
			break
		}
		acc += n
		keepFrom = i
	}
	// Never split an assistant call from its results: pull the boundary
	// back to the assistant that owns a leading run of results.
	for keepFrom > 0 && keepFrom < len(items) && items[keepFrom].Kind == model.KindToolResult {
		keepFrom--
	}
	// If the boundary lands between an assistant and its results the loop
	// above already handled it (results lead the tail only if the
	// assistant is outside it); also pull forward: if the last retained
	// item before the boundary is an assistant whose results start the
	// tail, we pulled back already. Nothing more to do.

	supersededSeqs := map[int]bool{}
	for _, it := range items {
		if it.Kind == model.KindSummary {
			for _, s := range it.Supersedes {
				supersededSeqs[s] = true
			}
		}
	}
	coveredByActive := map[int]bool{}
	var activeSummaries []model.Item
	for _, it := range items {
		if it.Kind == model.KindSummary && !supersededSeqs[it.Seq] {
			activeSummaries = append(activeSummaries, it)
			for _, rg := range it.Covers {
				for s := rg.First; s <= rg.Last; s++ {
					coveredByActive[s] = true
				}
			}
		}
	}

	covered := map[int]bool{}
	var evicted []model.Item
	var supersedes []int
	evictedSeqs := map[int]bool{}
	for i := 0; i < keepFrom; i++ {
		it := items[i]
		if mandatory[it.Seq] {
			continue
		}
		if it.Kind == model.KindSummary {
			if supersededSeqs[it.Seq] {
				continue // already superseded; nothing to render or re-cover
			}
			// Consolidate this active summary iteratively (§5.6).
			evicted = append(evicted, it)
			supersedes = append(supersedes, it.Seq)
			for _, rg := range it.Covers {
				for s := rg.First; s <= rg.Last; s++ {
					covered[s] = true
				}
			}
			continue
		}
		covered[it.Seq] = true
		if !coveredByActive[it.Seq] {
			// Originals already covered by an active (non-evicted) summary
			// are represented by that summary, not re-rendered; originals
			// covered by an evicted summary are represented by its text.
			evicted = append(evicted, it)
			evictedSeqs[it.Seq] = true
		}
	}
	_ = evictedSeqs
	if len(covered) == 0 {
		return nil, false
	}
	// Pair integrity at the boundary: a covered assistant needs its results
	// covered too (the keepFrom adjustment guarantees the converse).
	for i, it := range items {
		if it.Kind != model.KindAssistant || !covered[it.Seq] {
			continue
		}
		for j := i + 1; j < len(items) && items[j].Kind == model.KindToolResult; j++ {
			if !covered[items[j].Seq] {
				covered[items[j].Seq] = true
				if j >= keepFrom {
					// The whole exchange is evicted; retain less tail.
				}
			}
		}
	}
	return &selection{
		evicted: evicted, covers: seqsToRanges(covered),
		supersedes: supersedes, mandatory: mandatory,
	}, true
}

func seqsToRanges(set map[int]bool) []model.SeqRange {
	seqs := make([]int, 0, len(set))
	for s := range set {
		seqs = append(seqs, s)
	}
	sort.Ints(seqs)
	var ranges []model.SeqRange
	for _, s := range seqs {
		if n := len(ranges); n > 0 && s == ranges[n-1].Last+1 {
			ranges[n-1].Last = s
			continue
		}
		ranges = append(ranges, model.SeqRange{First: s, Last: s})
	}
	return ranges
}

// renderPlainText renders items as plain text for the summariser (§5.6):
// no tool protocol survives this rendering.
func renderPlainText(items []model.Item) string {
	var b strings.Builder
	for _, it := range items {
		switch it.Kind {
		case model.KindSystem:
		case model.KindUser:
			fmt.Fprintf(&b, "user (seq %d): %s\n", it.Seq, it.Text)
		case model.KindAssistant:
			if it.Text != "" {
				fmt.Fprintf(&b, "assistant (seq %d): %s\n", it.Seq, it.Text)
			}
			for _, c := range it.Calls {
				fmt.Fprintf(&b, "assistant (seq %d) called %s with %s\n", it.Seq, c.Name, string(nonempty(c.Args)))
			}
		case model.KindToolResult:
			fmt.Fprintf(&b, "result (seq %d, call %s): %s\n", it.Seq, it.CallID, it.Text)
		case model.KindNotice:
			fmt.Fprintf(&b, "notice (seq %d): %s\n", it.Seq, it.Text)
		case model.KindSummary:
			fmt.Fprintf(&b, "previous summary (seq %d): %s\n", it.Seq, it.Text)
		}
	}
	return b.String()
}

func chunkItems(items []model.Item, tokenBudget int) [][]model.Item {
	if tokenBudget <= 0 {
		tokenBudget = 1
	}
	var chunks [][]model.Item
	var cur []model.Item
	curTokens := 0
	for _, it := range items {
		n := transcript.EstimateTokens([]model.Item{it})
		if len(cur) > 0 && curTokens+n > tokenBudget {
			chunks = append(chunks, cur)
			cur, curTokens = nil, 0
		}
		cur = append(cur, it)
		curTokens += n
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// validateSummary checks a proposed summary against the raw transcript:
// sorted disjoint coverage, no mandatory item covered, no split calls.
func validateSummary(items []model.Item, sum model.Item, mandatory map[int]bool) error {
	prev := -2
	covered := map[int]bool{}
	for _, rg := range sum.Covers {
		if rg.First < 0 || rg.Last < rg.First {
			return fmt.Errorf("invalid cover range %+v", rg)
		}
		if rg.First <= prev {
			return fmt.Errorf("covers not sorted/disjoint at %+v", rg)
		}
		prev = rg.Last
		for s := rg.First; s <= rg.Last; s++ {
			if mandatory[s] {
				return fmt.Errorf("summary covers mandatory seq %d", s)
			}
			covered[s] = true
		}
	}
	for i, it := range items {
		if it.Kind != model.KindAssistant {
			continue
		}
		for j := i + 1; j < len(items) && items[j].Kind == model.KindToolResult; j++ {
			if covered[it.Seq] != covered[items[j].Seq] {
				return fmt.Errorf("summary splits call/result pair at seq %d/%d", it.Seq, items[j].Seq)
			}
		}
	}
	return nil
}
