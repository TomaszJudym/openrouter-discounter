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
	// one tool-capable endpoint at half its list price; For re-prices it
	// per role workload
	now := discounts.Tariff{Base: discounts.Rates{Prompt: prompt, Completion: completion}}
	o := discounts.Offer{Provider: "P", Endpoint: id + "@ep", Avg: now, Peak: now,
		List: discounts.Tariff{Base: discounts.Rates{Prompt: 2 * prompt, Completion: 2 * completion}}, Tools: true}
	d := discounts.Discount{ModelID: id, Serves: []string{o.Endpoint}, Offers: []discounts.Offer{o}}
	d, _ = d.For(discounts.Require{}, discounts.DefaultWorkload)
	return d
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
	out := SelectHermes(models, discs, discs, intel, nil)
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
	out := SelectHermes(models, discs, discs, intel, nil)
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
	out := SelectHermes(models, discs, discs, close, nil)
	if got := roleIDs(out, HermesDelegate); got[0] != "a/top" {
		t.Errorf("delegate #1 = %s, want a/top", got[0])
	}
	if got := roleIDs(out, HermesVision); got[0] != "b/close" {
		t.Errorf("vision #1 = %s, want b/close (a penalised)", got[0])
	}
	// scaled: a 100, b 50 — a's 37.5-point composite lead survives the penalty
	far := map[string]float64{"a/top": 30, "b/close": 25, "c/far": 20}
	out = SelectHermes(models, discs, discs, far, nil)
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
		for _, sr := range SelectHermes(models, discs, discs, intel, code) {
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

// flat is a tool-capable model at list price: no discount.
func flat(id string, prompt, completion float64) discounts.Discount {
	t := discounts.Tariff{Base: discounts.Rates{Prompt: prompt, Completion: completion}}
	o := discounts.Offer{Provider: "P", Endpoint: id + "@ep", Avg: t, Peak: t, List: t, Tools: true}
	d := discounts.Discount{ModelID: id, Serves: []string{o.Endpoint}, Offers: []discounts.Offer{o}}
	d, _ = d.ForAny(discounts.Require{}, discounts.DefaultWorkload)
	return d
}

// deep is a tool-capable model at 10% of its list price.
func deep(id string, prompt, completion float64) discounts.Discount {
	now := discounts.Tariff{Base: discounts.Rates{Prompt: prompt, Completion: completion}}
	o := discounts.Offer{Provider: "P", Endpoint: id + "@ep", Avg: now, Peak: now,
		List: discounts.Tariff{Base: discounts.Rates{Prompt: 10 * prompt, Completion: 10 * completion}}, Tools: true}
	d := discounts.Discount{ModelID: id, Serves: []string{o.Endpoint}, Offers: []discounts.Offer{o}}
	d, _ = d.For(discounts.Require{}, discounts.DefaultWorkload)
	return d
}

func TestSelectHermesLightPrefersCheaperListPrice(t *testing.T) {
	models := []discounts.Model{hermesModel("a/deal", caps{}), hermesModel("b/flat", caps{})}
	intel := map[string]float64{"a/deal": 30, "b/flat": 25}
	discs := []discounts.Discount{deep("a/deal", 0.132, 0.396)}
	priced := []discounts.Discount{deep("a/deal", 0.132, 0.396), flat("b/flat", 0.036, 0.081)}
	out := SelectHermes(models, discs, priced, intel, nil)
	if got := roleIDs(out, HermesLight); !slices.Equal(got, []string{"b/flat", "a/deal"}) {
		t.Errorf("light = %v, want the cheaper list-price model first", got)
	}
}

func TestSelectHermesLightSkipsMandatoryReasoning(t *testing.T) {
	thinker := hermesModel("a/thinker", caps{})
	thinker.Reasoning = &discounts.Reasoning{Mandatory: true}
	models := []discounts.Model{thinker, hermesModel("b/plain", caps{})}
	intel := map[string]float64{"a/thinker": 30, "b/plain": 25}
	priced := []discounts.Discount{flat("a/thinker", 0.01, 0.02), flat("b/plain", 0.1, 0.3)}
	out := SelectHermes(models, nil, priced, intel, nil)
	if got := roleIDs(out, HermesLight); !slices.Equal(got, []string{"b/plain"}) {
		t.Errorf("light = %v, want [b/plain] (a always reasons)", got)
	}
}

func TestSelectHermesReviewQualityOverDiscount(t *testing.T) {
	c := caps{tools: true}
	models := []discounts.Model{hermesModel("a/smart", c), hermesModel("b/deal", c), hermesModel("c/weak", c)}
	// scaled II: a 100, b 50, c 0. The old discount composite put b first
	// (25 + 45 for −90%); review weighs quality 80%, so a leads.
	intel := map[string]float64{"a/smart": 40, "b/deal": 30, "c/weak": 20}
	priced := []discounts.Discount{flat("a/smart", 0.45, 0.95), deep("b/deal", 0.05, 0.1), flat("c/weak", 0.3, 0.6)}
	out := SelectHermes(models, nil, priced, intel, nil)
	if got := roleIDs(out, HermesReview); !slices.Equal(got, []string{"a/smart", "b/deal", "c/weak"}) {
		t.Errorf("review = %v, want [a/smart b/deal c/weak]", got)
	}
}

func TestSelectHermesCheapEnoughWaivesPenalty(t *testing.T) {
	c := caps{ctx: 300_000, tools: true, vision: true}
	models := []discounts.Model{hermesModel("a/top", c), hermesModel("b/close", c), hermesModel("c/far", c)}
	// same scores as TestSelectHermesDiversityPenalty, but nearly free:
	// a may hold #1 in every role
	discs := []discounts.Discount{disc("a/top", 0.001, 0.002), disc("b/close", 0.001, 0.002), disc("c/far", 0.001, 0.002)}
	close := map[string]float64{"a/top": 30, "b/close": 29, "c/far": 20}
	out := SelectHermes(models, discs, discs, close, nil)
	for _, sec := range []Sector{HermesDelegate, HermesVision, HermesReview} {
		if got := roleIDs(out, sec); got[0] != "a/top" {
			t.Errorf("%s #1 = %s, want a/top (cheap enough to repeat)", sec, got[0])
		}
	}
}
