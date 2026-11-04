package loop

import (
	"slices"
	"testing"
)

func TestSuggestTools(t *testing.T) {
	ghidra := []string{"ghidra_decompilation", "ghidra_functions", "ghidra_function-calls",
		"ghidra_strings", "ghidra_symbols", "web_fetch", "web_search", "result_read"}
	cases := []struct {
		name string
		want []string // leading suggestions, in order
	}{
		{"ghidra_decompile", []string{"ghidra_decompilation"}},
		{"GHIDRA-DECOMPILATION", []string{"ghidra_decompilation"}},
		{"ghidra_decomp", []string{"ghidra_decompilation"}},
		{"web_fech", []string{"web_fetch"}},
		{"ghidra_function_call", []string{"ghidra_function-calls"}},
		{"ghidra_foo", []string{"ghidra_symbols", "ghidra_functions", "ghidra_strings"}}, // partial: nearest by edits
		{"decompilation", []string{"ghidra_decompilation"}},
		{"result_reed", []string{"result_read"}},
		{"frobnicate", nil},
	}
	for _, c := range cases {
		got := suggestTools(c.name, ghidra)
		if len(got) > maxSuggestions {
			t.Errorf("suggestTools(%q) = %v: more than %d", c.name, got, maxSuggestions)
		}
		if c.want == nil {
			if len(got) != 0 {
				t.Errorf("suggestTools(%q) = %v, want none", c.name, got)
			}
			continue
		}
		if len(got) < len(c.want) || !slices.Equal(got[:len(c.want)], c.want) {
			t.Errorf("suggestTools(%q) = %v, want it to start with %v", c.name, got, c.want)
		}
	}
}

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		d    int
	}{{"", "", 0}, {"abc", "", 3}, {"kitten", "sitting", 3}, {"decompile", "decompilation", 5}} {
		if got := editDistance(c.a, c.b); got != c.d {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.d)
		}
	}
}
