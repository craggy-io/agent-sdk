package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSize parses byte sizes: plain integers, or a number with a suffix
// B|KiB|MiB|GiB (binary) / KB|MB|GB (decimal), case-insensitive ("32KiB").
func ParseSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("config: empty size")
	}
	upper := strings.ToUpper(s)
	mult := int64(1)
	num := upper
	for _, suf := range []struct {
		name string
		mult int64
	}{
		{"KIB", 1024}, {"MIB", 1024 * 1024}, {"GIB", 1024 * 1024 * 1024}, {"TIB", 1024 * 1024 * 1024 * 1024},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
		{"B", 1}, {"K", 1024}, {"M", 1024 * 1024}, {"G", 1024 * 1024 * 1024},
	} {
		if strings.HasSuffix(upper, suf.name) {
			mult = suf.mult
			num = upper[:len(upper)-len(suf.name)]
			break
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, fmt.Errorf("config: bad size %q: %w", s, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("config: negative size %q", s)
	}
	return int(f * float64(mult)), nil
}
