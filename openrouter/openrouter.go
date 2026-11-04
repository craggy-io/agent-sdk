// Package openrouter is the hand-written OpenRouter provider (§8):
// net/http plus a small SSE reader, no SDK. It implements model.Provider.
//
// The request body is built by trace/query.BuildBody so the bytes sent are
// byte-identical to trace/query.RebuildRequest for the same step (§7.1
// exact replay).
package openrouter

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/trace/query"
)

// DefaultBaseURL is the OpenRouter API root.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// DefaultIdleTimeout is the §5.3 stream idle timeout; it resets on any
// byte, including SSE ":" keep-alive comments.
const DefaultIdleTimeout = 180 * time.Second

// Client implements model.Provider against OpenRouter.
type Client struct {
	APIKey     string
	BaseURL    string // default DefaultBaseURL; tests point it at httptest
	HTTPClient *http.Client
	// IdleTimeout overrides DefaultIdleTimeout when a request carries none.
	IdleTimeout time.Duration

	mu     sync.Mutex
	models map[string]model.ModelInfo // Model() cache, filled once per client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets the API root (tests).
func WithBaseURL(u string) Option { return func(c *Client) { c.BaseURL = u } }

// WithHTTPClient sets the HTTP client used for requests.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.HTTPClient = h } }

// WithIdleTimeout sets the client-level idle timeout.
func WithIdleTimeout(d time.Duration) Option { return func(c *Client) { c.IdleTimeout = d } }

// New builds a Client for apiKey.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{APIKey: apiKey, BaseURL: DefaultBaseURL}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

// BuildRequestBody renders req as the exact OpenRouter body Sample sends
// (§8, shared with trace replay via trace/query.BuildBody).
func BuildRequestBody(req model.Request) ([]byte, error) {
	return query.BuildBody(trace.ParamsOf(req), req.Items)
}

// Sample streams one completion (§8). onDelta may be nil.
func (c *Client) Sample(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	body, err := BuildRequestBody(req)
	if err != nil {
		return model.Response{}, err
	}
	idle := req.IdleTimeout
	if idle <= 0 {
		idle = c.IdleTimeout
	}
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return model.Response{}, err
	}
	hreq.Header.Set("Authorization", "Bearer "+c.APIKey)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient().Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return model.Response{}, ctx.Err()
		}
		return model.Response{}, &model.Error{Code: "network_error", Message: err.Error(), Transient: true}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return model.Response{}, MapHTTPError(resp.StatusCode, raw, resp.Header.Get("Retry-After"))
	}
	return readStream(ctx, cancel, resp.Body, idle, onDelta)
}
