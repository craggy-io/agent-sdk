package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

// This file defines the canonical OpenRouter request body (§8). The
// openrouter provider emits byte-identical bodies for the same
// RequestParams+items by calling BuildBody, so RebuildRequest is exact
// replay (§7.1), not a reconstruction. Anthropic-family cache_control
// breakpoints (§8) and the require_parameters default live here for the
// same reason: one definition of the bytes.

type orBody struct {
	Model      string      `json:"model"`
	Models     []string    `json:"models,omitempty"`
	Messages   []orMessage `json:"messages"`
	Tools      []orTool    `json:"tools,omitempty"`
	ToolChoice string      `json:"tool_choice,omitempty"`
	// Omitempty: with require_parameters, merely sending the field (even
	// false) 404s on models that do not list parallel_tool_calls as a
	// supported parameter (found live against meta/muse-spark-1.3-contributor).
	ParallelToolCalls bool         `json:"parallel_tool_calls,omitempty"`
	Reasoning         *orReasoning `json:"reasoning,omitempty"`
	MaxTokens         int          `json:"max_tokens,omitempty"`
	Provider          orProvider   `json:"provider"`
	SessionID         string       `json:"session_id,omitempty"`
	Plugins           []orPlugin   `json:"plugins"`
	Stream            bool         `json:"stream"`
}

type orMessage struct {
	Role string `json:"role"`
	// Content is a plain string, or []orContentBlock when a cache_control
	// breakpoint lands on this message (§8: text blocks only).
	Content          any             `json:"content,omitempty"`
	ToolCalls        []orToolCall    `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
}

type orCacheControl struct {
	Type string `json:"type"` // ephemeral
}

type orContentBlock struct {
	Type         string          `json:"type"` // text
	Text         string          `json:"text"`
	CacheControl *orCacheControl `json:"cache_control,omitempty"`
}

type orToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type orTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
	CacheControl *orCacheControl `json:"cache_control,omitempty"`
}

type orReasoning struct {
	Effort    string `json:"effort,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
}

type orProvider struct {
	RequireParameters bool   `json:"require_parameters"`
	DataCollection    string `json:"data_collection,omitempty"`
	Sort              string `json:"sort,omitempty"`
}

type orPlugin struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// RebuildRequest returns the exact JSON body sent to OpenRouter for stepID,
// rebuilt from the step.request event's ordered item hashes and params blob
// (§7.1 exact replay).
func RebuildRequest(ctx context.Context, s trace.Sink, stepID string) ([]byte, error) {
	ev, ok, err := findOne(ctx, s, Filter{Type: "step.request", StepID: stepID})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errNoStepRequest(stepID)
	}
	var data trace.StepRequestData
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return nil, fmt.Errorf("trace/query: step.request data: %w", err)
	}
	pb, err := s.GetBlob(ctx, data.Params)
	if err != nil {
		return nil, err
	}
	var params trace.RequestParams
	if err := json.Unmarshal(pb, &params); err != nil {
		return nil, fmt.Errorf("trace/query: params blob: %w", err)
	}
	items := make([]model.Item, len(data.Items))
	for i, h := range data.Items {
		it, err := trace.GetItem(ctx, s, h)
		if err != nil {
			return nil, err
		}
		items[i] = it
	}
	return BuildBody(params, items)
}

// BuildBody renders params+items as the canonical OpenRouter body. It is
// exported so the provider and tests share one definition of the bytes.
func BuildBody(p trace.RequestParams, items []model.Item) ([]byte, error) {
	body := orBody{
		Model:             p.Model,
		ParallelToolCalls: p.ParallelToolCalls,
		MaxTokens:         p.MaxOutputTokens,
		SessionID:         p.SessionID,
		Provider: orProvider{
			// §8: require_parameters defaults true so unsupported
			// parameters fail loudly. ProviderPrefs has no tri-state, so
			// an entirely unset prefs block means "default".
			RequireParameters: p.Provider.RequireParameters ||
				(p.Provider.DataCollection == "" && p.Provider.Sort == ""),
			DataCollection: p.Provider.DataCollection,
			Sort:           p.Provider.Sort,
		},
		// §8: gateway context compression off; compaction is ours.
		Plugins: []orPlugin{{ID: "context-compression", Enabled: false}},
		Stream:  true,
	}
	if len(p.Fallbacks) > 0 {
		body.Models = append([]string{p.Model}, p.Fallbacks...)
	}
	if p.Reasoning.Effort != "" || p.Reasoning.MaxTokens != 0 || p.Reasoning.Enabled != nil {
		body.Reasoning = &orReasoning{
			Effort: p.Reasoning.Effort, MaxTokens: p.Reasoning.MaxTokens, Enabled: p.Reasoning.Enabled,
		}
	}
	// §8: tool_choice none also sends no tools.
	if p.ToolChoice == "none" {
		body.ToolChoice = "none"
	} else {
		if p.ToolChoice != "" {
			body.ToolChoice = p.ToolChoice
		}
		for _, t := range p.Tools {
			var ot orTool
			ot.Type = "function"
			ot.Function.Name = t.Name
			ot.Function.Description = t.Description
			ot.Function.Parameters = t.Schema
			body.Tools = append(body.Tools, ot)
		}
	}
	for _, it := range items {
		m, err := toMessage(it)
		if err != nil {
			return nil, err
		}
		body.Messages = append(body.Messages, m)
	}
	if p.Cache == "auto" && AnthropicFamily(p.Model) {
		applyCacheBreakpoints(&body)
	}
	return json.Marshal(body)
}

// AnthropicFamily reports whether a model id is Anthropic-family (§8:
// explicit cache breakpoints only for those; other providers cache
// automatically).
func AnthropicFamily(modelID string) bool {
	m := strings.ToLower(modelID)
	return strings.HasPrefix(m, "anthropic/") || strings.Contains(m, "claude")
}

// applyCacheBreakpoints adds cache_control on text blocks only (§8): the
// system prompt, the last tool definition, and the newest user or tool
// boundary — at most four, here at most three.
func applyCacheBreakpoints(body *orBody) {
	cc := &orCacheControl{Type: "ephemeral"}
	mark := func(m *orMessage) {
		if s, ok := m.Content.(string); ok && s != "" {
			m.Content = []orContentBlock{{Type: "text", Text: s, CacheControl: cc}}
		}
	}
	for i := range body.Messages {
		if body.Messages[i].Role == "system" {
			mark(&body.Messages[i])
			break
		}
	}
	if len(body.Tools) > 0 {
		body.Tools[len(body.Tools)-1].CacheControl = cc
	}
	for i := len(body.Messages) - 1; i >= 0; i-- {
		if r := body.Messages[i].Role; r == "user" || r == "tool" {
			mark(&body.Messages[i])
			break
		}
	}
}

// toMessage maps one item to its OpenRouter message (pinned mapping:
// System->system, User/Notice/Summary->user, Assistant->assistant with
// tool_calls + reasoning_details, ToolResult->tool).
// contentOf keeps empty text as a nil Content so omitempty drops it,
// exactly as the pre-cache string field did.
func contentOf(text string) any {
	if text == "" {
		return nil
	}
	return text
}

func toMessage(it model.Item) (orMessage, error) {
	switch it.Kind {
	case model.KindSystem:
		return orMessage{Role: "system", Content: contentOf(it.Text)}, nil
	case model.KindUser, model.KindNotice, model.KindSummary:
		return orMessage{Role: "user", Content: contentOf(it.Text)}, nil
	case model.KindToolResult:
		return orMessage{Role: "tool", ToolCallID: it.CallID, Content: contentOf(it.Text)}, nil
	case model.KindAssistant:
		m := orMessage{Role: "assistant", Content: contentOf(it.Text)}
		for _, c := range it.Calls {
			var tc orToolCall
			tc.ID = c.ID
			tc.Type = "function"
			tc.Function.Name = c.Name
			args := string(c.Args)
			if args == "" {
				args = "{}"
			}
			tc.Function.Arguments = args
			m.ToolCalls = append(m.ToolCalls, tc)
		}
		if it.Reasoning != nil && len(it.Reasoning.Details) > 0 {
			rd, err := StripReasoningIndex(it.Reasoning.Details)
			if err != nil {
				return orMessage{}, err
			}
			m.ReasoningDetails = rd
		}
		return m, nil
	default:
		return orMessage{}, fmt.Errorf("trace/query: unknown item kind %q", it.Kind)
	}
}

// StripReasoningIndex removes the "index" key from each reasoning_details
// entry (§8: stripped before sending back). Entries without an index pass
// through byte-exact; only rebuilt entries lose key order.
func StripReasoningIndex(details json.RawMessage) (json.RawMessage, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(details, &entries); err != nil {
		return details, nil // opaque non-array detail: replay as stored
	}
	out := make([]json.RawMessage, len(entries))
	for i, e := range entries {
		if !bytes.Contains(e, []byte(`"index"`)) {
			out[i] = e
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(e, &m); err != nil {
			return nil, fmt.Errorf("trace/query: reasoning detail %d: %w", i, err)
		}
		delete(m, "index")
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return json.Marshal(out)
}
