package match

import (
	"slices"
	"testing"
)

func TestLookup(t *testing.T) {
	tb := New([]string{"~vendor/model-4.5", "vendor/model-4.5:free", "vendor/model-4.5:batch", "other/thing"})
	got := tb.Lookup("nomatch", "Model 4.5")
	slices.Sort(got)
	if want := []string{"vendor/model-4.5", "vendor/model-4.5:free"}; !slices.Equal(got, want) {
		t.Errorf("Lookup(Model 4.5) = %v, want %v (batch excluded, ~ stripped)", got, want)
	}
	if got := tb.Lookup("vendor-model-4-5-batch"); !slices.Equal(got, []string{"vendor/model-4.5:batch"}) {
		t.Errorf("Lookup(full batch id) = %v, want the batch id", got)
	}
	if got := tb.Lookup("missing"); got != nil {
		t.Errorf("Lookup(missing) = %v, want nil", got)
	}
}

func TestSetMax(t *testing.T) {
	m := map[string]float64{"a": 5}
	SetMax(m, []string{"a", "b"}, 3)
	SetMax(m, []string{"b"}, 4)
	if m["a"] != 5 || m["b"] != 4 {
		t.Errorf("SetMax = %v, want a=5 b=4", m)
	}
}
