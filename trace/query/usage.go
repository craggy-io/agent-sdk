package query

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

// UsageTotals aggregates token and cost counters over steps. Counts
// come from step.response usage (§8: OpenRouter-reported cost), usage
// a provider reported on step.attempt_failed, and call.end events;
// nothing is estimated here. Usage.Reported is the union over the
// summed samples (see model.Usage.Add).
type UsageTotals struct {
	model.Usage
	Steps int `json:"steps"`
	Calls int `json:"calls"`
	// FailedAttempts counts step.attempt_failed events; their reported
	// usage (rare: most providers report none on errors) is in Usage.
	FailedAttempts int `json:"failed_attempts,omitempty"`
}

// UsageRow is one group of a usage breakdown.
type UsageRow struct {
	Key string `json:"key"` // model id, node id, run id, or tool name
	UsageTotals
	Errors int `json:"errors,omitempty"` // tool rows only: settled is_error calls
}

// UsageReport is the monitoring rollup over a trace selection:
// totals plus per-model, per-run, per-node and per-tool rows.
type UsageReport struct {
	Total   UsageTotals `json:"total"`
	ByModel []UsageRow  `json:"by_model"`
	ByRun   []UsageRow  `json:"by_run"`
	ByNode  []UsageRow  `json:"by_node"`
	ByTool  []UsageRow  `json:"by_tool"`
}

type stepResponseData struct {
	ServedModel string      `json:"served_model"`
	Usage       model.Usage `json:"usage"`
}

type attemptFailedData struct {
	Usage *model.Usage `json:"usage"`
}

type callEndData struct {
	Name    string `json:"name"`
	IsError bool   `json:"is_error"`
}

// Usage aggregates token usage and cost for every step matching f
// (SessionID/RunID/NodeID scoping; Type and StepID ignored). Served
// model is the model that actually answered, so fallback traffic is
// attributed correctly (§7.1).
func Usage(ctx context.Context, s trace.Sink, f Filter) (UsageReport, error) {
	f.Type, f.StepID = "", ""
	evs, err := Events(ctx, s, f)
	if err != nil {
		return UsageReport{}, err
	}
	rep := UsageReport{}
	models := map[string]*UsageRow{}
	runs := map[string]*UsageRow{}
	nodes := map[string]*UsageRow{}
	tools := map[string]*UsageRow{}
	row := func(m map[string]*UsageRow, key string) *UsageRow {
		if key == "" {
			key = "-"
		}
		r, ok := m[key]
		if !ok {
			r = &UsageRow{Key: key}
			m[key] = r
		}
		return r
	}
	for _, e := range evs {
		switch e.Type {
		case "step.response":
			var d stepResponseData
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			for _, t := range []*UsageTotals{&rep.Total,
				&row(models, d.ServedModel).UsageTotals,
				&row(runs, e.RunID).UsageTotals,
				&row(nodes, e.NodeID).UsageTotals} {
				t.Add(d.Usage)
				t.Steps++
			}
		case "step.attempt_failed":
			var d attemptFailedData
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			for _, t := range []*UsageTotals{&rep.Total,
				&row(runs, e.RunID).UsageTotals,
				&row(nodes, e.NodeID).UsageTotals} {
				if d.Usage != nil {
					t.Add(*d.Usage)
				}
				t.FailedAttempts++
			}
		case "call.end":
			var d callEndData
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			rep.Total.Calls++
			row(runs, e.RunID).Calls++
			row(nodes, e.NodeID).Calls++
			tr := row(tools, d.Name)
			tr.Calls++
			if d.IsError {
				tr.Errors++
			}
		}
	}
	rep.ByModel = sortedRows(models)
	rep.ByRun = sortedRows(runs)
	rep.ByNode = sortedRows(nodes)
	rep.ByTool = sortedRows(tools)
	return rep, nil
}

// sortedRows ranks by cost, then calls, then key: the expensive and
// busy groups surface first.
func sortedRows(m map[string]*UsageRow) []UsageRow {
	out := make([]UsageRow, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Key < out[j].Key
	})
	return out
}
