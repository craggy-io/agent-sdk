package transcript

import (
	"fmt"

	"github.com/craggy-io/agent-sdk/model"
)

// Check verifies the §13.1 invariants on items (a raw transcript or a
// normalised projection):
//   - append-only Seq order: conversational Seq values strictly increase.
//     Summary and ToolResult items are exempt: a projection places a summary
//     at its earliest covered position (§5.6) and results by call pairing in
//     call order (§5.4.1), while Seq keeps each item's raw identity; a
//     synthetic ToolResult from Normalise may share its assistant's Seq.
//   - no System item after index 0;
//   - every assistant call has exactly one result, contiguous directly after
//     the assistant item, in call order; no orphan results;
//   - call IDs unique, 1..40 chars of [A-Za-z0-9_-] (§5.4 rule 3);
//   - reasoning only on Assistant items with a non-empty ServedBy (bound to
//     the served model; Normalise additionally enforces ServedBy==target);
//   - Summary Covers sorted and disjoint (§5.1), kinds valid.
func Check(items []model.Item) error {
	seenIDs := map[string]int{}
	lastSeq := -1
	for i, it := range items {
		switch it.Kind {
		case model.KindSystem, model.KindUser, model.KindAssistant,
			model.KindToolResult, model.KindNotice, model.KindSummary:
		default:
			return fmt.Errorf("item %d (seq %d): invalid kind %q", i, it.Seq, it.Kind)
		}
		if it.Seq < 0 {
			return fmt.Errorf("item %d: negative seq %d", i, it.Seq)
		}
		if it.Kind == model.KindSystem && i != 0 {
			return fmt.Errorf("item %d (seq %d): system item after index 0", i, it.Seq)
		}
		if it.Kind != model.KindSummary && it.Kind != model.KindToolResult {
			if it.Seq < lastSeq {
				return fmt.Errorf("item %d: seq %d out of append-only order (after %d)", i, it.Seq, lastSeq)
			}
			if it.Seq == lastSeq {
				return fmt.Errorf("item %d: duplicate seq %d", i, it.Seq)
			}
			lastSeq = it.Seq
		}
		if it.Reasoning != nil {
			if it.Kind != model.KindAssistant {
				return fmt.Errorf("seq %d: reasoning on non-assistant item", it.Seq)
			}
			if it.ServedBy == "" {
				return fmt.Errorf("seq %d: reasoning not bound to a served model", it.Seq)
			}
		}
		if it.Kind == model.KindSummary {
			prevLast := -2 // allow First==0
			for _, r := range it.Covers {
				if r.First < 0 || r.Last < r.First {
					return fmt.Errorf("summary seq %d: invalid cover range %+v", it.Seq, r)
				}
				if r.First <= prevLast {
					return fmt.Errorf("summary seq %d: covers not sorted/disjoint at %+v", it.Seq, r)
				}
				prevLast = r.Last
			}
		}
		for _, c := range it.Calls {
			if it.Kind != model.KindAssistant {
				return fmt.Errorf("seq %d: calls on non-assistant item", it.Seq)
			}
			if !validCallID(c.ID) {
				return fmt.Errorf("seq %d: call ID %q not 1..40 chars of [A-Za-z0-9_-]", it.Seq, c.ID)
			}
			if prev, dup := seenIDs[c.ID]; dup {
				return fmt.Errorf("call ID %q duplicated (seq %d and %d)", c.ID, prev, it.Seq)
			}
			seenIDs[c.ID] = it.Seq
		}
	}
	// Pairing and contiguity (§5.4 rule 1).
	for i := 0; i < len(items); i++ {
		it := items[i]
		if it.Kind == model.KindToolResult {
			return fmt.Errorf("seq %d: orphan tool result for call %q", it.Seq, it.CallID)
		}
		if it.Kind != model.KindAssistant {
			continue
		}
		for j, c := range it.Calls {
			k := i + 1 + j
			if k >= len(items) {
				return fmt.Errorf("seq %d: call %q has no result", it.Seq, c.ID)
			}
			r := items[k]
			if r.Kind != model.KindToolResult {
				return fmt.Errorf("seq %d: call %q result not contiguous after assistant (found %s seq %d)", it.Seq, c.ID, r.Kind, r.Seq)
			}
			if r.CallID != c.ID {
				return fmt.Errorf("seq %d: result %d is for call %q, want %q (call order)", it.Seq, j, r.CallID, c.ID)
			}
		}
		i += len(it.Calls)
	}
	return nil
}
