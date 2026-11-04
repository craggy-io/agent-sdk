package agentcap

import (
	"context"
	"encoding/json"

	"github.com/craggy-io/agent-sdk/trace/query"
	"github.com/craggy-io/agent-sdk/workflow"
	"github.com/craggy-io/capability-services/registry"
)

func (s *service) traceCaps() []registry.Capability {
	ro := registry.WithEffects(registry.ReadOnly)
	return []registry.Capability{
		registry.Func("recovery.list", "List operations with unknown outcomes",
			func(ctx context.Context, in struct {
				RunID string `json:"run_id,omitempty"`
			}) (json.RawMessage, error) {
				ops, err := workflow.ListRecovery(ctx, s.h.Sink(), in.RunID)
				if err != nil {
					return nil, coded(err)
				}
				return json.Marshal(ops)
			}, ro),
		registry.Func("recovery.resolve", "Resolve an unknown outcome",
			func(ctx context.Context, in struct {
				OpKey      string `json:"op_key"`
				RunID      string `json:"run_id,omitempty"`
				Decision   string `json:"decision" jsonschema:"success|failure|retry"`
				Evidence   string `json:"evidence,omitempty"`
				ResultText string `json:"result_text,omitempty"`
			}) (map[string]any, error) {
				caller := s.h.Caller().ID
				if c, ok := registry.CallerFrom(ctx); ok && c.ID != "" {
					caller = c.ID
				}
				if err := workflow.ResolveRecovery(ctx, s.h.Recorder(), in.RunID, in.OpKey,
					in.Decision, in.Evidence, in.ResultText, caller); err != nil {
					return nil, coded(err)
				}
				return map[string]any{"resolved": true, "op_key": in.OpKey, "decision": in.Decision}, nil
			}),
		registry.Func("trace.runs", "List runs in the trace",
			func(ctx context.Context, _ struct{}) (json.RawMessage, error) {
				if s.h.Sink() == nil {
					return json.RawMessage(`[]`), nil
				}
				runs, err := query.Runs(ctx, s.h.Sink())
				if err != nil {
					return nil, coded(err)
				}
				return json.Marshal(runs)
			}, ro),
		registry.Func("trace.tree", "Node tree of one run",
			func(ctx context.Context, in struct {
				RunID string `json:"run_id"`
			}) (json.RawMessage, error) {
				if s.h.Sink() == nil {
					return nil, registry.Errorf(registry.Unavailable, "no trace sink")
				}
				nodes, err := query.Tree(ctx, s.h.Sink(), in.RunID)
				if err != nil {
					return nil, coded(err)
				}
				return json.Marshal(nodes)
			}, ro),
		registry.Func("trace.step", "Rebuilt request and events for one step",
			func(ctx context.Context, in struct {
				StepID string `json:"step_id"`
			}) (json.RawMessage, error) {
				if s.h.Sink() == nil {
					return nil, registry.Errorf(registry.Unavailable, "no trace sink")
				}
				body, err := query.RebuildRequest(ctx, s.h.Sink(), in.StepID)
				if err != nil {
					return nil, coded(err)
				}
				evs, err := query.StepEvents(ctx, s.h.Sink(), in.StepID)
				if err != nil {
					return nil, coded(err)
				}
				return json.Marshal(map[string]any{"step_id": in.StepID,
					"request": json.RawMessage(body), "events": evs})
			}, ro),
		registry.Func("trace.query", "Filter trace events (paged)",
			func(ctx context.Context, in struct {
				Type      string `json:"type,omitempty"`
				RunID     string `json:"run_id,omitempty"`
				NodeID    string `json:"node_id,omitempty"`
				StepID    string `json:"step_id,omitempty"`
				SessionID string `json:"session_id,omitempty"`
				Offset    int    `json:"offset,omitempty"`
				Limit     int    `json:"limit,omitempty"`
			}) (json.RawMessage, error) {
				if s.h.Sink() == nil {
					return json.RawMessage(`{"events":[],"total":0}`), nil
				}
				evs, err := query.Events(ctx, s.h.Sink(), query.Filter{
					Type: in.Type, RunID: in.RunID, NodeID: in.NodeID,
					StepID: in.StepID, SessionID: in.SessionID})
				if err != nil {
					return nil, coded(err)
				}
				total := len(evs)
				if in.Offset > 0 {
					if in.Offset >= len(evs) {
						evs = nil
					} else {
						evs = evs[in.Offset:]
					}
				}
				limit := in.Limit
				if limit <= 0 || limit > 500 {
					limit = 500
				}
				if len(evs) > limit {
					evs = evs[:limit]
				}
				return json.Marshal(map[string]any{"events": evs, "total": total})
			}, ro),
		registry.Func("trace.usage", "Token and cost rollups",
			func(ctx context.Context, in struct {
				RunID     string `json:"run_id,omitempty"`
				NodeID    string `json:"node_id,omitempty"`
				SessionID string `json:"session_id,omitempty"`
			}) (json.RawMessage, error) {
				if s.h.Sink() == nil {
					return json.Marshal(query.UsageReport{})
				}
				rep, err := query.Usage(ctx, s.h.Sink(), query.Filter{
					RunID: in.RunID, NodeID: in.NodeID, SessionID: in.SessionID})
				if err != nil {
					return nil, coded(err)
				}
				return json.Marshal(rep)
			}, ro),
	}
}
