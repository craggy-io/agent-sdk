package openrouter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/craggy-io/agent-sdk/model"
)

// --- SSE chunk shapes (OpenAI-compatible streaming, §8) ---

type orErrObj struct {
	Message  string          `json:"message"`
	Code     json.RawMessage `json:"code"` // string or number
	Provider string          `json:"provider"`
	Metadata *struct {
		Raw          json.RawMessage `json:"raw"`
		ProviderName string          `json:"provider_name"`
	} `json:"metadata"`
}

type orToolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type orDelta struct {
	Content          string            `json:"content"`
	Reasoning        string            `json:"reasoning"`
	ReasoningDetails []json.RawMessage `json:"reasoning_details"`
	ToolCalls        []orToolCallDelta `json:"tool_calls"`
}

type orChoice struct {
	Index        int      `json:"index"`
	Delta        orDelta  `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
	Message      *orDelta `json:"message"` // some providers send a final message
}

// orUsage is the usage object. Fields are pointers so presence survives
// decoding: model.Usage.Reported lists what the provider returned, not
// what happened to be non-zero.
type orUsage struct {
	PromptTokens     *int     `json:"prompt_tokens"`
	CachedTokens     *int     `json:"cached_tokens"`
	CompletionTokens *int     `json:"completion_tokens"`
	Cost             *float64 `json:"cost"`
	PromptDetails    *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// toModel converts u, recording each field's presence in Reported.
// cached_tokens may arrive top-level or under prompt_tokens_details; a
// non-zero details value wins.
func (u *orUsage) toModel() model.Usage {
	var out model.Usage
	var reported []string
	set := func(dst *int, src *int, name string) {
		if src != nil {
			*dst = *src
			reported = append(reported, name)
		}
	}
	set(&out.PromptTokens, u.PromptTokens, model.UsagePromptTokens)
	var details *int
	if u.PromptDetails != nil {
		details = u.PromptDetails.CachedTokens
	}
	switch {
	case details != nil && *details != 0, details != nil && u.CachedTokens == nil:
		set(&out.CachedTokens, details, model.UsageCachedTokens)
	default:
		set(&out.CachedTokens, u.CachedTokens, model.UsageCachedTokens)
	}
	set(&out.CompletionTokens, u.CompletionTokens, model.UsageCompletionTokens)
	if u.CompletionDetails != nil {
		set(&out.ReasoningTokens, u.CompletionDetails.ReasoningTokens, model.UsageReasoningTokens)
	}
	if u.Cost != nil {
		out.CostUSD = *u.Cost
		reported = append(reported, model.UsageCostUSD)
	}
	out.Reported = reported
	return out
}

type orChunk struct {
	ID       string     `json:"id"`
	Model    string     `json:"model"`
	Provider string     `json:"provider"`
	Choices  []orChoice `json:"choices"`
	Usage    *orUsage   `json:"usage"`
	Error    *orErrObj  `json:"error"`
}

type callAcc struct {
	id, name string
	args     strings.Builder
}

// readStream consumes the SSE body. The idle timer resets on every line
// received, including ":" keep-alives (§5.3) — lines are read as they
// arrive, and any line proves the peer sent bytes.
func readStream(ctx context.Context, cancel context.CancelFunc, body io.Reader, idle time.Duration, onDelta func(model.Delta)) (model.Response, error) {
	// One channel for lines and the terminal error so EOF can never race
	// ahead of buffered lines.
	type lineMsg struct {
		line string
		err  error
	}
	lines := make(chan lineMsg, 16)
	go func() {
		r := bufio.NewReader(body)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				select {
				case lines <- lineMsg{line: line}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				select {
				case lines <- lineMsg{err: err}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()

	var (
		text       strings.Builder
		reasonText strings.Builder
		reasoning  []json.RawMessage
		calls      = map[int]*callAcc{}
		finish     string
		usage      model.Usage
		usageSeen  bool // a usage object arrived; failures carry it
		served     string
		genID      string
		provider   string
		sawDone    bool
	)
	emit := func(d model.Delta) {
		if onDelta != nil {
			onDelta(d)
		}
	}
	applyUsage := func(u *orUsage) {
		if u == nil {
			return
		}
		usage = u.toModel()
		usageSeen = true
	}
	// fail returns a failed-attempt error carrying any usage the stream
	// reported before it failed.
	fail := func(e *model.Error) error {
		if usageSeen && e.Usage == nil {
			u := usage
			e.Usage = &u
		}
		return e
	}
	applyDelta := func(d orDelta) {
		if d.Content != "" {
			text.WriteString(d.Content)
			emit(model.Delta{Kind: "text", Text: d.Content})
		}
		if d.Reasoning != "" {
			reasonText.WriteString(d.Reasoning)
			emit(model.Delta{Kind: "reasoning", Text: d.Reasoning})
		}
		for _, rd := range d.ReasoningDetails {
			reasoning = append(reasoning, rd)
			var probe struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(rd, &probe) == nil && probe.Text != "" {
				reasonText.WriteString(probe.Text)
				emit(model.Delta{Kind: "reasoning", Text: probe.Text})
			}
		}
		for _, tc := range d.ToolCalls {
			name, args := tc.Function.Name, tc.Function.Arguments
			// §8: ignore fragments with neither a name nor arguments.
			if name == "" && args == "" && tc.ID == "" {
				continue
			}
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			acc := calls[idx]
			if acc == nil {
				acc = &callAcc{}
				calls[idx] = acc
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if name != "" {
				acc.name = name
			}
			if args != "" {
				acc.args.WriteString(args)
				emit(model.Delta{Kind: "tool_args", Text: args, CallIndex: idx})
			}
		}
	}

	timer := time.NewTimer(idle)
	defer timer.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		case <-timer.C:
			cancel()
			return model.Response{}, fail(&model.Error{
				Code: "idle_timeout", Transient: true,
				Message: fmt.Sprintf("no bytes from provider for %s", idle),
			})
		case msg := <-lines:
			if msg.err != nil {
				if msg.err != io.EOF && ctx.Err() != nil {
					return model.Response{}, ctx.Err()
				}
				break loop // EOF (validators below decide if that is an error)
			}
			line := msg.line
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, ":") {
				continue // keep-alive comment; timer already reset (§8)
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				sawDone = true
				break loop
			}
			var chunk orChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return model.Response{}, fail(&model.Error{Code: "bad_chunk", Message: "undecodable SSE chunk: " + err.Error(), Raw: data})
			}
			// §8: a 200 chunk carrying a top-level error is a failed attempt.
			// Usage on the error chunk itself, if any, is kept with it.
			if chunk.Error != nil {
				applyUsage(chunk.Usage)
				return model.Response{}, fail(mapChunkError(chunk.Error))
			}
			if chunk.Model != "" {
				served = chunk.Model
			}
			if chunk.ID != "" {
				genID = chunk.ID
			}
			if chunk.Provider != "" {
				provider = chunk.Provider
			}
			// §8: the trailing usage chunk is accounting, not a finish.
			applyUsage(chunk.Usage)
			if len(chunk.Choices) == 0 {
				if chunk.Usage != nil {
					continue
				}
				// §8: empty choices is an error, never a panic.
				return model.Response{}, fail(&model.Error{Code: "empty_choices", Message: "provider chunk had no choices", Raw: data})
			}
			for _, ch := range chunk.Choices {
				applyDelta(ch.Delta)
				if ch.Message != nil {
					applyDelta(*ch.Message)
				}
				// Reasoning chunks after finish_reason are tolerated (§8):
				// we only record the reason and keep reading until [DONE].
				if ch.FinishReason != nil && *ch.FinishReason != "" {
					finish = *ch.FinishReason
				}
			}
		}
	}
	if !sawDone && finish == "" {
		// §5.3: a stream that closes without [DONE] or a finish is transient.
		return model.Response{}, fail(&model.Error{Code: "stream_closed", Transient: true, Message: "stream ended without [DONE] or finish_reason"})
	}

	item := model.Item{
		Kind:     model.KindAssistant,
		Text:     text.String(),
		ServedBy: served, // §8: the served model, not the requested one
		Finish:   mapFinish(finish),
	}
	idxs := make([]int, 0, len(calls))
	for i := range calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		acc := calls[i]
		if acc.name == "" && acc.args.Len() == 0 {
			continue // only an ID ever arrived; not a callable call
		}
		args := json.RawMessage(acc.args.String())
		item.Calls = append(item.Calls, model.ToolCall{
			ID: acc.id, Name: acc.name, Args: args, ArgsOK: json.Valid(args),
		})
	}
	if len(reasoning) > 0 || reasonText.Len() > 0 {
		r := &model.Reasoning{Text: reasonText.String()}
		if len(reasoning) > 0 {
			// Concatenated in arrival order (§8); index is stripped on
			// replay by trace/query.BuildBody.
			joined, err := json.Marshal(reasoning)
			if err == nil {
				r.Details = joined
			}
		}
		item.Reasoning = r
	}
	return model.Response{Item: item, ServedModel: served, GenerationID: genID, Provider: provider, Usage: usage}, nil
}

func mapFinish(f string) model.Finish {
	switch model.Finish(f) {
	case model.FinishStop, model.FinishToolCalls, model.FinishLength,
		model.FinishContentFilter, model.FinishError:
		return model.Finish(f)
	default:
		return model.FinishMissing
	}
}
