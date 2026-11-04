package model

import "path"

// FilterSpec is a tool filter (§4.3): And(Or(Match(include)), Not(Match(exclude)),
// Or(EffectAtMost(MaxEffect), Match(allow))). Empty Include admits all names.
// Allow names tools exempt from MaxEffect only: Include must still admit them
// and Exclude still wins. It lives in model so def and session can share it
// without an import cycle.
type FilterSpec struct {
	Include   []string `json:"include,omitempty" yaml:"include,omitempty"`
	Exclude   []string `json:"exclude,omitempty" yaml:"exclude,omitempty"`
	MaxEffect string   `json:"max_effect,omitempty" yaml:"max_effect,omitempty"`
	Allow     []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	Inherit   bool     `json:"inherit,omitempty" yaml:"inherit,omitempty"`
}

// EffectRank orders effects: read-only < idempotent < mutating. "long" is
// treated as mutating for ordering. Empty or unknown effects fail closed as
// mutating, so an unclassified tool is never admitted by a narrow filter.
func EffectRank(effect string) int {
	switch effect {
	case "read-only":
		return 0
	case "idempotent":
		return 1
	default: // mutating, long, control, admin, "", unknown
		return 2
	}
}

// matchName reports whether name matches a glob like "storage.*" (§4.1).
func matchName(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	if err != nil {
		return pattern == name
	}
	return ok
}

func anyMatch(patterns []string, name string) bool {
	for _, p := range patterns {
		if matchName(p, name) {
			return true
		}
	}
	return false
}

// Allows reports whether the filter admits tool (§4.3). MaxEffect "any" or ""
// admits all effects; an unrecognised MaxEffect admits nothing (fail closed)
// except the tools Allow names.
func (f FilterSpec) Allows(t Tool) bool {
	if len(f.Include) > 0 && !anyMatch(f.Include, t.Name) {
		return false
	}
	if anyMatch(f.Exclude, t.Name) {
		return false
	}
	if anyMatch(f.Allow, t.Name) {
		return true
	}
	switch f.MaxEffect {
	case "", "any":
		return true
	case "read-only", "idempotent", "mutating":
		return EffectRank(t.Effect) <= EffectRank(f.MaxEffect)
	default:
		return false
	}
}
