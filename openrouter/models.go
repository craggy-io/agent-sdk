package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/craggy-io/agent-sdk/model"
)

type orModel struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	Pricing       struct {
		Prompt     string `json:"prompt"`     // USD per token, as a string
		Completion string `json:"completion"` // USD per token, as a string
	} `json:"pricing"`
	TopProvider struct {
		ContextLength int `json:"context_length"`
	} `json:"top_provider"`
}

// Model returns model info from GET /models (§8), cached per client: the
// list is fetched once and later lookups do not hit the network.
func (c *Client) Model(ctx context.Context, id string) (model.ModelInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.models == nil {
		if err := c.fetchModelsLocked(ctx); err != nil {
			return model.ModelInfo{}, err
		}
	}
	m, ok := c.models[id]
	if !ok {
		return model.ModelInfo{}, fmt.Errorf("openrouter: unknown model %q", id)
	}
	return m, nil
}

func (c *Client) fetchModelsLocked(ctx context.Context) error {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/models", nil)
	if err != nil {
		return err
	}
	hreq.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.httpClient().Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &model.Error{Code: "network_error", Message: err.Error(), Transient: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return MapHTTPError(resp.StatusCode, raw, resp.Header.Get("Retry-After"))
	}
	var list struct {
		Data []orModel `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("openrouter: decode /models: %w", err)
	}
	out := make(map[string]model.ModelInfo, len(list.Data))
	for _, m := range list.Data {
		info := model.ModelInfo{ID: m.ID, ContextWindow: m.ContextLength}
		if info.ContextWindow == 0 {
			info.ContextWindow = m.TopProvider.ContextLength
		}
		info.InputPricePerToken, _ = strconv.ParseFloat(m.Pricing.Prompt, 64)
		info.OutputPricePerToken, _ = strconv.ParseFloat(m.Pricing.Completion, 64)
		out[m.ID] = info
	}
	c.models = out
	return nil
}
