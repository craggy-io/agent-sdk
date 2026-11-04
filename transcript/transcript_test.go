package transcript_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/transcript"
)

func sys(seq int) model.Item { return model.Item{Seq: seq, Kind: model.KindSystem, Text: "sys"} }
func usr(seq int, text string) model.Item {
	return model.Item{Seq: seq, Kind: model.KindUser, Text: text}
}
func asst(seq int, served string, calls ...model.ToolCall) model.Item {
	return model.Item{Seq: seq, Kind: model.KindAssistant, ServedBy: served, Finish: model.FinishToolCalls, Calls: calls}
}
func call(id, name string) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Args: json.RawMessage(`{}`), ArgsOK: true}
}
func result(seq int, callID string) model.Item {
	return model.Item{Seq: seq, Kind: model.KindToolResult, CallID: callID, Text: "ok", Settled: model.SettleRan}
}

func norm(t *testing.T, in []model.Item) ([]model.Item, []string) {
	t.Helper()
	out, repairs := transcript.Normalise(in, "node-1", "m-target")
	if err := transcript.Check(out); err != nil {
		t.Fatalf("Normalise output fails Check: %v\nout=%+v", err, out)
	}
	// §5.4 rule 3: normalising twice gives the same output.
	out2, _ := transcript.Normalise(out, "node-1", "m-target")
	if !reflect.DeepEqual(out, out2) {
		t.Fatalf("Normalise not idempotent:\nfirst=%+v\nsecond=%+v", out, out2)
	}
	return out, repairs
}

// Rule 1 (append-only): Transcript assigns Seq from 0 and copies on readout.
func TestTranscriptAppendAndCopy(t *testing.T) {
	tr := transcript.New()
	got := tr.Append(model.Item{Seq: 99, Kind: model.KindUser, Text: "a"}, usr(7, "b"))
	if got[0].Seq != 0 || got[1].Seq != 1 || tr.Len() != 2 {
		t.Fatalf("seq assignment: %+v len=%d", got, tr.Len())
	}
	items := tr.Items()
	items[0].Text = "mutated"
	if tr.Items()[0].Text != "a" {
		t.Fatal("Items() must return a copy")
	}
}

func TestEstimateTokens(t *testing.T) {
	// bytes/4 over text+args (§5.6): 8 text bytes + 8 arg bytes = 4.
	items := []model.Item{
		{Kind: model.KindUser, Text: "12345678"},
		asst(1, "m", model.ToolCall{ID: "c1", Name: "x", Args: json.RawMessage(`{"a":12}`)}),
	}
	if got := transcript.EstimateTokens(items); got != 4 {
		t.Fatalf("EstimateTokens = %d, want 4", got)
	}
}

// §5.4.1: a missing result is synthesised with Settled=not_dispatched + repair.
func TestNormaliseMissingResultSynthesised(t *testing.T) {
	out, repairs := norm(t, []model.Item{
		sys(0), usr(1, "hi"),
		asst(2, "m-target", call("c1", "x")),
		usr(3, "later"),
	})
	if len(out) != 5 || out[3].Kind != model.KindToolResult || out[3].CallID != "c1" ||
		out[3].Settled != model.SettleNotDispatched || !out[3].IsError {
		t.Fatalf("synthetic result wrong: %+v", out)
	}
	if len(repairs) == 0 || !strings.Contains(repairs[0], "missing result") {
		t.Fatalf("expected repair, got %v", repairs)
	}
}

// §5.4.2: orphan and duplicate results are dropped with repairs.
func TestNormaliseOrphanAndDuplicateResultsDropped(t *testing.T) {
	out, repairs := norm(t, []model.Item{
		sys(0),
		asst(1, "m-target", call("c1", "x")),
		result(2, "c1"), result(3, "c1"), result(4, "ghost"),
		usr(5, "next"),
	})
	if len(out) != 4 {
		t.Fatalf("len=%d out=%+v", len(out), out)
	}
	joined := strings.Join(repairs, "\n")
	if !strings.Contains(joined, "orphan or duplicate") {
		t.Fatalf("expected orphan repairs, got %v", repairs)
	}
}

// §5.4.1: results are re-placed contiguously after their assistant, in call order.
func TestNormaliseResultsContiguousInCallOrder(t *testing.T) {
	out, _ := norm(t, []model.Item{
		sys(0),
		asst(1, "m-target", call("c1", "x"), call("c2", "y")),
		result(2, "c2"),
		{Seq: 3, Kind: model.KindNotice, Text: "between"},
		result(4, "c1"),
	})
	if out[2].CallID != "c1" || out[3].CallID != "c2" || out[4].Kind != model.KindNotice {
		t.Fatalf("contiguity/order wrong: %+v", out)
	}
}

// §5.4.3: missing, duplicate, over-long and illegal IDs get deterministic replacements.
func TestNormaliseCallIDReplacement(t *testing.T) {
	long := strings.Repeat("a", 41)
	in := []model.Item{
		sys(0),
		asst(1, "m-target", call("", "a"), call("dup", "b")),
		result(2, ""), result(3, "dup"),
		asst(4, "m-target", call("dup", "c"), call(long, "d"), call("bad id!", "e")),
		result(5, "dup"), result(6, long), result(7, "bad id!"),
	}
	out, repairs := norm(t, in)

	first := out[1].Calls
	if first[0].ID != transcript.ReplacementID("node-1", 1, 0) {
		t.Fatalf("missing ID replacement = %q", first[0].ID)
	}
	if first[1].ID != "dup" {
		t.Fatalf("first valid dup should be kept, got %q", first[1].ID)
	}
	second := out[4].Calls
	if second[0].ID != transcript.ReplacementID("node-1", 4, 0) ||
		second[1].ID != transcript.ReplacementID("node-1", 4, 1) ||
		second[2].ID != transcript.ReplacementID("node-1", 4, 2) {
		t.Fatalf("replacements wrong: %+v", second)
	}
	// Results recorded under old IDs are remapped to the replacements, in order.
	if out[2].CallID != first[0].ID || out[5].CallID != second[0].ID || out[6].CallID != second[1].ID {
		t.Fatalf("result remap wrong: %+v", out)
	}
	for _, c := range append(append([]model.ToolCall{}, first...), second...) {
		if len(c.ID) > 40 || !strings.HasPrefix(c.ID, "c_") && c.ID != "dup" {
			t.Fatalf("bad replacement ID %q", c.ID)
		}
	}
	joined := strings.Join(repairs, "\n")
	for _, want := range []string{"missing call ID", "duplicate call ID", "too long call ID"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("repairs missing %q: %v", want, repairs)
		}
	}
	// Determinism across independent runs (§5.4.3).
	outB, _ := transcript.Normalise(in, "node-1", "m-target")
	if !reflect.DeepEqual(out, outB) {
		t.Fatal("Normalise not deterministic")
	}
	// Input is never mutated (rule 1 / §5.4).
	if in[1].Calls[0].ID != "" || in[4].Calls[1].ID != long {
		t.Fatalf("Normalise mutated its input: %+v", in)
	}
}

// §5.4.4: reasoning goes back only to the served model, minus the index field.
func TestNormaliseReasoningBinding(t *testing.T) {
	details := json.RawMessage(`[{"index":0,"type":"reasoning.text","text":"hmm"}]`)
	mk := func(served string) model.Item {
		it := asst(1, served)
		it.Reasoning = &model.Reasoning{Details: details, Text: "hmm"}
		it.Finish = model.FinishStop
		return it
	}
	// Same model: kept, index stripped.
	out, _ := norm(t, []model.Item{sys(0), mk("m-target")})
	if out[1].Reasoning == nil {
		t.Fatal("reasoning for target model must be kept")
	}
	if strings.Contains(string(out[1].Reasoning.Details), "index") {
		t.Fatalf("index not stripped: %s", out[1].Reasoning.Details)
	}
	if !strings.Contains(string(out[1].Reasoning.Details), "hmm") {
		t.Fatalf("details mangled: %s", out[1].Reasoning.Details)
	}
	// Other model: dropped, never translated.
	out, repairs := norm(t, []model.Item{sys(0), mk("other-model")})
	if out[1].Reasoning != nil {
		t.Fatal("reasoning from another model must be dropped")
	}
	if len(repairs) == 0 || !strings.Contains(repairs[0], "reasoning dropped") {
		t.Fatalf("expected repair, got %v", repairs)
	}
	// No index present: bytes preserved exactly.
	plain := json.RawMessage(`[{"type":"reasoning.text","text":"x"}]`)
	it := mk("m-target")
	it.Reasoning.Details = plain
	out, _ = norm(t, []model.Item{sys(0), it})
	if string(out[1].Reasoning.Details) != string(plain) {
		t.Fatalf("details without index must be byte-exact, got %s", out[1].Reasoning.Details)
	}
}

// §5.4.5 + §5.6: an active summary sits at the earliest covered position,
// covered originals and superseded summaries are suppressed.
func TestNormaliseSummaryProjection(t *testing.T) {
	in := []model.Item{
		sys(0),
		usr(1, "first task"),
		asst(2, "m-target", call("c1", "x")),
		result(3, "c1"),
		usr(4, "kept"),
		{Seq: 5, Kind: model.KindSummary, Text: "old summary", Covers: []model.SeqRange{{First: 1, Last: 1}}},
		{Seq: 6, Kind: model.KindSummary, Text: "active summary",
			Covers: []model.SeqRange{{First: 1, Last: 3}}, Supersedes: []int{5}},
		usr(7, "latest"),
	}
	out, _ := norm(t, in)
	wantKinds := []model.ItemKind{model.KindSystem, model.KindSummary, model.KindUser, model.KindUser}
	if len(out) != len(wantKinds) {
		t.Fatalf("len=%d out=%+v", len(out), out)
	}
	for i, k := range wantKinds {
		if out[i].Kind != k {
			t.Fatalf("out[%d].Kind=%s want %s (out=%+v)", i, out[i].Kind, k, out)
		}
	}
	if out[1].Text != "active summary" || out[2].Text != "kept" || out[3].Text != "latest" {
		t.Fatalf("projection order wrong: %+v", out)
	}
}

// §5.4.6: adjacent user/notice items are left as they are (no merging).
func TestNormaliseAdjacentUsersUntouched(t *testing.T) {
	in := []model.Item{sys(0), usr(1, "a"), usr(2, "b"), {Seq: 3, Kind: model.KindNotice, Text: "n"}}
	out, repairs := norm(t, in)
	if len(out) != 4 || out[1].Text != "a" || out[2].Text != "b" {
		t.Fatalf("adjacent users altered: %+v", out)
	}
	if len(repairs) != 0 {
		t.Fatalf("unexpected repairs: %v", repairs)
	}
}

// §13.1: Check catches each violated invariant.
func TestCheckCatchesViolations(t *testing.T) {
	good := []model.Item{
		sys(0), usr(1, "x"),
		asst(2, "m", call("c1", "x")), result(3, "c1"),
	}
	if err := transcript.Check(good); err != nil {
		t.Fatalf("valid transcript rejected: %v", err)
	}
	cases := []struct {
		name  string
		items []model.Item
		want  string
	}{
		{"system after 0", []model.Item{usr(0, "x"), sys(1)}, "system item after index 0"},
		{"missing result", []model.Item{asst(0, "m", call("c1", "x"))}, "has no result"},
		{"non-contiguous result", []model.Item{
			asst(0, "m", call("c1", "x")), usr(1, "gap"), result(2, "c1")}, "not contiguous"},
		{"result order", []model.Item{
			asst(0, "m", call("c1", "x"), call("c2", "y")), result(1, "c2"), result(2, "c1")}, "call order"},
		{"orphan result", []model.Item{usr(0, "x"), result(1, "c9")}, "orphan"},
		{"duplicate call ID", []model.Item{
			asst(0, "m", call("c1", "x")), result(1, "c1"),
			asst(2, "m", call("c1", "y")), result(3, "c1")}, "duplicated"},
		{"illegal call ID", []model.Item{
			asst(0, "m", call("bad id", "x")), result(1, "bad id")}, "not 1..40"},
		{"seq out of order", []model.Item{usr(5, "x"), usr(2, "y")}, "append-only order"},
		{"duplicate seq", []model.Item{usr(1, "x"), usr(1, "y")}, "duplicate seq"},
		{"invalid kind", []model.Item{{Seq: 0, Kind: "bogus"}}, "invalid kind"},
		{"reasoning unbound", []model.Item{
			{Seq: 0, Kind: model.KindAssistant, Reasoning: &model.Reasoning{Text: "r"}}}, "not bound"},
		{"reasoning on user", []model.Item{
			{Seq: 0, Kind: model.KindUser, Reasoning: &model.Reasoning{Text: "r"}}}, "non-assistant"},
		{"covers overlapping", []model.Item{
			{Seq: 0, Kind: model.KindSummary, Covers: []model.SeqRange{{First: 0, Last: 2}, {First: 1, Last: 3}}}}, "sorted/disjoint"},
		{"calls on non-assistant", []model.Item{
			{Seq: 0, Kind: model.KindUser, Calls: []model.ToolCall{call("c1", "x")}}}, "non-assistant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := transcript.Check(tc.items)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check = %v, want error containing %q", err, tc.want)
			}
		})
	}
}
