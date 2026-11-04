package trace_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/trace/memory"
)

func TestItemHashCanonical(t *testing.T) {
	it := model.Item{Seq: 3, Kind: model.KindUser, Text: "hi"}
	h1, h2 := trace.ItemHash(it), trace.ItemHash(it)
	if h1 != h2 {
		t.Errorf("ItemHash not stable: %s vs %s", h1, h2)
	}
	raw, err := trace.CanonicalItem(it)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != trace.HashOf(raw) {
		t.Errorf("ItemHash != HashOf(canonical JSON)")
	}
	other := it
	other.Text = "bye"
	if trace.ItemHash(other) == h1 {
		t.Errorf("distinct items share a hash")
	}
}

// §7.1: step.request stores ordered item hashes + a params blob; items are
// stored once (dedupe across steps follows from content addressing).
func TestRecorderStepRequest(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	rec := trace.NewRecorder(s, nil)
	items := []model.Item{
		{Seq: 0, Kind: model.KindSystem, Text: "sys"},
		{Seq: 1, Kind: model.KindUser, Text: "go"},
	}
	meta := trace.Event{SessionID: "s1", RunID: "r1", NodeID: "n1", StepID: "step-1"}
	params := trace.RequestParams{Model: "m", Reasoning: model.ReasoningCfg{Effort: "low"}}
	if err := rec.RecordStepRequest(ctx, meta, items, params, []string{"dropped orphan"}); err != nil {
		t.Fatal(err)
	}
	evs, _, err := s.ReadEvents(ctx, 0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events=%v err=%v", evs, err)
	}
	if evs[0].Type != "step.request" || evs[0].StepID != "step-1" {
		t.Fatalf("event=%+v", evs[0])
	}
	if evs[0].Time.IsZero() {
		t.Errorf("Recorder.Emit did not stamp Time")
	}
	var data trace.StepRequestData
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Items) != 2 || data.Items[0] != trace.ItemHash(items[0]) || data.Items[1] != trace.ItemHash(items[1]) {
		t.Errorf("hashes=%v", data.Items)
	}
	if len(data.Repairs) != 1 {
		t.Errorf("repairs=%v", data.Repairs)
	}
	got, err := trace.GetItem(ctx, s, data.Items[1])
	if err != nil || got.Text != "go" {
		t.Errorf("GetItem=%+v err=%v", got, err)
	}
	pb, err := s.GetBlob(ctx, data.Params)
	if err != nil {
		t.Fatal(err)
	}
	var p trace.RequestParams
	if json.Unmarshal(pb, &p) != nil || p.Model != "m" {
		t.Errorf("params blob=%s", pb)
	}
}

// A Recorder built with a lease appends under its fencing token (§7.2).
func TestRecorderWithLease(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	lease, err := s.AcquireWriter(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	rec := trace.NewRecorder(s, lease)
	if err := rec.Emit(ctx, trace.Event{Type: "run.start", SessionID: "s1"}); err != nil {
		t.Errorf("leased Emit: %v", err)
	}
	bare := trace.NewRecorder(s, nil)
	if err := bare.Emit(ctx, trace.Event{Type: "run.start", SessionID: "s1"}); err == nil {
		t.Errorf("unleased Emit under live lease succeeded, want fencing error")
	}
}
