package loop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/craggy-io/agent-sdk/agenttest"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/trace/memory"
	"github.com/craggy-io/agent-sdk/transcript"
)

// failSink fails Append for one event type, delegating everything else.
type failSink struct {
	trace.Sink
	failType string
}

func (s failSink) Append(ctx context.Context, events ...trace.Event) error {
	for _, ev := range events {
		if ev.Type == s.failType {
			return errors.New("sink down")
		}
	}
	return s.Sink.Append(ctx, events...)
}

// Rule 5 / §7.2: trace-commit failures at write-before-send points are
// fatal — no tool runs against an unrecorded step.response/call.start,
// and a failed call.end never reports success.
func TestTraceCommitFailuresAreFatal(t *testing.T) {
	for _, tc := range []struct {
		failType string
		toolRuns bool // a tool that already ran before call.end cannot un-run
	}{
		{"step.response", false},
		{"call.start", false},
		{"call.end", true},
	} {
		t.Run(tc.failType, func(t *testing.T) {
			tools := newFakeTools().echoTools("echo")
			prov := agenttest.NewScripted(
				resp("", model.FinishToolCalls, call("c1", "echo", `{}`)),
				resp("done", model.FinishStop),
			)
			th := transcript.New()
			rec := trace.NewRecorder(failSink{Sink: memory.New(), failType: tc.failType}, nil)
			in := loop.Input{
				NodeID: "node-1", RunID: "run-1", SessionID: "sess-1", DefName: "test",
				System: "you are a test agent", Thread: th, Task: "do the task",
				Policy:      loop.Policy{},
				Profile:     config.Profile{Model: "scripted", MaxOutputTokens: 100, ParallelToolCalls: true},
				ProfileName: "default",
			}
			deps := loop.Deps{Provider: prov, Tools: tools, Trace: rec, Budget: loop.NewBudget()}
			_, err := loop.Run(context.Background(), deps, in)
			if err == nil {
				t.Fatalf("failing %s commit: loop.Run returned nil error (rule 5)", tc.failType)
			}
			if got := len(tools.called()) > 0; got != tc.toolRuns {
				t.Fatalf("failing %s commit: toolRan=%v, want %v (calls=%v)", tc.failType, got, tc.toolRuns, tools.called())
			}
			// Settle-exactly-once still holds on the fatal path (rule 2).
			agenttest.CheckTranscript(t, th.Items())
		})
	}
}
