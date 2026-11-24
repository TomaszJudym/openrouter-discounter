package rank

import (
	"slices"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

func TestSelectHindsightFree(t *testing.T) {
	model := func(id string, params ...string) discounts.Model {
		m := discounts.Model{ID: id, SupportedParameters: params}
		m.Architecture.InputModalities = []string{"text", "image"}
		return m
	}
	models := []discounts.Model{
		model("a/json:free", "tools", "response_format"),
		model("b/strong:free", "tools"),
		model("c/notools:free", "response_format"),
		model("d/schema:free", "tools", "structured_outputs"),
		model("e/noii:free", "tools", "response_format"),
		model("f/weak:free", "tools"),
		model("thinkingmachines/inkling:free", "tools", "response_format"),
	}
	intel := map[string]float64{"a/json:free": 30, "b/strong:free": 45, "c/notools:free": 50, "d/schema:free": 35, "f/weak:free": 10, "thinkingmachines/inkling:free": 60}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	sr := SelectHindsightFree(ids, models, intel)
	// strongest first; c has no tools, e no II, inkling is harness-only,
	// f is below half of b's 45
	if got := idsOf(sr.Rows); !slices.Equal(got, []string{"b/strong:free", "d/schema:free", "a/json:free"}) {
		t.Errorf("rows = %v, want [b d a]", got)
	}
	// equal scores: the schema-capable model leads; f (10) is under half of 20
	intel2 := map[string]float64{"a/json:free": 20, "b/strong:free": 20, "f/weak:free": 9}
	if got := idsOf(SelectHindsightFree(ids, models, intel2).Rows); !slices.Equal(got, []string{"a/json:free", "b/strong:free"}) {
		t.Errorf("tie rows = %v, want [a b]", got)
	}
	for _, r := range sr.Rows {
		if r.Endpoint != "" || !r.Scored {
			t.Errorf("row %+v: want scored and unpinned", r)
		}
	}
	if sr := SelectHindsightFree(nil, models, intel); len(sr.Rows) != 0 {
		t.Errorf("no free ids: rows = %v, want none", sr.Rows)
	}
}
