package agenttest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/internal/jschema"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/transcript"
)

func TestScriptedProvider(t *testing.T) {
	p := agenttest.NewScripted(agenttest.TextResponse("one"))
	p.PushError(errors.New("boom"))
	p.Push(agenttest.TextResponse("two"))
	ctx := context.Background()
	var deltas []model.Delta
	r1, err := p.Sample(ctx, model.Request{Model: "m"}, func(d model.Delta) { deltas = append(deltas, d) })
	if err != nil || r1.Item.Text != "one" || len(deltas) != 1 {
		t.Fatalf("first sample: %+v err=%v deltas=%v", r1, err, deltas)
	}
	if _, err := p.Sample(ctx, model.Request{}, nil); err == nil || err.Error() != "boom" {
		t.Fatalf("error entry: %v", err)
	}
	if r3, err := p.Sample(ctx, model.Request{}, nil); err != nil || r3.Item.Text != "two" {
		t.Fatalf("third sample: %+v err=%v", r3, err)
	}
	if _, err := p.Sample(ctx, model.Request{}, nil); err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("exhaustion must error, got %v", err)
	}
	if len(p.Requests) != 4 {
		t.Fatalf("requests recorded = %d, want 4", len(p.Requests))
	}
	info, _ := p.Model(ctx, "x/y")
	if info.ID != "x/y" || info.ContextWindow != 128000 {
		t.Fatalf("Model: %+v", info)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	p.Push(agenttest.TextResponse("never"))
	if _, err := p.Sample(cancelled, model.Request{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: %v", err)
	}
}

func callsResp() model.Response {
	return agenttest.CallsResponse(
		model.ToolCall{ID: "c1", Name: "x", Args: json.RawMessage(`{"a":1}`), ArgsOK: true},
		model.ToolCall{ID: "c2", Name: "y", Args: json.RawMessage(`{"b":2222}`), ArgsOK: true},
	)
}

// §13.3 faults, forced deterministically.
func TestChaosForcedFaults(t *testing.T) {
	ctx := context.Background()
	mk := func(f agenttest.Fault) *agenttest.ChaosProvider {
		return &agenttest.ChaosProvider{Inner: agenttest.NewScripted(callsResp()), Force: f}
	}
	t.Run("drop stream is transient", func(t *testing.T) {
		_, err := mk(agenttest.FaultDropStream).Sample(ctx, model.Request{}, nil)
		var me *model.Error
		if !errors.As(err, &me) || !me.Transient {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("error chunk is a failed attempt", func(t *testing.T) {
		_, err := mk(agenttest.FaultErrorChunk).Sample(ctx, model.Request{}, nil)
		var me *model.Error
		if !errors.As(err, &me) || me.Code != "error_chunk" {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		_, err := mk(agenttest.FaultCancel).Sample(ctx, model.Request{}, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("length mid call truncates args", func(t *testing.T) {
		r, err := mk(agenttest.FaultLengthMidCall).Sample(ctx, model.Request{}, nil)
		if err != nil || r.Item.Finish != model.FinishLength || json.Valid(r.Item.Calls[1].Args) {
			t.Fatalf("resp=%+v err=%v", r, err)
		}
	})
	t.Run("malformed args", func(t *testing.T) {
		r, _ := mk(agenttest.FaultMalformedArgs).Sample(ctx, model.Request{}, nil)
		if json.Valid(r.Item.Calls[0].Args) || r.Item.Calls[0].ArgsOK {
			t.Fatalf("args=%s", r.Item.Calls[0].Args)
		}
	})
	t.Run("double encoded args decode once", func(t *testing.T) {
		r, _ := mk(agenttest.FaultDoubleEncodedArgs).Sample(ctx, model.Request{}, nil)
		got, ok := jschema.DecodeArgs(r.Item.Calls[0].Args)
		if !ok || string(got) != `{"a":1}` {
			t.Fatalf("decoded=%s ok=%v raw=%s", got, ok, r.Item.Calls[0].Args)
		}
	})
	t.Run("missing and duplicate ids repaired by Normalise", func(t *testing.T) {
		for _, f := range []agenttest.Fault{agenttest.FaultMissingCallID, agenttest.FaultDuplicateCallID} {
			r, _ := mk(f).Sample(ctx, model.Request{}, nil)
			r.Item.Seq = 1
			out, repairs := transcript.Normalise([]model.Item{
				{Seq: 0, Kind: model.KindSystem, Text: "s"}, r.Item,
			}, "node-1", "scripted")
			agenttest.CheckTranscript(t, out)
			if len(repairs) == 0 {
				t.Fatalf("fault %s: expected ID repair", f)
			}
		}
	})
	t.Run("reasoning after final is a trailing delta", func(t *testing.T) {
		var deltas []model.Delta
		r, err := mk(agenttest.FaultReasoningAfterFinal).Sample(ctx, model.Request{}, func(d model.Delta) { deltas = append(deltas, d) })
		if err != nil || len(deltas) == 0 || deltas[len(deltas)-1].Kind != "reasoning" || len(r.Item.Calls) != 2 {
			t.Fatalf("resp=%+v deltas=%v err=%v", r, deltas, err)
		}
	})
}

// Seeded chaos: same seed, same fault sequence; counts are recorded.
func TestChaosSeededDeterministic(t *testing.T) {
	run := func() []string {
		script := agenttest.NewScripted()
		for i := 0; i < 30; i++ {
			script.Push(callsResp())
		}
		c := agenttest.NewChaos(script, 42, 1.0) // every call faulted
		var outcomes []string
		for i := 0; i < 30; i++ {
			_, err := c.Sample(context.Background(), model.Request{}, nil)
			if err != nil {
				outcomes = append(outcomes, "err:"+err.Error())
			} else {
				outcomes = append(outcomes, "ok")
			}
		}
		total := 0
		for _, n := range c.Counts {
			total += n
		}
		if total != 30 {
			t.Fatalf("counts total=%d want 30 (%v)", total, c.Counts)
		}
		return outcomes
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("seeded chaos diverges at %d: %q vs %q", i, a[i], b[i])
		}
	}
}

// §13.1 helper actually runs the checker (valid input must not fail).
func TestCheckTranscriptHelper(t *testing.T) {
	agenttest.CheckTranscript(t, []model.Item{
		{Seq: 0, Kind: model.KindSystem, Text: "s"},
		{Seq: 1, Kind: model.KindUser, Text: "u"},
	})
}
