package pricesheet

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/craggy-io/agent-sdk/config"
)

// Format names this version of the sheet.
const Format = "craggy.prices@1"

// URL is where the sheet is published.
const URL = "https://prices.craggy.io/v1/sheet.json"

// The slots a sheet names a model for. Low, Med and High are general tiers;
// Decision and Audio are purpose-specific defaults. Embed is pinned because
// vectors from different models can't be compared.
const (
	Low      = "low"
	Med      = "med"
	High     = "high"
	Decision = "decision"
	Audio    = "audio"
	Embed    = "embed"
)

// Tiers are the swappable slots, cheapest first.
var Tiers = []string{Low, Med, High}

var slots = []string{Low, Med, High, Decision, Audio, Embed}

// Model is the model behind a slot, with its price per million billed units.
type Model struct {
	// ID is the provider's ID for it (OpenRouter's, e.g. anthropic/claude-sonnet-5.5).
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	InUSDPerM  float64 `json:"in_usd_per_m"`
	OutUSDPerM float64 `json:"out_usd_per_m"`
	// Context is the context window in tokens; MaxOutput the cap on one answer.
	Context   int  `json:"context,omitempty"`
	MaxOutput int  `json:"max_output,omitempty"`
	Pinned    bool `json:"pinned,omitempty"`
	// Empty means tokens; speech models may bill characters or seconds.
	PriceUnit string `json:"price_unit,omitempty"`
}

// InUSDPerToken is the input price of one token.
func (m Model) InUSDPerToken() float64 {
	if m.PriceUnit != "" {
		return 0
	}
	return m.InUSDPerM / 1e6
}

// OutUSDPerToken is the output price of one token.
func (m Model) OutUSDPerToken() float64 {
	if m.PriceUnit != "" {
		return 0
	}
	return m.OutUSDPerM / 1e6
}

// Cost is what one unit of a resource costs, e.g. gcp.run.vcpu_second.
type Cost struct {
	USD  float64 `json:"usd"`
	Unit string  `json:"unit"`
	// Source is where the price came from: "openrouter", "gcp" or "manual".
	Source string    `json:"source"`
	AsOf   time.Time `json:"as_of"`
}

// Sheet is one published price sheet.
type Sheet struct {
	Format string `json:"format"`
	// Version goes up by one with every publish; Builtin is 0.
	Version   int64            `json:"version"`
	Published time.Time        `json:"published"`
	Models    map[string]Model `json:"models"`
	Costs     map[string]Cost  `json:"costs"`
}

// Tier returns the model behind a slot. Older signed sheets predate the
// purpose-specific slots, so those use the built-in choice until republished.
func (s Sheet) Tier(slot string) Model {
	if m, ok := s.Models[slot]; ok {
		return m
	}
	if slot == Decision || slot == Audio {
		return Builtin().Models[slot]
	}
	return Model{}
}

// Cost returns a resource's cost.
func (s Sheet) Cost(key string) (Cost, bool) {
	c, ok := s.Costs[key]
	return c, ok
}

// SlotOf returns the slot whose model has this provider ID, preferring the
// cheapest tier when several share it.
func (s Sheet) SlotOf(id string) (string, bool) {
	for _, slot := range slots {
		if s.Models[slot].ID == id {
			return slot, true
		}
	}
	return "", false
}

// Profiles points agent-sdk's profiles at the tiers: fast is Low, default is
// Med, deep is High. Fallbacks are cleared, since the operator chose the
// model; everything else in the profile is kept.
func (s Sheet) Profiles(c *config.SessionConfig) {
	if c.Profiles == nil {
		c.Profiles = map[string]config.Profile{}
	}
	for name, slot := range map[string]string{"fast": Low, "default": Med, "deep": High} {
		m := s.Models[slot]
		p := c.Profiles[name]
		p.Model, p.Fallbacks = m.ID, nil
		if m.MaxOutput > 0 && (p.MaxOutputTokens == 0 || p.MaxOutputTokens > m.MaxOutput) {
			p.MaxOutputTokens = m.MaxOutput
		}
		c.Profiles[name] = p
	}
}

// Check reports every problem with the sheet, or nil.
func (s Sheet) Check() error {
	var problems []string
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }
	if s.Format != Format {
		add("format is %q, want %q", s.Format, Format)
	}
	if s.Version < 0 {
		add("version is negative")
	}
	// Decision and Audio are optional in legacy signed sheets. A reader
	// supplies their built-in defaults after verifying the old signature.
	for _, slot := range []string{Low, Med, High, Embed} {
		m, ok := s.Models[slot]
		switch {
		case !ok:
			add("no model for %s", slot)
		case m.ID == "":
			add("%s has no model id", slot)
		case m.InUSDPerM < 0 || m.OutUSDPerM < 0:
			add("%s has a negative price", slot)
		case slot == Embed && !m.Pinned:
			add("embed must be pinned")
		}
	}
	for _, slot := range []string{Decision, Audio} {
		if m, ok := s.Models[slot]; ok && (m.ID == "" || m.InUSDPerM < 0 || m.OutUSDPerM < 0) {
			add("%s needs a model id and non-negative prices", slot)
		}
	}
	for _, name := range sortedKeys(s.Models) {
		if !contains(slots, name) {
			add("unknown slot %q", name)
		}
		if unit := s.Models[name].PriceUnit; unit != "" && unit != "characters" && unit != "seconds" {
			add("%s has unknown price unit %q", name, unit)
		}
	}
	for _, key := range sortedKeys(s.Costs) {
		c := s.Costs[key]
		if c.USD < 0 || c.Unit == "" || c.Source == "" {
			add("cost %s needs a non-negative usd, a unit and a source", key)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New("pricesheet: " + strings.Join(problems, "; "))
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
