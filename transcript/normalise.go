package transcript

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/craggy-io/agent-sdk/model"
)

// ReplacementID is the deterministic call ID of §5.4 rule 3: "c_" plus
// lowercase base32 (no padding) of sha256(nodeID|seq|index), truncated so the
// total length is at most 40 characters from [A-Za-z0-9_-].
func ReplacementID(nodeID string, seq, index int) string {
	return replacementID(nodeID, seq, index, 0)
}

func replacementID(nodeID string, seq, index, salt int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d", nodeID, seq, index, salt)))
	// Salt 0 hashes "node|seq|index|0"; keep the common case exact per spec
	// by special-casing it below.
	if salt == 0 {
		h = sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", nodeID, seq, index)))
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	id := "c_" + strings.ToLower(enc.EncodeToString(h[:]))
	if len(id) > 40 {
		id = id[:40]
	}
	return id
}

// validCallID implements §5.4 rule 3's shape: 1..40 chars of [A-Za-z0-9_-].
func validCallID(id string) bool {
	if id == "" || len(id) > 40 {
		return false
	}
	for _, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// stripReasoningIndex removes the stream-only "index" field from
// reasoning_details (§5.4 rule 4). Bytes without an index are returned
// unchanged (byte-for-byte); unparseable details are returned unchanged.
func stripReasoningIndex(details json.RawMessage) json.RawMessage {
	if len(details) == 0 {
		return details
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(details, &arr); err == nil {
		changed := false
		for _, m := range arr {
			if _, ok := m["index"]; ok {
				delete(m, "index")
				changed = true
			}
		}
		if !changed {
			return details
		}
		out, err := json.Marshal(arr)
		if err != nil {
			return details
		}
		return out
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(details, &obj); err == nil {
		if _, ok := obj["index"]; !ok {
			return details
		}
		delete(obj, "index")
		out, err := json.Marshal(obj)
		if err != nil {
			return details
		}
		return out
	}
	return details
}

// normaliseCovers sorts ranges and merges overlaps (§5.1: sorted, disjoint).
func normaliseCovers(in []model.SeqRange) []model.SeqRange {
	if len(in) == 0 {
		return in
	}
	rs := append([]model.SeqRange(nil), in...)
	for i := range rs {
		if rs[i].Last < rs[i].First {
			rs[i].First, rs[i].Last = rs[i].Last, rs[i].First
		}
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].First != rs[j].First {
			return rs[i].First < rs[j].First
		}
		return rs[i].Last < rs[j].Last
	})
	out := rs[:1]
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.First <= last.Last+1 {
			if r.Last > last.Last {
				last.Last = r.Last
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

func coversSeq(covers []model.SeqRange, seq int) bool {
	for _, r := range covers {
		if seq >= r.First && seq <= r.Last {
			return true
		}
	}
	return false
}

// Normalise is the pure projection of §5.4. It never mutates its input and is
// idempotent: Normalise(Normalise(x)) == Normalise(x), including repair-free
// second passes for ID replacement (replaced IDs are valid and kept).
//
// Rules: (1) every assistant call gets exactly one result directly after its
// assistant item, synthesising Settled=not_dispatched results for missing
// ones; (2) orphan/duplicate results are dropped; (3) missing, over-long,
// illegal or duplicated call IDs are replaced deterministically (see
// ReplacementID) and results are remapped; (4) reasoning is kept only when
// ServedBy == targetModel (and targetModel != ""), with "index" stripped;
// (5) active summaries are placed at the earliest covered position, covered
// originals and superseded summaries are suppressed; (6) notices and adjacent
// user items are left as they are.
// Synthetic results carry the assistant item's Seq (they have no transcript
// Seq of their own; Check accounts for this).
func Normalise(items []model.Item, nodeID, targetModel string) (out []model.Item, repairs []string) {
	// Deep-copy and normalise summary metadata first.
	work := make([]model.Item, len(items))
	for i, it := range items {
		c := cloneItem(it)
		if c.Kind == model.KindSummary {
			nc := normaliseCovers(c.Covers)
			if fmt.Sprint(nc) != fmt.Sprint(it.Covers) {
				repairs = append(repairs, fmt.Sprintf("summary seq %d: covers sorted/merged", c.Seq))
			}
			c.Covers = nc
			if len(c.Supersedes) > 0 {
				sorted := append([]int(nil), c.Supersedes...)
				sort.Ints(sorted)
				u := sorted[:0]
				for _, s := range sorted {
					if len(u) == 0 || s != u[len(u)-1] {
						u = append(u, s)
					}
				}
				c.Supersedes = u
			}
		}
		work[i] = c
	}

	// Rule 5: summary projection.
	superseded := map[int]bool{}
	var active []model.Item
	activeSeq := map[int]bool{}
	for _, it := range work {
		if it.Kind == model.KindSummary {
			for _, s := range it.Supersedes {
				superseded[s] = true
			}
		}
	}
	for _, it := range work {
		if it.Kind == model.KindSummary && !superseded[it.Seq] {
			active = append(active, it)
			activeSeq[it.Seq] = true
		}
	}
	coveredByOther := func(seq int) bool {
		for _, s := range active {
			if s.Seq != seq && coversSeq(s.Covers, seq) {
				return true
			}
		}
		return false
	}
	isCovered := func(seq int) bool {
		for _, s := range active {
			if coversSeq(s.Covers, seq) {
				return true
			}
		}
		return false
	}
	// Summaries emitted at the earliest covered position present in work.
	emitAt := map[int][]model.Item{} // index in work -> summaries to emit before it
	emittedElsewhere := map[int]bool{}
	for _, s := range active {
		if coveredByOther(s.Seq) {
			emittedElsewhere[s.Seq] = true // suppressed by another active summary
			continue
		}
		placed := false
		for i, it := range work {
			if it.Seq == s.Seq {
				continue
			}
			if coversSeq(s.Covers, it.Seq) {
				emitAt[i] = append(emitAt[i], s)
				emittedElsewhere[s.Seq] = true
				placed = true
				break
			}
		}
		_ = placed // unplaced summaries emit at their natural position
	}
	var proj []model.Item
	for i, it := range work {
		for _, s := range emitAt[i] {
			proj = append(proj, s)
		}
		if it.Kind == model.KindSummary {
			if superseded[it.Seq] || emittedElsewhere[it.Seq] {
				continue
			}
			if activeSeq[it.Seq] {
				proj = append(proj, it) // natural position (no covered item present)
			}
			continue
		}
		if isCovered(it.Seq) {
			continue
		}
		proj = append(proj, it)
	}

	// Rules 3 and 4: call IDs and reasoning binding. Remember each call's
	// original ID so results recorded under the old ID can be remapped.
	type callKey struct{ projIdx, callIdx int }
	oldIDs := map[callKey]string{}
	seenIDs := map[string]bool{}
	for i := range proj {
		it := &proj[i]
		if it.Kind != model.KindAssistant {
			if it.Reasoning != nil {
				repairs = append(repairs, fmt.Sprintf("seq %d: reasoning dropped from non-assistant item", it.Seq))
				it.Reasoning = nil
			}
			continue
		}
		if it.Reasoning != nil {
			if targetModel == "" || it.ServedBy != targetModel {
				repairs = append(repairs, fmt.Sprintf("seq %d: reasoning dropped (served by %q, target %q)", it.Seq, it.ServedBy, targetModel))
				it.Reasoning = nil
			} else {
				it.Reasoning.Details = stripReasoningIndex(it.Reasoning.Details)
			}
		}
		for j := range it.Calls {
			old := it.Calls[j].ID
			oldIDs[callKey{i, j}] = old
			if validCallID(old) && !seenIDs[old] {
				seenIDs[old] = true
				continue
			}
			reason := "invalid"
			switch {
			case old == "":
				reason = "missing"
			case seenIDs[old]:
				reason = "duplicate"
			case len(old) > 40:
				reason = "too long"
			}
			nid := ReplacementID(nodeID, it.Seq, j)
			for salt := 1; seenIDs[nid]; salt++ {
				nid = replacementID(nodeID, it.Seq, j, salt)
			}
			seenIDs[nid] = true
			repairs = append(repairs, fmt.Sprintf("seq %d call %d: %s call ID %q replaced with %q", it.Seq, j, reason, old, nid))
			it.Calls[j].ID = nid
		}
	}

	// Rules 1 and 2: pair results contiguously after their assistant item, in
	// call order; synthesise missing ones; drop orphans and duplicates.
	var results []model.Item
	var resultProjIdx []int // proj index per result; a result can only settle an earlier call
	var nonResults []model.Item
	// Map from proj index to position in nonResults for assistants.
	assistantNonResultIdx := map[int]int{}
	for i, it := range proj {
		if it.Kind == model.KindToolResult {
			results = append(results, it)
			resultProjIdx = append(resultProjIdx, i)
			continue
		}
		if it.Kind == model.KindAssistant {
			assistantNonResultIdx[i] = len(nonResults)
		}
		nonResults = append(nonResults, it)
	}
	used := make([]bool, len(results))
	// Results to emit after each position in nonResults.
	after := map[int][]model.Item{}
	for i, it := range proj {
		if it.Kind != model.KindAssistant {
			continue
		}
		nr := assistantNonResultIdx[i]
		// assistant in nonResults carries the (possibly replaced) IDs.
		asst := nonResults[nr]
		for j, call := range asst.Calls {
			old := oldIDs[callKey{i, j}]
			found := -1
			for k, r := range results {
				if !used[k] && resultProjIdx[k] > i && r.CallID == call.ID {
					found = k
					break
				}
			}
			if found < 0 && old != "" && old != call.ID {
				for k, r := range results {
					if !used[k] && resultProjIdx[k] > i && r.CallID == old {
						found = k
						break
					}
				}
			}
			if found >= 0 {
				used[found] = true
				r := cloneItem(results[found])
				r.CallID = call.ID
				if r.Reasoning != nil {
					r.Reasoning = nil
				}
				after[nr] = append(after[nr], r)
				continue
			}
			repairs = append(repairs, fmt.Sprintf("seq %d call %q: missing result synthesised (not_dispatched)", it.Seq, call.ID))
			after[nr] = append(after[nr], model.Item{
				Seq: it.Seq, Kind: model.KindToolResult, CallID: call.ID,
				IsError: true, Settled: model.SettleNotDispatched,
				Text: model.Fail(model.CodeCancelled, "not_dispatched", "aborted before dispatch").Text,
			})
		}
	}
	for k, r := range results {
		if !used[k] {
			repairs = append(repairs, fmt.Sprintf("seq %d: orphan or duplicate result for call %q dropped", r.Seq, r.CallID))
		}
	}
	out = make([]model.Item, 0, len(nonResults)+len(results))
	for i, it := range nonResults {
		out = append(out, it)
		out = append(out, after[i]...)
	}
	return out, repairs
}
