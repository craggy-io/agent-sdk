// Package query reads the trace back (§7.3): it rebuilds the exact
// OpenRouter request body for a step from stored item hashes and params,
// and provides the run/tree/step listings agentctl and agentcap expose.
package query

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/craggy-io/agent-sdk/trace"
)

const pageSize = 512

// Filter selects events by exact-match fields; empty fields match all.
type Filter struct {
	Type      string
	SessionID string
	RunID     string
	NodeID    string
	StepID    string
}

func (f Filter) matches(e trace.Event) bool {
	return (f.Type == "" || e.Type == f.Type) &&
		(f.SessionID == "" || e.SessionID == f.SessionID) &&
		(f.RunID == "" || e.RunID == f.RunID) &&
		(f.NodeID == "" || e.NodeID == f.NodeID) &&
		(f.StepID == "" || e.StepID == f.StepID)
}

// Events scans the whole log (in cursor order) returning matches. The trace
// is the source of truth for every projection (§7.2), so listings scan
// rather than maintaining side indices.
func Events(ctx context.Context, s trace.Sink, f Filter) ([]trace.Event, error) {
	var out []trace.Event
	after := int64(0)
	for {
		page, next, err := s.ReadEvents(ctx, after, pageSize)
		if err != nil {
			return nil, err
		}
		for _, e := range page {
			if f.matches(e) {
				out = append(out, e)
			}
		}
		if len(page) < pageSize {
			return out, nil
		}
		after = next
	}
}

// findOne returns the last event matching f (events are append-only; a
// step.request is committed once per step attempt chain, last wins).
func findOne(ctx context.Context, s trace.Sink, f Filter) (trace.Event, bool, error) {
	evs, err := Events(ctx, s, f)
	if err != nil || len(evs) == 0 {
		return trace.Event{}, false, err
	}
	return evs[len(evs)-1], true, nil
}

// StepEvents returns every event recorded for a step, in cursor order.
func StepEvents(ctx context.Context, s trace.Sink, stepID string) ([]trace.Event, error) {
	return Events(ctx, s, Filter{StepID: stepID})
}

// RunInfo summarises one run from its run.start/run.end events (§7.1).
type RunInfo struct {
	RunID     string          `json:"run_id"`
	SessionID string          `json:"session_id,omitempty"`
	Status    string          `json:"status,omitempty"` // from run.end; "running" until then
	Start     time.Time       `json:"start"`
	End       time.Time       `json:"end,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"` // run.start payload
	Events    int             `json:"events"`
}

type statusData struct {
	Status string `json:"status"`
}

// Runs lists runs in first-seen order.
func Runs(ctx context.Context, s trace.Sink) ([]RunInfo, error) {
	evs, err := Events(ctx, s, Filter{})
	if err != nil {
		return nil, err
	}
	var order []string
	byID := map[string]*RunInfo{}
	get := func(e trace.Event) *RunInfo {
		r, ok := byID[e.RunID]
		if !ok {
			r = &RunInfo{RunID: e.RunID, SessionID: e.SessionID, Status: "running", Start: e.Time}
			byID[e.RunID] = r
			order = append(order, e.RunID)
		}
		return r
	}
	for _, e := range evs {
		if e.RunID == "" {
			continue
		}
		r := get(e)
		r.Events++
		switch e.Type {
		case "run.start":
			r.Start = e.Time
			r.Data = e.Data
		case "run.end":
			r.End = e.Time
			var d statusData
			if json.Unmarshal(e.Data, &d) == nil && d.Status != "" {
				r.Status = d.Status
			} else {
				r.Status = "ended"
			}
		}
	}
	out := make([]RunInfo, len(order))
	for i, id := range order {
		out[i] = *byID[id]
	}
	return out, nil
}

// NodeInfo is one node of a run tree, from node.start/node.end (§7.1).
type NodeInfo struct {
	NodeID string          `json:"node_id"`
	RunID  string          `json:"run_id"`
	Parent string          `json:"parent,omitempty"`
	Kind   string          `json:"kind,omitempty"`
	Status string          `json:"status,omitempty"`
	Start  time.Time       `json:"start"`
	End    time.Time       `json:"end,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"` // node.start payload
}

type nodeStartData struct {
	Parent string `json:"parent"`
	Kind   string `json:"kind"`
}

// Tree lists the nodes of a run in start order; Parent links form the tree.
func Tree(ctx context.Context, s trace.Sink, runID string) ([]NodeInfo, error) {
	evs, err := Events(ctx, s, Filter{RunID: runID})
	if err != nil {
		return nil, err
	}
	var order []string
	byID := map[string]*NodeInfo{}
	for _, e := range evs {
		switch e.Type {
		case "node.start":
			if _, ok := byID[e.NodeID]; ok {
				continue
			}
			n := &NodeInfo{NodeID: e.NodeID, RunID: e.RunID, Status: "running", Start: e.Time, Data: e.Data}
			var d nodeStartData
			if json.Unmarshal(e.Data, &d) == nil {
				n.Parent, n.Kind = d.Parent, d.Kind
			}
			byID[e.NodeID] = n
			order = append(order, e.NodeID)
		case "node.end":
			if n, ok := byID[e.NodeID]; ok {
				n.End = e.Time
				var d statusData
				if json.Unmarshal(e.Data, &d) == nil && d.Status != "" {
					n.Status = d.Status
				} else {
					n.Status = "ended"
				}
			}
		}
	}
	out := make([]NodeInfo, len(order))
	for i, id := range order {
		out[i] = *byID[id]
	}
	return out, nil
}

// Grep returns anomaly events of given types within a run (§7.3 `grep`):
// e.g. "repair", "retry", "config.fallback".
func Grep(ctx context.Context, s trace.Sink, runID string, types ...string) ([]trace.Event, error) {
	evs, err := Events(ctx, s, Filter{RunID: runID})
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(types))
	for _, t := range types {
		want[t] = true
	}
	var out []trace.Event
	for _, e := range evs {
		if want[e.Type] {
			out = append(out, e)
		}
	}
	return out, nil
}

var errNoStepRequest = func(stepID string) error {
	return fmt.Errorf("trace/query: no step.request event for step %q", stepID)
}
