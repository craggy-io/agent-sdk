package flow

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// Lib is the Starlark library every pipeline loads as Module: a function
// per step kind and flow.run, which runs STEPS. Its steps call capabilities
// with the host's call(ref, **args) built-in and report progress with
// print() lines that ReadEvent reads. It also needs struct and json among
// the host's built-ins.
//
//go:embed flow.star
var Lib string

// EventPrefix starts each progress line Lib prints.
const EventPrefix = "flow:"

// Event is one progress line: a step starting (Step, with Item and Keys
// inside a For each) or done (Done, with the Count it made).
type Event struct {
	Step  string   `json:"step,omitempty"`
	Item  *int     `json:"item,omitempty"`
	Keys  []string `json:"keys,omitempty"`
	Done  string   `json:"done,omitempty"`
	Count int      `json:"count,omitempty"`
}

// ReadEvent reads a line a run printed; ok is false for any other line.
func ReadEvent(line string) (Event, bool) {
	raw, ok := strings.CutPrefix(line, EventPrefix)
	if !ok {
		return Event{}, false
	}
	var ev Event
	if json.Unmarshal([]byte(raw), &ev) != nil {
		return Event{}, false
	}
	return ev, true
}

// Modules are the files a run loads: Lib as Module and the bundle's own
// .star files but pipeline.star.
func (b Bundle) Modules() map[string]string {
	out := map[string]string{Module: Lib}
	for p, text := range b {
		if strings.HasSuffix(p, ".star") && p != FileName {
			out[p] = text
		}
	}
	return out
}

// TestInputs are a test run's inputs, from InputsFile, in the order the
// header declares them and keyed (Keyed).
func TestInputs(b Bundle, h Header) (map[string][]map[string]any, error) {
	text, ok := b[InputsFile]
	if !ok {
		return nil, fmt.Errorf("flow: the pipeline has no %s", InputsFile)
	}
	var td map[string][]map[string]any
	if err := json.Unmarshal([]byte(text), &td); err != nil {
		return nil, fmt.Errorf("flow: %s: %w", InputsFile, err)
	}
	out := map[string][]map[string]any{}
	for _, name := range h.InputOrder {
		out[name] = Keyed(td[name], h.Inputs[name].Key)
	}
	return out, nil
}

// Keyed is rows each marked with its key as _key, which Lib follows
// through every step for lineage.
func Keyed(rows []map[string]any, key string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		cp := make(map[string]any, len(row)+1)
		for k, v := range row {
			cp[k] = v
		}
		cp["_key"] = fmt.Sprint(row[key])
		out = append(out, cp)
	}
	return out
}

// Check refuses an input whose key isn't a column name or whose filter is
// more than one expression: what a host checks before it builds a query.
func (in Input) Check() error {
	if !ident(in.Key) {
		return fmt.Errorf("bad key %q", in.Key)
	}
	if strings.Contains(in.Where, ";") {
		return fmt.Errorf("a filter is one expression")
	}
	return nil
}

func ident(s string) bool {
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}
