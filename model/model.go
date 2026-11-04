// Package model defines the provider-neutral data types (§5.1, §8).
// It has no dependencies on other agent-sdk packages.
package model

import "encoding/json"

// ItemKind classifies a transcript item (§5.1).
type ItemKind string

// Kind consts are Kind-prefixed because Go cannot have both a const
// ToolResult and the pinned type ToolResult (below) in one package.
const (
	KindSystem     ItemKind = "system"
	KindUser       ItemKind = "user"
	KindAssistant  ItemKind = "assistant"
	KindToolResult ItemKind = "tool_result"
	KindNotice     ItemKind = "notice"
	KindSummary    ItemKind = "summary"
)

// Finish is why a sample stopped (§5.1).
type Finish string

const (
	FinishStop          Finish = "stop"
	FinishToolCalls     Finish = "tool_calls"
	FinishLength        Finish = "length"
	FinishContentFilter Finish = "content_filter"
	FinishError         Finish = "error"
	FinishMissing       Finish = "missing"
)

// SettleReason records how a tool call was settled (§5.1, rule 2).
type SettleReason string

const (
	SettleRan             SettleReason = "ran"
	SettleInvalidArgs     SettleReason = "invalid_args"
	SettleUnknownTool     SettleReason = "unknown_tool"
	SettleAborted         SettleReason = "aborted"
	SettleTruncatedOutput SettleReason = "truncated_output"
	SettleBudget          SettleReason = "budget"
	SettleNotDispatched   SettleReason = "not_dispatched"
	SettleOutcomeUnknown  SettleReason = "outcome_unknown"
)

// SeqRange is an inclusive range of original item sequence numbers (§5.1).
type SeqRange struct {
	First int `json:"first"`
	Last  int `json:"last"`
}

// InputOrigin records where a delivered input came from (§5.1, §6.6).
type InputOrigin struct {
	CallerID   string `json:"caller_id,omitempty"`
	SourceNode string `json:"source_node,omitempty"`
	Mode       string `json:"mode,omitempty"` // message | steer | follow_up
}

// Reasoning is opaque provider reasoning, bound to the served model (§5.1).
type Reasoning struct {
	Details json.RawMessage `json:"details,omitempty"` // provider reasoning_details, byte-exact minus index
	Text    string          `json:"text,omitempty"`    // display text, if the provider supplied any
}

// ToolCall is one tool call proposed by an assistant item (§5.1).
type ToolCall struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args,omitempty"` // exactly as received, even if invalid
	ArgsOK bool            `json:"args_ok"`
}

// Item is one immutable transcript entry (§5.1). The transcript is append-only (rule 1).
type Item struct {
	Seq     int          `json:"seq"`
	Kind    ItemKind     `json:"kind"`
	Text    string       `json:"text,omitempty"`
	InputID string       `json:"input_id,omitempty"`
	Origin  *InputOrigin `json:"origin,omitempty"`
	// Assistant only.
	Calls     []ToolCall `json:"calls,omitempty"`
	Reasoning *Reasoning `json:"reasoning,omitempty"`
	ServedBy  string     `json:"served_by,omitempty"`
	Finish    Finish     `json:"finish,omitempty"`
	// ToolResult only.
	CallID  string       `json:"call_id,omitempty"`
	IsError bool         `json:"is_error,omitempty"`
	Settled SettleReason `json:"settled,omitempty"`
	// Summary only (§5.6).
	Covers     []SeqRange `json:"covers,omitempty"`
	Supersedes []int      `json:"supersedes,omitempty"`
	// Pinned items are kept verbatim by compaction (§5.6).
	Pinned bool `json:"pinned,omitempty"`
}

// Tool is one advertised tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Effect      string          `json:"effect,omitempty"` // read-only|idempotent|mutating|long
	// Group names the ToolGroup this tool belongs to; empty when none.
	Group string `json:"group,omitempty"`
}

// ToolGroup describes a family of tools in one catalogue line (the
// registry's Group, mirrored). LoadAlways groups are advertised from
// the first step under loop.progressive_tools; the rest wait for
// tools_load.
type ToolGroup struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	LoadAlways  bool   `json:"load_always,omitempty"`
}

// ToolResult is the outcome of running a tool (§9.3.3 shape, agents-local).
type ToolResult struct {
	Text    string `json:"text,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	Code    string `json:"code,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}
