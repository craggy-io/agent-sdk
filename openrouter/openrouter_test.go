package openrouter_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/openrouter"
)

// capture records the last request body and serves scripted SSE.
type capture struct {
	body []byte
	auth string
}

func sseServer(t *testing.T, cap *capture, script string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		cap.body, _ = io.ReadAll(r.Body)
		cap.auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, script)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func baseRequest() model.Request {
	return model.Request{
		Model:     "openai/gpt-5.2",
		Fallbacks: []string{"anthropic/claude-sonnet-5.5"},
		Items: []model.Item{
			{Seq: 0, Kind: model.KindSystem, Text: "You are helpful."},
			{Seq: 1, Kind: model.KindUser, Text: "do it"},
		},
		Tools: []model.Tool{{
			Name: "fs.read", Description: "Read a file", Effect: "read-only",
			Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
		Reasoning:         model.ReasoningCfg{Effort: "medium"},
		MaxOutputTokens:   16000,
		ParallelToolCalls: true,
		ToolChoice:        "auto",
		Provider:          model.ProviderPrefs{RequireParameters: true, DataCollection: "deny"},
		Cache:             "auto",
		SessionID:         "sess-1",
	}
}

const simpleScript = ": keepalive\n\n" +
	`data: {"id":"gen-1","model":"openai/gpt-5.2","provider":"OpenAI","choices":[{"index":0,"delta":{"content":"hel"}}]}` + "\n\n" +
	`data: {"id":"gen-1","model":"openai/gpt-5.2","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}` + "\n\n" +
	`data: {"id":"gen-1","model":"openai/gpt-5.2","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"cost":0.002,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":1}}}` + "\n\n" +
	"data: [DONE]\n\n"

// §8 request body: models fallbacks, provider block, plugins disabled,
// session_id, reasoning/max_tokens/tools/parallel_tool_calls sent.
func TestRequestBodyAndSimpleStream(t *testing.T) {
	cap := &capture{}
	srv := sseServer(t, cap, simpleScript)
	c := openrouter.New("test-key", openrouter.WithBaseURL(srv.URL))
	var deltas []model.Delta
	resp, err := c.Sample(context.Background(), baseRequest(), func(d model.Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if cap.auth != "Bearer test-key" {
		t.Errorf("auth=%q", cap.auth)
	}
	var body map[string]any
	if err := json.Unmarshal(cap.body, &body); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(body["models"]) != "[openai/gpt-5.2 anthropic/claude-sonnet-5.5]" {
		t.Errorf("models=%v", body["models"])
	}
	prov, _ := body["provider"].(map[string]any)
	if prov["require_parameters"] != true || prov["data_collection"] != "deny" {
		t.Errorf("provider=%v", prov)
	}
	plugins, _ := body["plugins"].([]any)
	if len(plugins) != 1 || plugins[0].(map[string]any)["id"] != "context-compression" || plugins[0].(map[string]any)["enabled"] != false {
		t.Errorf("plugins=%v", plugins)
	}
	if body["session_id"] != "sess-1" || body["stream"] != true || body["parallel_tool_calls"] != true {
		t.Errorf("session/stream/parallel: %v %v %v", body["session_id"], body["stream"], body["parallel_tool_calls"])
	}
	if body["max_tokens"] != float64(16000) {
		t.Errorf("max_tokens=%v", body["max_tokens"])
	}
	if r, _ := body["reasoning"].(map[string]any); r["effort"] != "medium" {
		t.Errorf("reasoning=%v", body["reasoning"])
	}
	if tools, _ := body["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools=%v", body["tools"])
	}
	// Non-Anthropic requested model: no cache_control anywhere (§8).
	if strings.Contains(string(cap.body), "cache_control") {
		t.Errorf("cache_control sent for openai model: %s", cap.body)
	}
	// Stream assembly + usage-only final chunk is accounting, not a finish.
	if resp.Item.Text != "hello" || resp.Item.Finish != model.FinishStop || resp.ServedModel != "openai/gpt-5.2" || resp.GenerationID != "gen-1" || resp.Provider != "OpenAI" {
		t.Errorf("resp item=%+v served=%q gen=%q provider=%q", resp.Item, resp.ServedModel, resp.GenerationID, resp.Provider)
	}
	wantUsage := model.Usage{PromptTokens: 11, CachedTokens: 4, CompletionTokens: 3, ReasoningTokens: 1, CostUSD: 0.002,
		Reported: []string{"prompt_tokens", "cached_tokens", "completion_tokens", "reasoning_tokens", "cost_usd"}}
	if !reflect.DeepEqual(resp.Usage, wantUsage) {
		t.Errorf("usage=%+v", resp.Usage)
	}
	if len(deltas) != 2 || deltas[0].Text != "hel" || deltas[1].Text != "lo" {
		t.Errorf("deltas=%v", deltas)
	}
	// Byte-exactness with the trace rebuild path (§7.1): Sample must send
	// exactly BuildRequestBody's bytes.
	want, err := openrouter.BuildRequestBody(baseRequest())
	if err != nil || string(cap.body) != string(want) {
		t.Errorf("sent body != BuildRequestBody\nsent: %s\nwant: %s err=%v", cap.body, want, err)
	}
}

// §8: tool_choice none sends no tools.
func TestToolChoiceNoneOmitsTools(t *testing.T) {
	cap := &capture{}
	srv := sseServer(t, cap, simpleScript)
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	req := baseRequest()
	req.ToolChoice = "none"
	if _, err := c.Sample(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(cap.body, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["tools"]; ok {
		t.Errorf("tools present under tool_choice none: %s", cap.body)
	}
	if string(body["tool_choice"]) != `"none"` {
		t.Errorf("tool_choice=%s", body["tool_choice"])
	}
}

// §8: unset provider prefs default require_parameters to true.
func TestRequireParametersDefaultsTrue(t *testing.T) {
	cap := &capture{}
	srv := sseServer(t, cap, simpleScript)
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	req := baseRequest()
	req.Provider = model.ProviderPrefs{}
	if _, err := c.Sample(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cap.body), `"require_parameters":true`) {
		t.Errorf("require_parameters not defaulted true: %s", cap.body)
	}
}

// §8 caching: anthropic/* with cache=auto gets breakpoints on the system
// prompt, the last tool and the newest user/tool boundary (≤4); other
// models (previous test) and cache!=auto get none.
func TestCacheControlAnthropic(t *testing.T) {
	cap := &capture{}
	srv := sseServer(t, cap, simpleScript)
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	req := baseRequest()
	req.Model = "anthropic/claude-sonnet-5.5"
	req.Fallbacks = nil
	req.Tools = append(req.Tools, model.Tool{Name: "fs.write", Effect: "mutating", Schema: json.RawMessage(`{"type":"object"}`)})
	req.Items = append(req.Items,
		model.Item{Seq: 2, Kind: model.KindAssistant, ServedBy: "anthropic/claude-sonnet-5.5", Finish: model.FinishToolCalls,
			Calls: []model.ToolCall{{ID: "call_1", Name: "fs.read", Args: json.RawMessage(`{}`), ArgsOK: true}}},
		model.Item{Seq: 3, Kind: model.KindToolResult, CallID: "call_1", Text: "file body", Settled: model.SettleRan},
	)
	if _, err := c.Sample(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(cap.body), "cache_control"); n != 3 {
		t.Fatalf("cache_control count=%d, want 3 (system, last tool, tool boundary): %s", n, cap.body)
	}
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(cap.body, &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body.Messages[0].Content), "cache_control") {
		t.Errorf("system message lacks breakpoint: %s", body.Messages[0].Content)
	}
	last := body.Messages[len(body.Messages)-1]
	if last.Role != "tool" || !strings.Contains(string(last.Content), "cache_control") {
		t.Errorf("newest tool boundary lacks breakpoint: %s", last.Content)
	}
	if len(body.Tools[0].CacheControl) != 0 || len(body.Tools[1].CacheControl) == 0 {
		t.Errorf("tool breakpoints: first=%s last=%s", body.Tools[0].CacheControl, body.Tools[1].CacheControl)
	}
	// cache!=auto: no breakpoints even for Anthropic.
	req.Cache = ""
	if _, err := c.Sample(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cap.body), "cache_control") {
		t.Errorf("cache_control sent with cache unset: %s", cap.body)
	}
}

// §8: tool calls assembled by index; empty fragments ignored; reasoning
// after finish tolerated; ServedBy from the response model.
func TestToolCallAssemblyAndReasoningAfterFinish(t *testing.T) {
	script := `data: {"id":"g2","model":"openai/gpt-5.2-2026","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"fs.write","arguments":"{\"b\":"}}]}}]}` + "\n\n" +
		`data: {"id":"g2","model":"openai/gpt-5.2-2026","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"fs.read","arguments":"{\"path\":"}},{"index":0,"function":{}},{"index":1,"function":{"arguments":"2}"}}]}}]}` + "\n\n" +
		`data: {"id":"g2","model":"openai/gpt-5.2-2026","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: {"id":"g2","model":"openai/gpt-5.2-2026","choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.text","text":"late thought","index":0}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	cap := &capture{}
	srv := sseServer(t, cap, script)
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	resp, err := c.Sample(context.Background(), baseRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Item.Finish != model.FinishToolCalls || resp.Item.ServedBy != "openai/gpt-5.2-2026" {
		t.Fatalf("finish=%q served=%q", resp.Item.Finish, resp.Item.ServedBy)
	}
	if len(resp.Item.Calls) != 2 {
		t.Fatalf("calls=%v", resp.Item.Calls)
	}
	c0, c1 := resp.Item.Calls[0], resp.Item.Calls[1]
	if c0.ID != "c1" || c0.Name != "fs.read" || string(c0.Args) != `{"path":"a.txt"}` || !c0.ArgsOK {
		t.Errorf("call0=%+v args=%s", c0, c0.Args)
	}
	if c1.ID != "c2" || c1.Name != "fs.write" || string(c1.Args) != `{"b":2}` || !c1.ArgsOK {
		t.Errorf("call1=%+v args=%s", c1, c1.Args)
	}
	if resp.Item.Reasoning == nil || resp.Item.Reasoning.Text != "late thought" || !strings.Contains(string(resp.Item.Reasoning.Details), "late thought") {
		t.Errorf("reasoning=%+v", resp.Item.Reasoning)
	}
	// §8: reasoning_details are replayed with "index" stripped. Send the
	// assistant item back and check the next request body.
	req := baseRequest()
	item := resp.Item
	item.Seq = 2
	req.Items = append(req.Items, item, model.Item{Seq: 3, Kind: model.KindUser, Text: "next"})
	if _, err := c.Sample(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cap.body), "reasoning_details") || strings.Contains(string(cap.body), `"index"`) {
		t.Errorf("replay body has unstripped/missing reasoning_details: %s", cap.body)
	}
}

// §8: a 200 chunk carrying top-level error is a failed attempt.
func TestErrorChunkWith200(t *testing.T) {
	script := `data: {"id":"g3","error":{"code":429,"message":"rate limited","metadata":{"provider_name":"SomeProvider","raw":"slow down"}}}` + "\n\n"
	cap := &capture{}
	srv := sseServer(t, cap, script)
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	_, err := c.Sample(context.Background(), baseRequest(), nil)
	var merr *model.Error
	if !errors.As(err, &merr) {
		t.Fatalf("err=%T %v, want *model.Error", err, err)
	}
	if merr.Status != 429 || !merr.Transient || merr.Provider != "SomeProvider" || merr.Raw != "slow down" || merr.Message != "rate limited" {
		t.Errorf("mapped=%+v", merr)
	}
}

// Usage.Reported lists the fields the provider returned (a reported
// zero included) and leaves out the ones it did not.
func TestUsagePresence(t *testing.T) {
	script := `data: {"id":"g","model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"g","model":"m","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := sseServer(t, &capture{}, script)
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	resp, err := c.Sample(context.Background(), baseRequest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := model.Usage{CompletionTokens: 2, Reported: []string{"prompt_tokens", "cached_tokens", "completion_tokens"}}
	if !reflect.DeepEqual(resp.Usage, want) {
		t.Errorf("usage=%+v, want %+v", resp.Usage, want)
	}
	// No usage object at all: presence unknown (nil), not "none reported".
	srv2 := sseServer(t, &capture{}, `data: {"id":"g","model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	resp2, err := openrouter.New("k", openrouter.WithBaseURL(srv2.URL)).Sample(context.Background(), baseRequest(), nil)
	if err != nil || resp2.Usage.Reported != nil {
		t.Errorf("no-usage stream: usage=%+v err=%v", resp2.Usage, err)
	}
}

// Usage reported before or with a failure travels on the error.
func TestUsageOnFailedAttempt(t *testing.T) {
	// On the error chunk itself.
	srv := sseServer(t, &capture{}, `data: {"id":"g","error":{"code":502,"message":"upstream died"},"usage":{"prompt_tokens":40,"cost":0.001}}`+"\n\n")
	_, err := openrouter.New("k", openrouter.WithBaseURL(srv.URL)).Sample(context.Background(), baseRequest(), nil)
	var merr *model.Error
	if !errors.As(err, &merr) || merr.Usage == nil || merr.Usage.PromptTokens != 40 ||
		!reflect.DeepEqual(merr.Usage.Reported, []string{"prompt_tokens", "cost_usd"}) {
		t.Fatalf("error chunk: err=%v usage=%+v", err, merr)
	}
	// A usage chunk, then the stream closes without a finish.
	srv2 := sseServer(t, &capture{}, `data: {"id":"g","model":"m","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":1,"cost":0.0001}}`+"\n\n")
	_, err = openrouter.New("k", openrouter.WithBaseURL(srv2.URL)).Sample(context.Background(), baseRequest(), nil)
	if !errors.As(err, &merr) || merr.Code != "stream_closed" || merr.Usage == nil || merr.Usage.CompletionTokens != 1 {
		t.Fatalf("stream_closed: err=%v usage=%+v", err, merr.Usage)
	}
	// Non-200 body: OpenRouter documents no usage, so none is invented...
	if e := openrouter.MapHTTPError(429, []byte(`{"error":{"message":"x","code":429}}`), ""); e.Usage != nil {
		t.Errorf("usage invented on plain error body: %+v", e.Usage)
	}
	// ...but a body that carries one keeps it.
	e := openrouter.MapHTTPError(400, []byte(`{"error":{"message":"x","code":400},"usage":{"prompt_tokens":12}}`), "")
	if e.Usage == nil || e.Usage.PromptTokens != 12 || !reflect.DeepEqual(e.Usage.Reported, []string{"prompt_tokens"}) {
		t.Errorf("error body usage = %+v", e.Usage)
	}
}

// §8: empty choices is an error, never a panic.
func TestEmptyChoicesIsError(t *testing.T) {
	cap := &capture{}
	srv := sseServer(t, cap, `data: {"id":"g4","model":"m","choices":[]}`+"\n\n"+"data: [DONE]\n\n")
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	_, err := c.Sample(context.Background(), baseRequest(), nil)
	var merr *model.Error
	if !errors.As(err, &merr) || merr.Code != "empty_choices" {
		t.Fatalf("err=%v, want empty_choices *model.Error", err)
	}
}

// §5.3: stream closing without [DONE] or a finish is transient.
func TestStreamClosedWithoutFinishIsTransient(t *testing.T) {
	cap := &capture{}
	srv := sseServer(t, cap, `data: {"id":"g5","model":"m","choices":[{"index":0,"delta":{"content":"part"}}]}`+"\n\n")
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	_, err := c.Sample(context.Background(), baseRequest(), nil)
	var merr *model.Error
	if !errors.As(err, &merr) || merr.Code != "stream_closed" || !merr.Transient {
		t.Fatalf("err=%v, want transient stream_closed", err)
	}
}

// §5.3: idle timeout resets on keep-alive bytes but fires on silence.
func TestIdleTimeoutOnSilence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			io.WriteString(w, ": hello\n\n")
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	req := baseRequest()
	req.IdleTimeout = 150 * time.Millisecond
	start := time.Now()
	_, err := c.Sample(context.Background(), req, nil)
	var merr *model.Error
	if !errors.As(err, &merr) || merr.Code != "idle_timeout" || !merr.Transient {
		t.Fatalf("err=%v, want transient idle_timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("idle timeout took too long: %s", time.Since(start))
	}
}

// §8 error mapping table over HTTP status + body + Retry-After.
func TestHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		transient  bool
		ctxExc     bool
		code       string
		retry      time.Duration
	}{
		{"408", 408, `{"error":{"message":"timeout","code":408}}`, "", true, false, "408", 0},
		{"429 retry-after secs", 429, `{"error":{"message":"rate limited","code":429}}`, "7", true, false, "429", 7 * time.Second},
		{"502", 502, `{"error":{"message":"bad gateway","code":502}}`, "", true, false, "502", 0},
		{"503", 503, `{"error":{"message":"unavailable","code":503}}`, "", true, false, "503", 0},
		{"401 not transient", 401, `{"error":{"message":"bad key","code":401}}`, "", false, false, "401", 0},
		{"400 generic not transient", 400, `{"error":{"message":"bad param","code":400}}`, "", false, false, "400", 0},
		{"400 code form", 400, `{"error":{"message":"too long","code":"context_length_exceeded"}}`, "", false, true, "context_length_exceeded", 0},
		{"400 message form", 400, `{"error":{"message":"This model's maximum context length is 200000 tokens","code":400}}`, "", false, true, "400", 0},
		{"400 raw form", 400, `{"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"prompt is too long: 250000 tokens","provider_name":"Anthropic"}}}`, "", false, true, "400", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
			_, err := c.Sample(context.Background(), baseRequest(), nil)
			var merr *model.Error
			if !errors.As(err, &merr) {
				t.Fatalf("err=%T %v", err, err)
			}
			if merr.Status != tc.status || merr.Transient != tc.transient || merr.ContextExceeded != tc.ctxExc || merr.Code != tc.code {
				t.Errorf("mapped=%+v, want status=%d transient=%v ctx=%v code=%q", merr, tc.status, tc.transient, tc.ctxExc, tc.code)
			}
			if tc.retry > 0 && merr.RetryAfter != tc.retry {
				t.Errorf("RetryAfter=%s want %s", merr.RetryAfter, tc.retry)
			}
		})
	}
	// HTTP-date Retry-After parses to a positive duration.
	e := openrouter.MapHTTPError(429, []byte(`{"error":{"message":"x","code":429}}`), time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
	if e.RetryAfter <= time.Minute || e.RetryAfter > 2*time.Minute {
		t.Errorf("date RetryAfter=%s", e.RetryAfter)
	}
}

// §8: Model() serves from a per-client cache after one /models fetch.
func TestModelCache(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"data":[{"id":"openai/gpt-5.2","context_length":400000,"pricing":{"prompt":"0.000001","completion":"0.000004"}},{"id":"anthropic/claude-sonnet-5.5","top_provider":{"context_length":200000},"pricing":{"prompt":"0.000003","completion":"0.000015"}}]}`)
	}))
	defer srv.Close()
	c := openrouter.New("k", openrouter.WithBaseURL(srv.URL))
	ctx := context.Background()
	m1, err := c.Model(ctx, "openai/gpt-5.2")
	if err != nil {
		t.Fatal(err)
	}
	if m1.ContextWindow != 400000 || m1.InputPricePerToken != 0.000001 || m1.OutputPricePerToken != 0.000004 {
		t.Errorf("m1=%+v", m1)
	}
	m2, err := c.Model(ctx, "anthropic/claude-sonnet-5.5")
	if err != nil || m2.ContextWindow != 200000 {
		t.Errorf("m2=%+v err=%v", m2, err)
	}
	if _, err := c.Model(ctx, "nope/model"); err == nil {
		t.Error("unknown model: want error")
	}
	if hits.Load() != 1 {
		t.Errorf("/models fetched %d times, want 1 (cached)", hits.Load())
	}
}
