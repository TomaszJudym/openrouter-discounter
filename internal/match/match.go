// Package match maps third-party model names onto OpenRouter model ids.
package match

import (
	"strings"
)

// Table maps normalized names to every OpenRouter id they stand for. A base
// name also covers its ":free" variant, so one external score reaches both.
// Other variants (":batch", …) are entered only by their full form, so they
// stay unscored and do not duplicate the base model's row.
type Table map[string][]string

// New builds the lookup table for the OpenRouter ids. Each id is entered by
// its full form and by its tail after "vendor/"; a ":free" id is also
// entered without the suffix.
func New(ids []string) Table {
	t := make(Table, len(ids)*2)
	for _, id := range ids {
		clean := strings.TrimPrefix(id, "~")
		names := []string{clean}
		if base, ok := strings.CutSuffix(clean, ":free"); ok {
			names = append(names, base)
		}
		keys := make([]string, 0, 2*len(names))
		for _, n := range names {
			keys = append(keys, n)
			if _, tail, ok := strings.Cut(n, "/"); ok {
				keys = append(keys, tail)
			}
		}
		seen := make(map[string]bool, len(keys))
		for _, k := range keys {
			n := Normalize(k)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			t[n] = append(t[n], clean)
		}
	}
	return t
}

// Lookup returns the OpenRouter ids for the first candidate name that
// matches, or nil.
func (t Table) Lookup(candidates ...string) []string {
	for _, c := range candidates {
		if ids, ok := t[Normalize(c)]; ok {
			return ids
		}
	}
	return nil
}

// Normalize lowercases s and drops everything but ASCII letters and digits,
// so "vendor/model-4.5" and "Model 4.5" line up as "model45".
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 'a' - 'A')
		}
	}
	return b.String()
}

// SetMax records v for every id, keeping the highest value per id.
func SetMax(dst map[string]float64, ids []string, v float64) {
	for _, id := range ids {
		if cur, ok := dst[id]; !ok || v > cur {
			dst[id] = v
		}
	}
}
