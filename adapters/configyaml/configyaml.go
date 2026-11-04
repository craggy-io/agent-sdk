// Package configyaml decodes session configuration from YAML for hosts
// that keep config in files. The core config package is Go values only;
// this adapter overlays a YAML document onto config.Defaults.
package configyaml

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/model"
)

// sizeValue accepts an int or a size string ("32KiB") in YAML.
type sizeValue int

func (v *sizeValue) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!int" {
		var n int
		if err := node.Decode(&n); err != nil {
			return err
		}
		*v = sizeValue(n)
		return nil
	}
	n, err := config.ParseSize(node.Value)
	if err != nil {
		return err
	}
	*v = sizeValue(n)
	return nil
}

// durationValue accepts a duration string ("30m") or seconds as a number.
type durationValue time.Duration

func (v *durationValue) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!int" || node.Tag == "!!float" {
		var f float64
		if err := node.Decode(&f); err != nil {
			return err
		}
		*v = durationValue(time.Duration(f * float64(time.Second)))
		return nil
	}
	d, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("config: bad duration %q: %w", node.Value, err)
	}
	*v = durationValue(d)
	return nil
}

// noticeFractions accepts loop.budget_notice_at: fractions strictly
// between 0 and 1, or 0 / [] / [0] to disable. Ascending and unique.
type noticeFractions []float64

func (v *noticeFractions) UnmarshalYAML(node *yaml.Node) error {
	var fs []float64
	if node.Kind == yaml.ScalarNode {
		var f float64
		if err := node.Decode(&f); err != nil {
			return fmt.Errorf("config: loop.budget_notice_at: want a list of fractions: %w", err)
		}
		fs = []float64{f}
	} else if err := node.Decode(&fs); err != nil {
		return fmt.Errorf("config: loop.budget_notice_at: want a list of fractions: %w", err)
	}
	if len(fs) == 1 && fs[0] == 0 {
		fs = nil
	}
	out := []float64{}
	for _, f := range fs {
		if f <= 0 || f >= 1 {
			return fmt.Errorf("config: loop.budget_notice_at: %v is not a fraction between 0 and 1", f)
		}
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	*v = out
	return nil
}

// Raw file shapes: pointer fields record presence so Parse can overlay a
// file onto Defaults without zeroing unspecified settings.

type rawProfile struct {
	Model             *string              `yaml:"model"`
	Fallbacks         []string             `yaml:"fallbacks"`
	Reasoning         *model.ReasoningCfg  `yaml:"reasoning"`
	MaxOutputTokens   *int                 `yaml:"max_output_tokens"`
	ParallelToolCalls *bool                `yaml:"parallel_tool_calls"`
	Provider          *model.ProviderPrefs `yaml:"provider"`
	Cache             *string              `yaml:"cache"`
	IdleTimeout       *durationValue       `yaml:"idle_timeout"`
}

type rawLimits struct {
	USD         *float64       `yaml:"usd"`
	WallClock   *durationValue `yaml:"wall_clock"`
	Steps       *int           `yaml:"steps"`
	ToolCalls   *int           `yaml:"tool_calls"`
	Depth       *int           `yaml:"depth"`
	Concurrency *int           `yaml:"concurrency"`
}

type rawLoop struct {
	MaxSteps         *int             `yaml:"max_steps"`
	ToolResultBytes  *sizeValue       `yaml:"tool_result_bytes"`
	CompactAt        *float64         `yaml:"compact_at"`
	KeepRecentTokens *int             `yaml:"keep_recent_tokens"`
	SummaryTokens    *int             `yaml:"summary_tokens"`
	BudgetNoticeAt   *noticeFractions `yaml:"budget_notice_at"`
}

type rawInput struct {
	QueueMode  *string    `yaml:"queue_mode"`
	MaxPending *int       `yaml:"max_pending"`
	MaxBytes   *sizeValue `yaml:"max_bytes"`
}

type rawEvents struct {
	PersistDeltas    *bool      `yaml:"persist_deltas"`
	RingBytes        *sizeValue `yaml:"ring_bytes"`
	SubscriberEvents *int       `yaml:"subscriber_events"`
}

type rawFile struct {
	Profiles map[string]rawProfile `yaml:"profiles"`
	Limits   *rawLimits            `yaml:"limits"`
	Loop     *rawLoop              `yaml:"loop"`
	Input    *rawInput             `yaml:"input"`
	Events   *rawEvents            `yaml:"events"`
}

func set[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func (rp rawProfile) toProfile() config.Profile {
	var p config.Profile
	set(&p.Model, rp.Model)
	p.Fallbacks = rp.Fallbacks
	set(&p.Reasoning, rp.Reasoning)
	set(&p.MaxOutputTokens, rp.MaxOutputTokens)
	set(&p.ParallelToolCalls, rp.ParallelToolCalls)
	set(&p.Provider, rp.Provider)
	set(&p.Cache, rp.Cache)
	if rp.IdleTimeout != nil {
		p.IdleTimeout = time.Duration(*rp.IdleTimeout)
	}
	return p
}

// Parse decodes session YAML over config.Defaults: unspecified sections
// keep their default values; named profiles replace same-named defaults
// wholesale (a profile is a model binding) while new names are added.
func Parse(data []byte) (*config.SessionConfig, error) {
	var raw rawFile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	cfg := config.Defaults()
	for name, rp := range raw.Profiles {
		cfg.Profiles[name] = rp.toProfile()
	}
	if r := raw.Limits; r != nil {
		set(&cfg.Limits.USD, r.USD)
		if r.WallClock != nil {
			cfg.Limits.WallClock = time.Duration(*r.WallClock)
		}
		set(&cfg.Limits.Steps, r.Steps)
		set(&cfg.Limits.ToolCalls, r.ToolCalls)
		set(&cfg.Limits.Depth, r.Depth)
		set(&cfg.Limits.Concurrency, r.Concurrency)
	}
	if r := raw.Loop; r != nil {
		set(&cfg.Loop.MaxSteps, r.MaxSteps)
		if r.ToolResultBytes != nil {
			cfg.Loop.ToolResultBytes = int(*r.ToolResultBytes)
		}
		set(&cfg.Loop.CompactAt, r.CompactAt)
		set(&cfg.Loop.KeepRecentTokens, r.KeepRecentTokens)
		set(&cfg.Loop.SummaryTokens, r.SummaryTokens)
		if r.BudgetNoticeAt != nil {
			cfg.Loop.BudgetNoticeAt = []float64(*r.BudgetNoticeAt)
		}
	}
	if r := raw.Input; r != nil {
		set(&cfg.Input.QueueMode, r.QueueMode)
		set(&cfg.Input.MaxPending, r.MaxPending)
		if r.MaxBytes != nil {
			cfg.Input.MaxBytes = int(*r.MaxBytes)
		}
	}
	if r := raw.Events; r != nil {
		set(&cfg.Events.PersistDeltas, r.PersistDeltas)
		if r.RingBytes != nil {
			cfg.Events.RingBytes = int(*r.RingBytes)
		}
		set(&cfg.Events.SubscriberEvents, r.SubscriberEvents)
	}
	return &cfg, nil
}

// Load reads and parses a session config file.
func Load(path string) (*config.SessionConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}
