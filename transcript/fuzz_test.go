package transcript_test

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/transcript"
)

// genTranscript builds a pseudo-random transcript from a seed. It only emits
// faults §5.4 promises to repair (missing/orphan/duplicate results, bad call
// IDs, cross-model reasoning, summaries) plus valid structure; it never puts
// a System item after index 0, which Normalise does not repair.
func genTranscript(seed uint64) []model.Item {
	r := rand.New(rand.NewSource(int64(seed)))
	ids := []string{"c1", "c2", "dup", "", "bad id!", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	models := []string{"m-target", "other", ""}
	n := r.Intn(24)
	items := []model.Item{{Seq: 0, Kind: model.KindSystem, Text: "sys"}}
	summarySeqs := []int{}
	for i := 1; i <= n; i++ {
		switch r.Intn(6) {
		case 0:
			items = append(items, model.Item{Seq: i, Kind: model.KindUser, Text: "u"})
		case 1:
			items = append(items, model.Item{Seq: i, Kind: model.KindNotice, Text: "n"})
		case 2, 3:
			it := model.Item{Seq: i, Kind: model.KindAssistant, ServedBy: models[r.Intn(len(models))]}
			if r.Intn(3) == 0 {
				it.Reasoning = &model.Reasoning{
					Details: json.RawMessage(`[{"index":1,"type":"reasoning.text","text":"r"}]`),
					Text:    "r",
				}
			}
			for j := 0; j < r.Intn(3); j++ {
				it.Calls = append(it.Calls, model.ToolCall{
					ID: ids[r.Intn(len(ids))], Name: "tool",
					Args: json.RawMessage(`{}`), ArgsOK: true,
				})
			}
			items = append(items, it)
		case 4:
			items = append(items, model.Item{
				Seq: i, Kind: model.KindToolResult, CallID: ids[r.Intn(len(ids))],
				Text: "ok", Settled: model.SettleRan,
			})
		case 5:
			first := r.Intn(i + 1)
			last := first + r.Intn(3)
			it := model.Item{Seq: i, Kind: model.KindSummary, Text: "s",
				Covers: []model.SeqRange{{First: first, Last: last}}}
			if len(summarySeqs) > 0 && r.Intn(2) == 0 {
				it.Supersedes = []int{summarySeqs[r.Intn(len(summarySeqs))]}
			}
			summarySeqs = append(summarySeqs, i)
			items = append(items, it)
		}
	}
	return items
}

func checkNormaliseProperty(t *testing.T, seed uint64) {
	t.Helper()
	in := genTranscript(seed)
	out, _ := transcript.Normalise(in, "node-fuzz", "m-target")
	if err := transcript.Check(out); err != nil {
		t.Fatalf("seed %d: Normalise output fails Check: %v\nin=%+v\nout=%+v", seed, err, in, out)
	}
	out2, _ := transcript.Normalise(out, "node-fuzz", "m-target")
	if !reflect.DeepEqual(out, out2) {
		t.Fatalf("seed %d: Normalise not idempotent\nfirst=%+v\nsecond=%+v", seed, out, out2)
	}
}

// FuzzNormalise: §13.3 property — after any generated fault sequence the
// normalised projection passes the invariant checker and is a fixpoint.
func FuzzNormalise(f *testing.F) {
	for _, s := range []uint64{0, 1, 2, 3, 7, 42, 99, 12345, 1 << 40} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, seed uint64) {
		checkNormaliseProperty(t, seed)
	})
}

// The same property over a fixed seed sweep, so `go test` (no -fuzz) covers
// far more than the seed corpus alone.
func TestNormalisePropertySeedSweep(t *testing.T) {
	for seed := uint64(0); seed < 500; seed++ {
		checkNormaliseProperty(t, seed)
	}
}
