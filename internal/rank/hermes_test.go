package rank

import (
	"slices"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

type caps struct {
	ctx                  int
	tools, think, vision bool
}

func hermesModel(id string, c caps) discounts.Model {
	m := discounts.Model{ID: id, ContextLength: c.ctx}
	if c.tools {
		m.SupportedParameters = append(m.SupportedParameters, "tools")
	}
	if c.think {
		m.Reasoning = &discounts.Reasoning{}
	}
	m.Architecture.InputModalities = []string{"text"}
	if c.vision {
		m.Architecture.InputModalities = append(m.Architecture.InputModalities, "image")
	}
	return m
}

func disc(id string, prompt, completion float64) discounts.Discount {
	return discounts.Discount{ModelID: id, Price: prompt, Completion: completion, Was: prompt * 2, Pct: -50}
}

func roleIDs(out []SectorResult, sec Sector) []string {
	for _, sr := range out {
		if sr.Sector == sec {
			return idsOf(sr.Rows)
		}
	}
	return nil
}

func TestSelectHermesFilters(t *testing.T) {
	models := []discounts.Model{
		hermesModel("a/all", caps{ctx: 1_048_576, tools: true, think: true, vision: true}),
		hermesModel("b/notools", caps{ctx: 1_048_576, think: true}),
		hermesModel("c/smallctx", caps{ctx: 128_000, tools: true, think: true}),
		hermesModel("d/weak", caps{ctx: 1_048_576, tools: true, think: true, vision: true}),
		hermesModel("e/x:batch", caps{ctx: 1_048_576, tools: true, think: true, vision: true}),
		hermesModel("f/x:free", caps{ctx: 1_048_576, tools: true, think: true, vision: true}),
		hermesModel("g/noii", caps{ctx: 1_048_576, tools: true, think: true, vision: true}),
	}
	intel := map[string]float64{
		"a/all": 36, "b/notools": 30, "c/smallctx": 28, "d/weak": 15, "e/x:batch": 40, "f/x:free": 40,
	}
	discs := make([]discounts.Discount, 0, len(models))
	for _, m := range models {
		discs = append(discs, disc(m.ID, 0.1, 0.4))
	}
	out := SelectHermes(models, discs, intel, nil)
	tests := []struct {
		sec  Sector
		want []string
	}{
		{HermesDelegate, []string{"a/all", "c/smallctx", "d/weak"}}, // tools; a leads past the penalty
		{HermesVision, []string{"a/all", "d/weak"}},                 // image input
		{HermesCompress, []string{"a/all", "b/notools", "d/weak"}},  // ctx≥1M
		{HermesReview, []string{"a/all", "c/smallctx", "d/weak"}},   // tools
		{HermesLight, []string{"b/notools", "c/smallctx", "a/all"}}, // II≥20; equal price, a penalised
	}
	for _, tt := range tests {
		if got := roleIDs(out, tt.sec); !slices.Equal(got, tt.want) {
			t.Errorf("%s = %v, want %v", tt.sec, got, tt.want)
		}
	}
}

func TestSelectHermesLightCheapest(t *testing.T) {
	models := []discounts.Model{
		hermesModel("a/pricey", caps{}), hermesModel("b/cheap", caps{}), hermesModel("c/mid", caps{}),
	}
	intel := map[string]float64{"a/pricey": 40, "b/cheap": 25, "c/mid": 30}
	discs := []discounts.Discount{disc("a/pricey", 0.5, 1.0), disc("b/cheap", 0.01, 0.05), disc("c/mid", 0.2, 0.5)}
	out := SelectHermes(models, discs, intel, nil)
	if got := roleIDs(out, HermesLight); !slices.Equal(got, []string{"b/cheap", "c/mid", "a/pricey"}) {
		t.Errorf("light = %v, want cheapest first", got)
	}
	for _, sr := range out {
		if sr.Sector == HermesLight && (sr.Source != SourceCheapness || sr.Rows[2].Score != 0) {
			t.Errorf("light source/score = %q/%v, want %q/0 at the ceiling", sr.Source, sr.Rows[2].Score, SourceCheapness)
		}
	}
}

func TestSelectHermesDiversityPenalty(t *testing.T) {
	c := caps{ctx: 300_000, tools: true, vision: true}
	models := []discounts.Model{hermesModel("a/top", c), hermesModel("b/close", c), hermesModel("c/far", c)}
	discs := []discounts.Discount{disc("a/top", 0.1, 0.4), disc("b/close", 0.1, 0.4), disc("c/far", 0.1, 0.4)}
	// scaled: a 100, b 90, c 0 — a loses 15 in later roles and drops below b
	close := map[string]float64{"a/top": 30, "b/close": 29, "c/far": 20}
	out := SelectHermes(models, discs, close, nil)
	if got := roleIDs(out, HermesDelegate); got[0] != "a/top" {
		t.Errorf("delegate #1 = %s, want a/top", got[0])
	}
	if got := roleIDs(out, HermesVision); got[0] != "b/close" {
		t.Errorf("vision #1 = %s, want b/close (a penalised)", got[0])
	}
	// scaled: a 100, b 50 — a's 37.5-point composite lead survives the penalty
	far := map[string]float64{"a/top": 30, "b/close": 25, "c/far": 20}
	out = SelectHermes(models, discs, far, nil)
	if got := roleIDs(out, HermesVision); got[0] != "a/top" {
		t.Errorf("vision #1 = %s, want a/top (lead > penalty)", got[0])
	}
}

func TestSelectHermesDelegateCodeSource(t *testing.T) {
	c := caps{ctx: 300_000, tools: true}
	models := []discounts.Model{hermesModel("a/m", c), hermesModel("b/m", c), hermesModel("c/m", c), hermesModel("d/m", c)}
	discs := make([]discounts.Discount, 0, len(models))
	for _, m := range models {
		discs = append(discs, disc(m.ID, 0.1, 0.4))
	}
	intel := map[string]float64{"a/m": 40, "b/m": 30, "c/m": 20, "d/m": 10}
	source := func(code map[string]float64) (string, []string) {
		for _, sr := range SelectHermes(models, discs, intel, code) {
			if sr.Sector == HermesDelegate {
				return sr.Source, idsOf(sr.Rows)
			}
		}
		return "", nil
	}
	if src, _ := source(map[string]float64{"c/m": 90, "d/m": 80}); src != SourceIntelligence {
		t.Errorf("2 code scores: source = %q, want %q", src, SourceIntelligence)
	}
	src, ids := source(map[string]float64{"b/m": 50, "c/m": 90, "d/m": 80})
	if src != SourceCode || !slices.Equal(ids, []string{"c/m", "d/m", "b/m"}) {
		t.Errorf("3 code scores: %q %v, want %q [c d b]", src, ids, SourceCode)
	}
}
