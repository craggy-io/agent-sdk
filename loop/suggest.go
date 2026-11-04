package loop

import (
	"sort"
	"strings"
)

// maxSuggestions bounds the "did you mean" list of an unknown-tool error.
const maxSuggestions = 3

// suggestTools returns up to maxSuggestions names from candidates that
// name plausibly meant, best first: names equal but for case and
// separators, then one a prefix of the other, then names sharing most
// of name's segments (a segment matches when equal, one is a long
// prefix of the other, or one typo apart: ghidra_decompile →
// ghidra_decompilation), then names a few edits away. Matching is
// case-insensitive and treats '_', '-' and '.' alike. When any name
// matches in one of the first three ways, the weaker ones are dropped.
func suggestTools(name string, candidates []string) []string {
	q := normToolName(name)
	if q == "" {
		return nil
	}
	qseg := strings.Split(q, "_")
	type scored struct {
		name    string
		tier    int // lower is better
		matched int // query segments matched, more is better
		dist    int // edit distance, less is better
	}
	var out []scored
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c] || c == name {
			continue
		}
		seen[c] = true
		n := normToolName(c)
		s := scored{name: c, dist: editDistance(q, n), matched: matchedSegments(qseg, strings.Split(n, "_"))}
		switch {
		case n == q:
			s.tier = 0
		case strings.HasPrefix(n, q) || strings.HasPrefix(q, n):
			s.tier = 1
		case s.matched == len(qseg):
			s.tier = 2
		case s.matched > 0 && 2*s.matched >= len(qseg):
			s.tier = 3
		case s.dist <= max(2, len(q)/4):
			s.tier = 4
		default:
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.matched != b.matched {
			return a.matched > b.matched
		}
		if a.dist != b.dist {
			return a.dist < b.dist
		}
		return a.name < b.name
	})
	names := make([]string, 0, maxSuggestions)
	for _, s := range out {
		// A strong match (tiers 0-2) makes partial ones noise.
		if len(names) == maxSuggestions || (out[0].tier <= 2 && s.tier > 2) {
			break
		}
		names = append(names, s.name)
	}
	return names
}

func normToolName(s string) string {
	return strings.Trim(strings.Map(func(r rune) rune {
		if r == '-' || r == '.' {
			return '_'
		}
		return r
	}, strings.ToLower(s)), "_")
}

// matchedSegments counts the query segments that match some segment of
// the candidate.
func matchedSegments(query, cand []string) int {
	n := 0
	for _, q := range query {
		for _, c := range cand {
			if segmentsMatch(q, c) {
				n++
				break
			}
		}
	}
	return n
}

func segmentsMatch(a, b string) bool {
	if a == b {
		return a != ""
	}
	short := min(len(a), len(b))
	if short < 3 {
		return false
	}
	p := 0
	for p < short && a[p] == b[p] {
		p++
	}
	// A long shared prefix: decompile/decompilation, func/functions.
	if p >= 3 && 3*p >= 2*short {
		return true
	}
	return short >= 4 && editDistance(a, b) <= 1
}

// editDistance is the Levenshtein distance between a and b, by byte.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
