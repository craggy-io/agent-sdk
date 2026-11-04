// Package trace is the append-only event log plus content-addressed blob
// store (§7.1, §7.2). Items are stored once as blobs keyed by the SHA-256 of
// their canonical JSON; a step.request event records ordered item hashes so
// trace/query can rebuild the exact request body that was sent.
package trace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/craggy-io/agent-sdk/model"
)

// Hash is the hex SHA-256 of a blob's bytes.
type Hash string

// Event is one durable trace record (§7.1). Cursor is assigned by the sink
// atomically on Append; it is zero on input.
type Event struct {
	Cursor    int64           `json:"cursor,omitempty"`
	Type      string          `json:"type"`
	Time      time.Time       `json:"time"`
	SessionID string          `json:"session_id,omitempty"`
	RunID     string          `json:"run_id,omitempty"`
	NodeID    string          `json:"node_id,omitempty"`
	StepID    string          `json:"step_id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// WriterLease is the per-session single-writer lease (§7.2). Token is a
// monotonically increasing fencing token; sinks reject appends carrying a
// stale token (see WithLease).
type WriterLease interface {
	Token() int64
	Renew(ctx context.Context) error
	Release(ctx context.Context) error
}

// Sink is the storage contract (§7.2). Append is an atomic batch, durable on
// return, and assigns cursors. PutBlob is idempotent. ReadEvents returns the
// events after the given cursor and the cursor to resume from.
type Sink interface {
	Append(ctx context.Context, events ...Event) error
	PutBlob(ctx context.Context, data []byte) (Hash, error)
	GetBlob(ctx context.Context, h Hash) ([]byte, error)
	ReadEvents(ctx context.Context, after int64, limit int) (events []Event, next int64, err error)
	AcquireWriter(ctx context.Context, sessionID string) (WriterLease, error)
}

var (
	// ErrStaleWriter is returned when an append/renew carries a fencing
	// token that is no longer the current lease for its session.
	ErrStaleWriter = errors.New("trace: stale writer lease")
	// ErrLeaseHeld is returned by AcquireWriter when another writer holds
	// an unexpired lease for the session.
	ErrLeaseHeld = errors.New("trace: writer lease held")
	// ErrNotFound is returned by GetBlob for an unknown hash.
	ErrNotFound = errors.New("trace: blob not found")
)

type leaseCtxKey struct{}

// WithLease binds a lease fencing token to ctx. Sinks require Append calls
// for a leased session to carry the current token this way; Recorder does it
// automatically when constructed with a lease.
func WithLease(ctx context.Context, token int64) context.Context {
	return context.WithValue(ctx, leaseCtxKey{}, token)
}

// LeaseToken extracts the fencing token bound by WithLease.
func LeaseToken(ctx context.Context) (int64, bool) {
	t, ok := ctx.Value(leaseCtxKey{}).(int64)
	return t, ok
}

// HashOf returns the content hash of raw blob bytes.
func HashOf(data []byte) Hash {
	sum := sha256.Sum256(data)
	return Hash(hex.EncodeToString(sum[:]))
}

// CanonicalItem is the canonical JSON encoding of an item: encoding/json
// over the model.Item struct (fixed field order, sorted map keys), which is
// byte-stable for equal items.
func CanonicalItem(item model.Item) ([]byte, error) {
	return json.Marshal(item)
}

// ItemHash is the blob hash under which an item is stored (§7.1).
func ItemHash(item model.Item) Hash {
	b, err := CanonicalItem(item)
	if err != nil { // model.Item contains only JSON-safe values
		panic(err)
	}
	return HashOf(b)
}

// PutItem stores an item as a content-addressed blob and returns its hash.
func PutItem(ctx context.Context, s Sink, item model.Item) (Hash, error) {
	b, err := CanonicalItem(item)
	if err != nil {
		return "", err
	}
	return s.PutBlob(ctx, b)
}

// GetItem loads an item blob by hash.
func GetItem(ctx context.Context, s Sink, h Hash) (model.Item, error) {
	b, err := s.GetBlob(ctx, h)
	if err != nil {
		return model.Item{}, err
	}
	var item model.Item
	if err := json.Unmarshal(b, &item); err != nil {
		return model.Item{}, fmt.Errorf("trace: decode item %s: %w", h, err)
	}
	return item, nil
}

// RequestParams is everything about a step.request except the items
// themselves. It is stored as one blob; together with the ordered item
// hashes it rebuilds the exact OpenRouter body (§7.1, §8).
type RequestParams struct {
	Model             string              `json:"model"`
	Fallbacks         []string            `json:"fallbacks,omitempty"`
	Tools             []model.Tool        `json:"tools,omitempty"`
	Reasoning         model.ReasoningCfg  `json:"reasoning"`
	MaxOutputTokens   int                 `json:"max_output_tokens,omitempty"`
	ParallelToolCalls bool                `json:"parallel_tool_calls,omitempty"`
	ToolChoice        string              `json:"tool_choice,omitempty"`
	Provider          model.ProviderPrefs `json:"provider"`
	Cache             string              `json:"cache,omitempty"`
	SessionID         string              `json:"session_id,omitempty"`
}

// ParamsOf extracts the storable params from a model.Request.
func ParamsOf(req model.Request) RequestParams {
	return RequestParams{
		Model:             req.Model,
		Fallbacks:         req.Fallbacks,
		Tools:             req.Tools,
		Reasoning:         req.Reasoning,
		MaxOutputTokens:   req.MaxOutputTokens,
		ParallelToolCalls: req.ParallelToolCalls,
		ToolChoice:        req.ToolChoice,
		Provider:          req.Provider,
		Cache:             req.Cache,
		SessionID:         req.SessionID,
	}
}

// ToolMeta is queryable per-tool metadata recorded on step.request
// events (§7.1): which tools the model was offered at that exact
// request, without opening the params blob. Name is the tool ID as
// advertised; SchemaHash is ToolHash of the definition as sent.
type ToolMeta struct {
	Name       string `json:"name"`
	Effect     string `json:"effect,omitempty"`
	SchemaHash string `json:"schema_hash"`
}

// ToolHash is the short definition hash shared by node.start and
// step.request: first 8 bytes (hex) of SHA-256 over
// name|effect|schema+description. Equal hashes mean the model saw
// the same tool definition.
func ToolHash(t model.Tool) string {
	sum := sha256.Sum256(append(append([]byte(t.Name+"|"+t.Effect+"|"), t.Schema...), []byte(t.Description)...))
	return hex.EncodeToString(sum[:8])
}

// ToolMetas projects advertised tools to their metadata form.
func ToolMetas(tools []model.Tool) []ToolMeta {
	out := make([]ToolMeta, len(tools))
	for i, t := range tools {
		out[i] = ToolMeta{Name: t.Name, Effect: t.Effect, SchemaHash: ToolHash(t)}
	}
	return out
}

// StepRequestData is the Data payload of a step.request event (§7.1).
// Items/Params carry the exact bytes (blobs); Model/Tools duplicate
// the headline facts as plain event fields so SQL/agentctl queries
// can filter and aggregate without blob reads.
type StepRequestData struct {
	Items   []Hash     `json:"items"`             // ordered item hashes as sent
	Params  Hash       `json:"params"`            // blob hash of RequestParams JSON
	Model   string     `json:"model"`             // requested model (served may differ)
	Tools   []ToolMeta `json:"tools"`             // offered tools as sent, in order
	Repairs []string   `json:"repairs,omitempty"` // normalisation repairs (§5.4)
}

// Recorder wraps a Sink with the write helpers the loop uses. If built with
// a lease, every Append carries its fencing token.
type Recorder struct {
	Sink  Sink
	Lease WriterLease // optional
}

// NewRecorder builds a Recorder; lease may be nil (memory tests, read-mostly
// tools that still append annotations).
func NewRecorder(s Sink, lease WriterLease) *Recorder {
	return &Recorder{Sink: s, Lease: lease}
}

func (r *Recorder) ctx(ctx context.Context) context.Context {
	if r.Lease != nil {
		return WithLease(ctx, r.Lease.Token())
	}
	return ctx
}

// Emit appends events, stamping Time where unset. A Sink failure is fatal to
// the caller (§7.2): an unrecorded step is worse than a stopped one.
func (r *Recorder) Emit(ctx context.Context, events ...Event) error {
	now := time.Now().UTC()
	for i := range events {
		if events[i].Time.IsZero() {
			events[i].Time = now
		}
	}
	return r.Sink.Append(r.ctx(ctx), events...)
}

// PutItem stores one item blob (§7.1: messages are stored once).
func (r *Recorder) PutItem(ctx context.Context, item model.Item) (Hash, error) {
	return PutItem(ctx, r.Sink, item)
}

// PutBlob stores raw bytes and returns their hash.
func (r *Recorder) PutBlob(ctx context.Context, data []byte) (Hash, error) {
	return r.Sink.PutBlob(ctx, data)
}

// RecordStepRequest commits a step.request event: every item as a blob, the
// params as one blob, then the event referencing both. Callers must commit
// this before sending the request (write-before-send, §5.2/§7.1). meta
// supplies SessionID/RunID/NodeID/StepID; Type and Data are set here.
func (r *Recorder) RecordStepRequest(ctx context.Context, meta Event, items []model.Item, params RequestParams, repairs []string) error {
	hashes := make([]Hash, len(items))
	for i, it := range items {
		h, err := r.PutItem(ctx, it)
		if err != nil {
			return err
		}
		hashes[i] = h
	}
	pb, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ph, err := r.PutBlob(ctx, pb)
	if err != nil {
		return err
	}
	data, err := json.Marshal(StepRequestData{Items: hashes, Params: ph,
		Model: params.Model, Tools: ToolMetas(params.Tools), Repairs: repairs})
	if err != nil {
		return err
	}
	meta.Type = "step.request"
	meta.Data = data
	return r.Emit(ctx, meta)
}
