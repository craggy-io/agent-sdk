package agentcap

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/memory"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/capability-services/registry"
)

// llmCaps expose the host's model provider directly: provider-neutral
// one-shot sampling for scripts and tools that need a completion
// without a full agent loop. Profiles resolve through the host config,
// so callers name profiles, never models (a model= override exists for
// experiments).
func (s *service) llmCaps() []registry.Capability {
	return []registry.Capability{
		registry.Func("llm.sample", "Sample the model once",
			func(ctx context.Context, in struct {
				Profile         string `json:"profile,omitempty"`
				Model           string `json:"model,omitempty"`
				System          string `json:"system,omitempty"`
				Prompt          string `json:"prompt"`
				MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
			}) (struct {
				Text        string      `json:"text"`
				Model       string      `json:"model"`
				ServedModel string      `json:"served_model,omitempty"`
				Profile     string      `json:"profile"`
				Usage       model.Usage `json:"usage"`
			}, error) {
				var out struct {
					Text        string      `json:"text"`
					Model       string      `json:"model"`
					ServedModel string      `json:"served_model,omitempty"`
					Profile     string      `json:"profile"`
					Usage       model.Usage `json:"usage"`
				}
				prov := s.h.Provider()
				if prov == nil {
					return out, registry.Errorf(registry.Unavailable, "no model provider configured")
				}
				if in.Prompt == "" {
					return out, invalid("prompt is required")
				}
				eff := config.Resolve(s.h.Config(), nil, in.Profile)
				p := eff.Profile
				modelID := p.Model
				if in.Model != "" {
					modelID = in.Model
				}
				maxTok := p.MaxOutputTokens
				if in.MaxOutputTokens > 0 {
					maxTok = in.MaxOutputTokens
				}
				var items []model.Item
				if in.System != "" {
					items = append(items, model.Item{Kind: model.KindSystem, Text: in.System})
				}
				items = append(items, model.Item{Kind: model.KindUser, Text: in.Prompt})
				resp, err := prov.Sample(ctx, model.Request{
					Model: modelID, Fallbacks: p.Fallbacks, Items: items,
					Reasoning: p.Reasoning, MaxOutputTokens: maxTok,
					Provider: p.Provider, Cache: p.Cache, IdleTimeout: p.IdleTimeout,
					SessionID: s.h.ID(),
				}, nil)
				if err != nil {
					return out, coded(err)
				}
				out.Text = resp.Item.Text
				out.Model = modelID
				out.ServedModel = resp.ServedModel
				out.Profile = eff.ProfileName
				out.Usage = resp.Usage
				return out, nil
			}, registry.WithEffects(registry.OpenWorld),
			registry.Describe("One provider sample with no tools and no loop. For agent behaviour use agents.run or workflows.")),
		registry.Func("llm.model", "Describe one model",
			func(ctx context.Context, in struct {
				ID string `json:"id"`
			}) (model.ModelInfo, error) {
				prov := s.h.Provider()
				if prov == nil {
					return model.ModelInfo{}, registry.Errorf(registry.Unavailable, "no model provider configured")
				}
				info, err := prov.Model(ctx, in.ID)
				return info, coded(err)
			}, registry.WithEffects(registry.ReadOnly|registry.OpenWorld)),
		registry.Func("llm.profiles", "List configured profiles",
			func(_ context.Context, _ struct{}) ([]string, error) {
				names := []string{}
				for n := range s.h.Config().Profiles {
					names = append(names, n)
				}
				sort.Strings(names)
				return names, nil
			}, registry.WithEffects(registry.ReadOnly)),
	}
}

// memoryCaps expose the memory store. Values are arbitrary JSON;
// namespaces let hosts scope memory per agent, thread or project.
func (s *service) memoryCaps() []registry.Capability {
	ro := registry.WithEffects(registry.ReadOnly)
	return []registry.Capability{
		registry.Func("memory.put", "Store a value",
			func(ctx context.Context, in struct {
				Namespace string          `json:"namespace"`
				Key       string          `json:"key"`
				Value     json.RawMessage `json:"value"`
			}) (memory.Entry, error) {
				e, err := s.mem.Put(ctx, in.Namespace, in.Key, in.Value)
				if err != nil {
					return memory.Entry{}, invalid("%v", err)
				}
				return e, nil
			}, registry.WithEffects(registry.Idempotent)),
		registry.Func("memory.get", "Read a value",
			func(ctx context.Context, in struct {
				Namespace string `json:"namespace"`
				Key       string `json:"key"`
			}) (memory.Entry, error) {
				e, err := s.mem.Get(ctx, in.Namespace, in.Key)
				if err != nil {
					return memory.Entry{}, registry.WrapCode(registry.NotFound, err)
				}
				return e, nil
			}, ro),
		registry.Func("memory.list", "List a namespace",
			func(ctx context.Context, in struct {
				Namespace string `json:"namespace"`
			}) ([]memory.Entry, error) {
				list, err := s.mem.List(ctx, in.Namespace)
				if list == nil {
					list = []memory.Entry{}
				}
				return list, coded(err)
			}, ro),
		registry.Func("memory.delete", "Delete a value",
			func(ctx context.Context, in struct {
				Namespace string `json:"namespace"`
				Key       string `json:"key"`
			}) (map[string]any, error) {
				if err := s.mem.Delete(ctx, in.Namespace, in.Key); err != nil {
					return nil, coded(err)
				}
				return map[string]any{"deleted": true}, nil
			}, registry.WithEffects(registry.Idempotent|registry.Destructive)),
	}
}
