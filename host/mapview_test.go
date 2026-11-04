package host_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/model"
)

// Rule 4: MapView.Narrow must shrink what Run can dispatch, not only
// what Tools() lists.
func TestMapViewNarrowRunEnforcesFilter(t *testing.T) {
	ran := 0
	view := host.NewMapView(
		[]model.Tool{
			{Name: "safe", Effect: "read-only", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "danger", Effect: "mutating", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		map[string]host.ToolFunc{
			"safe": func(_ context.Context, _ json.RawMessage) (model.ToolResult, error) {
				return model.ToolResult{Text: "safe"}, nil
			},
			"danger": func(_ context.Context, _ json.RawMessage) (model.ToolResult, error) {
				ran++
				return model.ToolResult{Text: "danger"}, nil
			},
		},
	)
	narrowed := view.Narrow(model.FilterSpec{MaxEffect: "read-only"})
	if got := narrowed.Tools(); len(got) != 1 || got[0].Name != "safe" {
		t.Fatalf("narrowed Tools() = %v, want [safe]", got)
	}
	res, err := narrowed.Run(context.Background(), "danger", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("narrowed Run(danger) error: %v", err)
	}
	if ran != 0 || !res.IsError || res.Code != "unknown_tool" {
		t.Fatalf("narrowed Run(danger) executed filtered tool: ran=%d res=%+v", ran, res)
	}
	// The admitted tool still runs, and the parent view is unchanged.
	if res, _ := narrowed.Run(context.Background(), "safe", json.RawMessage(`{}`)); res.Text != "safe" {
		t.Fatalf("narrowed Run(safe) = %+v", res)
	}
	if res, _ := view.Run(context.Background(), "danger", json.RawMessage(`{}`)); res.Text != "danger" || ran != 1 {
		t.Fatalf("parent Run(danger) = %+v ran=%d", res, ran)
	}
}
