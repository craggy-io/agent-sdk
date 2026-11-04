// Package agenttest provides the scripted and chaos providers and invariant
// helpers of §13. It is test support for the loop, not production code.
package agenttest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/transcript"
)

// TextResponse builds a plain stop response.
func TextResponse(text string) model.Response {
	return model.Response{
		Item:        model.Item{Kind: model.KindAssistant, Text: text, Finish: model.FinishStop, ServedBy: "scripted"},
		ServedModel: "scripted",
	}
}

// CallsResponse builds a tool_calls response from calls.
func CallsResponse(calls ...model.ToolCall) model.Response {
	return model.Response{
		Item:        model.Item{Kind: model.KindAssistant, Calls: calls, Finish: model.FinishToolCalls, ServedBy: "scripted"},
		ServedModel: "scripted",
	}
}

type scriptEntry struct {
	resp model.Response
	err  error
}

// ScriptedProvider replays a queue of responses/errors and records requests (§13.2).
type ScriptedProvider struct {
	mu       sync.Mutex
	queue    []scriptEntry
	Requests []model.Request
	Info     model.ModelInfo // returned by Model; zero ID filled from argument
}

// NewScripted returns a provider replaying responses in order.
func NewScripted(responses ...model.Response) *ScriptedProvider {
	p := &ScriptedProvider{}
	for _, r := range responses {
		p.queue = append(p.queue, scriptEntry{resp: r})
	}
	return p
}

// Push appends a response to the queue.
func (p *ScriptedProvider) Push(r model.Response) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(p.queue, scriptEntry{resp: r})
}

// PushError appends an error result to the queue.
func (p *ScriptedProvider) PushError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(p.queue, scriptEntry{err: err})
}

// Sample implements model.Provider.
func (p *ScriptedProvider) Sample(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec := req
	rec.Items = append([]model.Item(nil), req.Items...)
	rec.Tools = append([]model.Tool(nil), req.Tools...)
	p.Requests = append(p.Requests, rec)
	if err := ctx.Err(); err != nil {
		return model.Response{}, err
	}
	if len(p.queue) == 0 {
		return model.Response{}, fmt.Errorf("agenttest: scripted provider queue exhausted after %d requests", len(p.Requests))
	}
	e := p.queue[0]
	p.queue = p.queue[1:]
	if e.err != nil {
		return model.Response{}, e.err
	}
	if onDelta != nil && e.resp.Item.Text != "" {
		onDelta(model.Delta{Kind: "text", Text: e.resp.Item.Text})
	}
	return e.resp, nil
}

// Model implements model.Provider.
func (p *ScriptedProvider) Model(_ context.Context, id string) (model.ModelInfo, error) {
	info := p.Info
	if info.ID == "" {
		info.ID = id
	}
	if info.ContextWindow == 0 {
		info.ContextWindow = 128000
	}
	return info, nil
}

// CheckTranscript fails the test unless items satisfy §13.1.
// (Request-rebuild consistency against a trace sink arrives with the trace
// package in a later build stage; see transcript.Check for the invariants.)
func CheckTranscript(t *testing.T, items []model.Item) {
	t.Helper()
	if err := transcript.Check(items); err != nil {
		t.Fatalf("transcript invariant violated: %v", err)
	}
}
