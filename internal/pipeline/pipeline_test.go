package pipeline

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

func TestIntelligenceVariantsInheritBase(t *testing.T) {
	mk := func(id string, ii float64) discounts.Model {
		m := discounts.Model{ID: id}
		m.Benchmarks.ArtificialAnalysis.IntelligenceIndex = ii
		return m
	}
	got := intelligence([]discounts.Model{
		mk("v/a", 50), mk("v/a:free", 0), mk("v/a:thinking", 0), mk("v/b:free", 30), mk("v/c", 0),
	})
	want := map[string]float64{"v/a": 50, "v/a:free": 50, "v/b:free": 30}
	if len(got) != len(want) {
		t.Errorf("intelligence = %v, want %v", got, want)
	}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("intelligence[%s] = %v, want %v", id, got[id], v)
		}
	}
}

// fixture is four tool-calling chat models, each served by an OpenRouter
// provider and by Darkbloom direct at half its price.
func fixture(t *testing.T) (models []discounts.Model, priced []discounts.Discount, direct []discounts.DirectModel) {
	t.Helper()
	ids := []string{"v/a", "v/b", "v/c", "v/d"}
	for i, id := range ids {
		m := discounts.Model{ID: id, SupportedParameters: []string{"tools", "reasoning"}, ContextLength: 2_000_000}
		m.Benchmarks.ArtificialAnalysis.IntelligenceIndex = float64(40 + i)
		models = append(models, m)
		tr := discounts.Tariff{Base: discounts.Rates{Prompt: 0.2, Completion: 0.8}}
		d := discounts.Discount{ModelID: id, Serves: []string{"or/" + id}, Offers: []discounts.Offer{{
			Provider: "Other", Endpoint: "or/" + id, Avg: tr, Peak: tr, List: tr, Tools: true, MaxOutput: 1 << 20,
		}}}
		p, ok := d.ForAny(discounts.Require{}, discounts.DefaultWorkload)
		if !ok {
			t.Fatal("fixture row not priced")
		}
		priced = append(priced, p)
		direct = append(direct, discounts.DirectModel{
			ID: "db-" + id[2:], BaseID: id, Tools: true, Context: 2_000_000, MaxOutput: 1 << 20,
			Pricing: discounts.Pricing{Prompt: "0.0000001", Completion: "0.0000004"},
		})
	}
	return models, priced, direct
}

// TestDirectSideRoutesOnlyDirect: every preset of the direct side picks
// Darkbloom-direct endpoints only, at most 3 per preset, and the report
// keeps its halflight pick apart from the preset list.
func TestDirectSideRoutesOnlyDirect(t *testing.T) {
	models, priced, direct := fixture(t)
	discs, all := discounts.Direct(direct, priced, time.Now())
	if len(discs) != 4 {
		t.Fatalf("direct discounts = %d, want 4", len(discs))
	}
	intel := intelligence(models)
	report, presets := selectSide(models, discounts.Affordable(discs), discs, all, nil, nil, intel)
	if len(presets) == 0 {
		t.Fatal("no preset sectors")
	}
	for _, sr := range presets {
		for _, r := range sr.Rows {
			if !discounts.IsDirect(r.Endpoint) {
				t.Errorf("%s: row %s routes via %q, want a db: endpoint", sr.Sector, r.ModelID, r.Endpoint)
			}
		}
		if sr.Sector == rank.StrongestDiscountShivers && len(sr.Rows) != 3 {
			t.Errorf("shivers rows = %d, want 3", len(sr.Rows))
		}
	}
	last := report.Sectors[len(report.Sectors)-1]
	if last.Sector != rank.StrongestDiscountHalflight {
		t.Errorf("report ends with %s, want the halflight pick", last.Sector)
	}
	if !slices.ContainsFunc(presets, func(sr rank.SectorResult) bool { return sr.Sector == rank.ExpensiveLong }) {
		t.Error("presets lack expensive-long")
	}
}

// taggedEndpointsBody is endpointsBody with the endpoint tag real
// OpenRouter endpoints carry; a preset row needs the tag to pin, so
// rank.Pinnable keeps untagged fixture rows out.
func taggedEndpointsBody(id string, discount float64) string {
	return fmt.Sprintf(`{"data":{"id":%q,"endpoints":[{"provider_name":"List","tag":"or/%s",`+
		`"pricing":{"prompt":"0.0000002","completion":"0.0000008","discount":%v},`+
		`"supported_parameters":["tools"],"context_length":2000000,`+
		`"max_completion_tokens":1048576,"supports_tool_choice":{}}]}}`, id, id, discount)
}

// TestRunEmitsEveryManagedPresetSector: each preset.ManagedSlugs() slug has
// rows on both sides whenever the day has eligible candidates: admech-code
// from the code sector and the six admech-code-<role> sectors from
// rank.SelectAdmech. cmd/tracker and cmd/pipresets rely on these rows to
// update or emit every managed preset.
func TestRunEmitsEveryManagedPresetSector(t *testing.T) {
	hc := runStub{
		catalog: catalogAll,
		endpoints: map[string]string{
			"vendor/alphachat": taggedEndpointsBody("vendor/alphachat", 0.25),
			"vendor/betachat":  taggedEndpointsBody("vendor/betachat", 0),
		},
		darkbloom: darkbloomBody("alphachat"),
	}.client(t)
	res := run(t, hc, Keys{Darkbloom: "db-key"})
	for _, side := range []struct {
		name    string
		sectors []rank.SectorResult
	}{{"openrouter", res.OpenRouter}, {"darkbloom", res.Direct}} {
		got := make(map[rank.Sector]bool, len(side.sectors))
		for _, sr := range side.sectors {
			if len(sr.Rows) > 0 {
				got[sr.Sector] = true
			}
		}
		for _, slug := range preset.ManagedSlugs() {
			sector, ok := sectorForSlug(slug)
			if !ok {
				t.Errorf("managed slug %s has no rank sector", slug)
				continue
			}
			if !got[sector] {
				t.Errorf("%s side lacks rows for managed slug %s (sector %s): %v", side.name, slug, sector, got)
			}
		}
	}
}

// sectorForSlug is preset.Slugs inverted.
func sectorForSlug(slug string) (rank.Sector, bool) {
	for sec, s := range preset.Slugs {
		if s == slug {
			return sec, true
		}
	}
	return "", false
}

// TestOpenRouterSideNeverDirect: the OpenRouter side is built from
// OpenRouter endpoints only, so no db: endpoint reaches a preset.
func TestOpenRouterSideNeverDirect(t *testing.T) {
	models, priced, _ := fixture(t)
	// make the OpenRouter rows discounted: list price twice the current
	discs := make([]discounts.Discount, 0, len(priced))
	for _, d := range priced {
		d.Offers[0].List = discounts.Tariff{Base: discounts.Rates{Prompt: 0.4, Completion: 1.6}}
		p, ok := d.For(discounts.Require{}, discounts.DefaultWorkload)
		if !ok {
			t.Fatal("fixture row not discounted")
		}
		discs = append(discs, p)
	}
	_, presets := selectSide(models, discounts.Affordable(discs), discs, priced, nil, nil, intelligence(models))
	for _, sr := range presets {
		for _, r := range sr.Rows {
			if discounts.IsDirect(r.Endpoint) {
				t.Errorf("%s: OpenRouter side routes %s via %s", sr.Sector, r.ModelID, r.Endpoint)
			}
		}
	}
}
