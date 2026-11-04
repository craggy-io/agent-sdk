package config

import (
	"fmt"
	"time"
)

// LimitsPatch tightens limits: only set fields apply, by minimum (§3.2).
type LimitsPatch struct {
	USD         *float64       `json:"usd,omitempty"`
	WallClock   *time.Duration `json:"wall_clock,omitempty"`
	Steps       *int           `json:"steps,omitempty"`
	ToolCalls   *int           `json:"tool_calls,omitempty"`
	Depth       *int           `json:"depth,omitempty"`
	Concurrency *int           `json:"concurrency,omitempty"`
}

// LoopPatch overrides loop defaults: only set fields apply, last wins (§3.2).
type LoopPatch struct {
	MaxSteps         *int     `json:"max_steps,omitempty"`
	ToolResultBytes  *int     `json:"tool_result_bytes,omitempty"`
	CompactAt        *float64 `json:"compact_at,omitempty"`
	KeepRecentTokens *int     `json:"keep_recent_tokens,omitempty"`
	SummaryTokens    *int     `json:"summary_tokens,omitempty"`
}

// Layer is one §3.2 contributor in order: session (the base), workflow
// chain, definition, call site. Name is the trace source label, e.g.
// "workflow:verified-task", "def:reviewer", "call".
type Layer struct {
	Name string
	// ProfileName names the profile for agents under this layer.
	ProfileName string
	// ProfileRemap rewrites profile names for the subtree (§3.2:
	// profiles: {deep: fast}).
	ProfileRemap map[string]string
	Limits       *LimitsPatch
	Loop         *LoopPatch
	// Profiles redefines profile bindings for the subtree. Profile names
	// are resolved against the nearest table at the end (§3.2).
	Profiles map[string]Profile
}

// Effective is the resolved config for one node (§3.2), with the source
// layer recorded beside each field for the trace.
type Effective struct {
	ProfileName string            `json:"profile_name"`
	Profile     Profile           `json:"profile"`
	Limits      Limits            `json:"limits"`
	Loop        LoopDefaults      `json:"loop"`
	Sources     map[string]string `json:"sources"`
	// FellBack is true when the wanted profile did not exist and "default"
	// was substituted (§3.2: caller logs config.fallback; never fails).
	FellBack bool `json:"fell_back,omitempty"`
}

// minCap tightens a cap: zero means uncapped, otherwise the minimum wins.
func minCap[T int | float64 | time.Duration](cur T, next *T) T {
	if next == nil {
		return cur
	}
	if cur == 0 || *next < cur {
		return *next
	}
	return cur
}

// Resolve computes the effective config: effective = session ⊕ layers,
// wantProfile (from the definition or call site) last. Limits take the
// minimum of all layers; loop fields take the last set value; the profile
// name is remapped per layer, then resolved against the nearest profile
// table, falling back to "default" when unknown (§3.2).
func Resolve(base SessionConfig, layers []Layer, wantProfile string) Effective {
	eff := Effective{
		Limits:  base.Limits,
		Loop:    base.Loop,
		Sources: map[string]string{},
	}
	src := func(key, layer string) { eff.Sources[key] = layer }
	for _, k := range []string{
		"limits.usd", "limits.wall_clock", "limits.steps", "limits.tool_calls",
		"limits.depth", "limits.concurrency",
		"loop.max_steps", "loop.tool_result_bytes", "loop.compact_at",
		"loop.keep_recent_tokens", "loop.summary_tokens", "loop.budget_notice_at",
	} {
		src(k, "session")
	}

	profiles := make(map[string]Profile, len(base.Profiles))
	profileSrc := make(map[string]string, len(base.Profiles))
	for name, p := range base.Profiles {
		profiles[name] = p
		profileSrc[name] = "session"
	}

	name := wantProfile
	nameSrc := "session"
	if wantProfile != "" {
		nameSrc = "call"
	}
	for i, l := range layers {
		ln := l.Name
		if ln == "" {
			ln = fmt.Sprintf("layer%d", i)
		}
		for pn, p := range l.Profiles {
			profiles[pn] = p
			profileSrc[pn] = ln
		}
		if l.ProfileName != "" && wantProfile == "" {
			name, nameSrc = l.ProfileName, ln
		}
		if l.Limits != nil {
			p := l.Limits
			tighten := func(key string, changed bool) {
				if changed {
					src(key, ln)
				}
			}
			old := eff.Limits
			eff.Limits.USD = minCap(old.USD, p.USD)
			tighten("limits.usd", eff.Limits.USD != old.USD)
			eff.Limits.WallClock = minCap(old.WallClock, p.WallClock)
			tighten("limits.wall_clock", eff.Limits.WallClock != old.WallClock)
			eff.Limits.Steps = minCap(old.Steps, p.Steps)
			tighten("limits.steps", eff.Limits.Steps != old.Steps)
			eff.Limits.ToolCalls = minCap(old.ToolCalls, p.ToolCalls)
			tighten("limits.tool_calls", eff.Limits.ToolCalls != old.ToolCalls)
			eff.Limits.Depth = minCap(old.Depth, p.Depth)
			tighten("limits.depth", eff.Limits.Depth != old.Depth)
			eff.Limits.Concurrency = minCap(old.Concurrency, p.Concurrency)
			tighten("limits.concurrency", eff.Limits.Concurrency != old.Concurrency)
		}
		if l.Loop != nil {
			p := l.Loop
			if p.MaxSteps != nil {
				eff.Loop.MaxSteps = *p.MaxSteps
				src("loop.max_steps", ln)
			}
			if p.ToolResultBytes != nil {
				eff.Loop.ToolResultBytes = *p.ToolResultBytes
				src("loop.tool_result_bytes", ln)
			}
			if p.CompactAt != nil {
				eff.Loop.CompactAt = *p.CompactAt
				src("loop.compact_at", ln)
			}
			if p.KeepRecentTokens != nil {
				eff.Loop.KeepRecentTokens = *p.KeepRecentTokens
				src("loop.keep_recent_tokens", ln)
			}
			if p.SummaryTokens != nil {
				eff.Loop.SummaryTokens = *p.SummaryTokens
				src("loop.summary_tokens", ln)
			}
		}
	}

	if name == "" {
		name = "default"
	}
	// Remaps apply in layer order, each at most once.
	remappedBy := ""
	for i, l := range layers {
		if to, ok := l.ProfileRemap[name]; ok && to != "" {
			name = to
			remappedBy = l.Name
			if remappedBy == "" {
				remappedBy = fmt.Sprintf("layer%d", i)
			}
		}
	}
	eff.ProfileName = name
	src("profile_name", nameSrc)
	if remappedBy != "" {
		src("profile_name", remappedBy)
		eff.Sources["profile_remap"] = remappedBy
	}

	if p, ok := profiles[name]; ok {
		eff.Profile = p
		src("profile", profileSrc[name])
	} else {
		// §3.2 default (settled): unknown profile -> "default", never fail.
		eff.ProfileName = "default"
		eff.Profile = profiles["default"] // zero Profile if even default is absent
		eff.FellBack = true
		src("profile", profileSrc["default"])
		if eff.Sources["profile"] == "" {
			eff.Sources["profile"] = "session"
		}
	}
	return eff
}
