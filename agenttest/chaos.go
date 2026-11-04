package agenttest

import (
	"context"
	"encoding/json"
	"math/rand"
	"sync"

	"github.com/craggy-io/agent-sdk/model"
)

// Fault is one injectable chaos fault (§13.3 subset expressible at the
// model.Provider boundary).
type Fault string

const (
	FaultNone                Fault = ""
	FaultDropStream          Fault = "drop_stream"           // transient stream error, no response
	FaultLengthMidCall       Fault = "length_mid_call"       // finish=length with truncated call args
	FaultMalformedArgs       Fault = "malformed_args"        // first call's args become invalid JSON
	FaultDoubleEncodedArgs   Fault = "double_encoded_args"   // first call's args wrapped as a JSON string
	FaultMissingCallID       Fault = "missing_call_id"       // first call's ID cleared
	FaultDuplicateCallID     Fault = "duplicate_call_id"     // second call (or first) reuses the first ID
	FaultErrorChunk          Fault = "error_chunk"           // HTTP-200-with-error-chunk as a failed attempt
	FaultReasoningAfterFinal Fault = "reasoning_after_final" // trailing reasoning delta after finish (see below)
	FaultCancel              Fault = "cancel"                // context.Canceled
)

// AllFaults lists the injectable faults for seeded random selection.
var AllFaults = []Fault{
	FaultDropStream, FaultLengthMidCall, FaultMalformedArgs, FaultDoubleEncodedArgs,
	FaultMissingCallID, FaultDuplicateCallID, FaultErrorChunk, FaultReasoningAfterFinal, FaultCancel,
}

// ChaosProvider wraps a provider and injects seeded faults (§13.3). With
// Force set, that fault is applied to every call (deterministic tests).
// Otherwise a fault is applied with probability Rate (default 0.3), chosen
// uniformly from Faults (default AllFaults). Counts records applied faults.
//
// Note: "reasoning after the finish chunk" is an SSE-level fault. At this
// boundary it is simulated by emitting a trailing reasoning Delta via
// onDelta just before returning the unchanged response; true post-finish
// chunk handling is tested in the openrouter package against raw SSE.
// Likewise empty-choices and idle-trickle faults belong to that layer.
type ChaosProvider struct {
	Inner  model.Provider
	Seed   int64
	Rate   float64
	Faults []Fault
	Force  Fault
	Counts map[Fault]int

	mu  sync.Mutex
	rng *rand.Rand
}

// NewChaos wraps inner with a seeded fault injector.
func NewChaos(inner model.Provider, seed int64, rate float64) *ChaosProvider {
	return &ChaosProvider{Inner: inner, Seed: seed, Rate: rate, Counts: map[Fault]int{}}
}

func (c *ChaosProvider) pick() Fault {
	if c.Force != FaultNone {
		return c.Force
	}
	rate := c.Rate
	if rate == 0 {
		rate = 0.3
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rng == nil {
		c.rng = rand.New(rand.NewSource(c.Seed))
	}
	if c.rng.Float64() >= rate {
		return FaultNone
	}
	faults := c.Faults
	if len(faults) == 0 {
		faults = AllFaults
	}
	return faults[c.rng.Intn(len(faults))]
}

// Sample implements model.Provider.
func (c *ChaosProvider) Sample(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	fault := c.pick()
	// Pre-response faults.
	switch fault {
	case FaultCancel:
		c.count(fault)
		return model.Response{}, context.Canceled
	case FaultDropStream:
		c.count(fault)
		return model.Response{}, &model.Error{Status: 503, Code: "stream_dropped", Message: "chaos: stream dropped", Transient: true}
	case FaultErrorChunk:
		c.count(fault)
		return model.Response{}, &model.Error{Status: 200, Code: "error_chunk", Message: "chaos: error chunk in 200 stream", Transient: true}
	}
	resp, err := c.Inner.Sample(ctx, req, onDelta)
	if err != nil || fault == FaultNone {
		return resp, err
	}
	c.count(fault)
	return ApplyFault(resp, fault, onDelta), nil
}

// Model implements model.Provider by delegating.
func (c *ChaosProvider) Model(ctx context.Context, id string) (model.ModelInfo, error) {
	return c.Inner.Model(ctx, id)
}

func (c *ChaosProvider) count(f Fault) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Counts == nil {
		c.Counts = map[Fault]int{}
	}
	c.Counts[f]++
}

// ApplyFault mutates a response with fault, deterministically. Faults that
// need calls are no-ops on a call-less response (except length, which sets
// Finish). Exported so loop tests can script exact fault sequences.
func ApplyFault(resp model.Response, fault Fault, onDelta func(model.Delta)) model.Response {
	switch fault {
	case FaultLengthMidCall:
		resp.Item.Finish = model.FinishLength
		if n := len(resp.Item.Calls); n > 0 {
			args := resp.Item.Calls[n-1].Args
			if len(args) > 2 {
				resp.Item.Calls[n-1].Args = args[:len(args)/2]
				resp.Item.Calls[n-1].ArgsOK = false
			}
		}
	case FaultMalformedArgs:
		if len(resp.Item.Calls) > 0 {
			resp.Item.Calls[0].Args = json.RawMessage(`{"chaos":`)
			resp.Item.Calls[0].ArgsOK = false
		}
	case FaultDoubleEncodedArgs:
		if len(resp.Item.Calls) > 0 {
			inner := resp.Item.Calls[0].Args
			if len(inner) == 0 {
				inner = json.RawMessage(`{}`)
			}
			wrapped, err := json.Marshal(string(inner))
			if err == nil {
				resp.Item.Calls[0].Args = wrapped
			}
		}
	case FaultMissingCallID:
		if len(resp.Item.Calls) > 0 {
			resp.Item.Calls[0].ID = ""
		}
	case FaultDuplicateCallID:
		if len(resp.Item.Calls) > 1 {
			resp.Item.Calls[1].ID = resp.Item.Calls[0].ID
		} else if len(resp.Item.Calls) == 1 {
			dup := resp.Item.Calls[0]
			resp.Item.Calls = append(resp.Item.Calls, dup)
		}
	case FaultReasoningAfterFinal:
		if onDelta != nil {
			onDelta(model.Delta{Kind: "reasoning", Text: "chaos trailing reasoning"})
		}
	}
	return resp
}
