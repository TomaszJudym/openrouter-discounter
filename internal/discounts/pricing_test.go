package discounts

import (
	"slices"
	"testing"
	"time"
)

//go:fix inline
func TestOverrideActiveAtWindows(t *testing.T) {
	friday := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		o    Override
		at   time.Time
		want bool
	}{
		// minimax-style window wrapping midnight, no days
		{"wrap evening", Override{Start: new(1500), End: new(100)}, friday.Add(22 * time.Hour), true},
		{"wrap after midnight", Override{Start: new(1500), End: new(100)}, friday.Add(30 * time.Minute), true},
		{"wrap closed", Override{Start: new(1500), End: new(100)}, friday.Add(10 * time.Hour), false},
		{"wrap end exclusive", Override{Start: new(1500), End: new(100)}, friday.Add(time.Hour), false},
		// utc_end 0 means midnight, not a wrap
		{"end zero open", Override{Start: new(1600), End: new(0)}, friday.Add(23*time.Hour + 59*time.Minute), true},
		{"end zero closed", Override{Start: new(1600), End: new(0)}, friday, false},
		// the after-midnight part of a wrapped window belongs to the previous day
		{"days wrap same day", Override{Days: []string{"friday"}, Start: new(2200), End: new(200)}, friday.Add(23 * time.Hour), true},
		{"days wrap next day", Override{Days: []string{"friday"}, Start: new(2200), End: new(200)}, friday.Add(25 * time.Hour), true},
		{"days wrap previous day", Override{Days: []string{"friday"}, Start: new(2200), End: new(200)}, friday.Add(time.Hour), false},
		{"days only", Override{Days: []string{"Friday"}}, friday.Add(12 * time.Hour), true},
		{"days only other day", Override{Days: []string{"saturday"}}, friday.Add(12 * time.Hour), false},
		{"non-UTC clock", Override{Start: new(1500), End: new(100)}, friday.Add(22 * time.Hour).In(time.FixedZone("JST", 9*3600)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.o.activeAt(tt.at); got != tt.want {
				t.Errorf("activeAt(%v) = %v, want %v", tt.at, got, tt.want)
			}
		})
	}
}

func TestOverrideLabel(t *testing.T) {
	if got := (Override{Start: new(1500), End: new(100)}).label(); got != "15:00–01:00 UTC" {
		t.Errorf("label() = %q, want %q", got, "15:00–01:00 UTC")
	}
	if got := (Override{Days: []string{"saturday"}}).label(); got != "saturday" {
		t.Errorf("label() = %q, want %q", got, "saturday")
	}
}

func TestBlendedCountsCacheAndReasoning(t *testing.T) {
	// live deepseek-v3.2 endpoints, 2026-11: Baidu lists a higher prompt
	// price but a far cheaper cache read, so it wins once 80% is cached
	deepInfra := Rates{Prompt: 0.26, Completion: 0.38, CacheRead: 0.13}
	baidu := Rates{Prompt: 0.28, Completion: 0.42, CacheRead: 0.028}
	if b, d := baidu.Blended(DefaultWorkload), deepInfra.Blended(DefaultWorkload); b >= d {
		t.Errorf("Blended: baidu %v >= deepinfra %v, want baidu cheaper under caching", b, d)
	}
	w := Workload{Input: 1000, Output: 1000, CacheHit: 0.5, Reasoning: 0.5}
	r := Rates{Prompt: 2, Completion: 10, CacheRead: 1, CacheWrite: 3, Reasoning: 20}
	// in: 500×1 + 500×3; out: 500×10 + 500×20; over 2000 tokens
	if got, want := r.Blended(w), (500.0+1500+5000+10000)/2000; !floatEq(got, want) {
		t.Errorf("Blended() = %v, want %v", got, want)
	}
	// a cache write listed below the prompt rate (Gemini storage) never
	// makes misses cheaper than plain input
	low := Rates{Prompt: 2, Completion: 10, CacheWrite: 0.02}
	if got, want := low.Blended(w), (1000*2.0+1000*10)/2000; !floatEq(got, want) {
		t.Errorf("Blended() with low cache write = %v, want %v", got, want)
	}
}

func TestTariffContextTiers(t *testing.T) {
	p := Pricing{Prompt: "0.000001", Completion: "0.000005", Overrides: []Override{
		{MinPrompt: 272_000, Prompt: "0.000002", Completion: "0.0000075"},
	}}
	tr := p.tariff()
	if got := tr.For(DefaultWorkload).Prompt; !floatEq(got, 1) {
		t.Errorf("For(DefaultWorkload).Prompt = %v, want base 1", got)
	}
	if got := tr.For(LongWorkload); !floatEq(got.Prompt, 2) || !floatEq(got.Completion, 7.5) {
		t.Errorf("For(LongWorkload) = %+v, want the 272k tier 2/7.5", got)
	}
}

func TestDiscountAtLongWorkload(t *testing.T) {
	// 50% endpoint discount on the base only: the 272k tier is listed at
	// the undiscounted price, so long requests save less
	byID := map[string][]Endpoint{"a/m": {{Provider: "P", Pricing: Pricing{
		Prompt: "0.000001", Completion: "0.000004", Discount: 0.5,
		Overrides: []Override{{MinPrompt: 272_000, Prompt: "0.000004", Completion: "0.000016"}},
	}}}}
	got := endpointDiscounts(byID, time.Time{})
	if len(got) != 1 {
		t.Fatalf("endpointDiscounts() = %+v, want 1 row", got)
	}
	if !floatEq(got[0].Pct, -50) {
		t.Errorf("DefaultWorkload pct = %v, want -50", got[0].Pct)
	}
	long, ok := got[0].For(Require{}, LongWorkload)
	if !ok || !floatEq(long.Pct, -50) {
		t.Errorf("For(LongWorkload) = %+v, %v, want pct -50 (tier scales with the discount)", long, ok)
	}
	if long.Price <= got[0].Price || !floatEq(long.Prompt, 4) || !floatEq(long.PeakPrompt, 4) {
		t.Errorf("For(LongWorkload) price/prompt/peak = %v/%v/%v, want the 272k tier 4/Mtok above base %v",
			long.Price, long.Prompt, long.PeakPrompt, got[0].Price)
	}
}

func TestEndpointWindowDiscount(t *testing.T) {
	// live minimax-m3.1-flash first-party endpoint, 2026-11: half price
	// 15:00–01:00 UTC, base price otherwise
	window := Endpoint{Provider: "Minimax", Pricing: Pricing{
		Prompt: "0.00000015", Completion: "0.0000006", CacheRead: "0.000000015",
		Overrides: []Override{
			{Start: new(1500), End: new(100), Prompt: "0.000000075", Completion: "0.0000003", CacheRead: "0.0000000075"},
			{Start: new(100), End: new(1500), Prompt: "0.00000015", Completion: "0.0000006", CacheRead: "0.000000015"},
		},
	}}
	byID := map[string][]Endpoint{"minimax/m": {window, ep("Other", "0.00000013", "0.00000052", 0)}}
	// half price 10h of every 24h: the day averages 19/24 of list, whenever
	// the run starts — inside the window or not
	want := (19.0/24 - 1) * 100
	run := time.Date(2026, 11, 17, 22, 0, 0, 0, time.UTC) // the daily cron
	for _, at := range []time.Time{run, run.Add(-12 * time.Hour)} {
		got := endpointDiscounts(byID, at)
		if len(got) != 1 || got[0].Provider != "Minimax" || got[0].Window != "15:00–01:00 UTC" || !floatEq(got[0].Pct, want) {
			t.Fatalf("endpointDiscounts(%v) = %+v, want Minimax %.1f%% over the day", at, got, want)
		}
		if !floatEq(got[0].PeakPrompt, 0.15) || !floatEq(got[0].PeakCompletion, 0.6) {
			t.Errorf("peak = %v/%v, want the 0.15/0.6 base outside the window", got[0].PeakPrompt, got[0].PeakCompletion)
		}
	}
	// a flat price below the window's daily average (0.11875) wins
	byID["minimax/m"][1] = ep("Other", "0.00000011", "0.00000044", 0)
	if got := endpointDiscounts(byID, run); len(got) != 0 {
		t.Errorf("endpointDiscounts() = %+v, want none (flat 0.11 beats the window's daily average)", got)
	}
}

func TestEndpointDiscountBeatenByUndiscounted(t *testing.T) {
	// a provider's "-50%" is no saving when another provider lists less
	byID := map[string][]Endpoint{"a/m": {
		ep("Discounted", "0.00000015", "0.0000006", 0.5),
		ep("Cheaper", "0.000000075", "0.0000003", 0),
	}}
	if got := endpointDiscounts(byID, time.Time{}); len(got) != 0 {
		t.Errorf("endpointDiscounts() = %+v, want none (undiscounted endpoint is cheaper)", got)
	}
}

func TestUsableFiltersQuantization(t *testing.T) {
	eps := []Endpoint{
		{Provider: "solid", Quantization: "fp8"},
		{Provider: "crushed", Quantization: "fp4"},
		{Provider: "unknown", Quantization: "unknown"},
	}
	got := make([]string, 0, len(eps))
	for _, e := range usable(eps) {
		got = append(got, e.Provider)
	}
	if want := []string{"solid", "unknown"}; !slices.Equal(got, want) {
		t.Errorf("usable() = %v, want %v", got, want)
	}
	// natively 4-bit: no higher-precision endpoint, so 4-bit stays
	native := []Endpoint{{Provider: "a", Quantization: "mxfp4"}, {Provider: "b", Quantization: "int4"}}
	if n := len(usable(native)); n != 2 {
		t.Errorf("usable(all 4-bit) kept %d, want 2", n)
	}
}

func TestEndpointDiscountPicksCheapestTotal(t *testing.T) {
	// LowPrompt lists the lowest prompt price but no cache discount; Cached
	// wins once 80% of the prompt is cached. Crushed is cheapest of all but
	// 4-bit next to fp8 peers, so it is never pinned.
	low := ep("LowPrompt", "0.0000002", "0.0000008", 0.5)
	low.Tag = "low/fp8"
	cached := Endpoint{Provider: "Cached", Tag: "cached/fp8", Quantization: "fp8", ImplicitCaching: true, Pricing: Pricing{
		Prompt: "0.0000003", Completion: "0.0000008", CacheRead: "0.00000003", Discount: 0.4,
	}}
	crushed := ep("Crushed", "0.00000001", "0.0000001", 0.9)
	crushed.Tag, crushed.Quantization = "crushed/fp4", "fp4"
	got := endpointDiscounts(map[string][]Endpoint{"a/m": {low, cached, crushed}}, time.Time{})
	if len(got) != 1 || got[0].Provider != "Cached" || got[0].Endpoint != "cached/fp8" {
		t.Fatalf("endpointDiscounts() = %+v, want Cached pinned at cached/fp8", got)
	}
	if want := []string{"low/fp8", "cached/fp8", "crushed/fp4"}; !slices.Equal(got[0].Serves, want) {
		t.Errorf("Serves = %v, want every endpoint tag %v", got[0].Serves, want)
	}
	// without implicit caching (and no cache_control from the client) the
	// cache-read price never applies, so the lower prompt price wins
	cached.ImplicitCaching = false
	got = endpointDiscounts(map[string][]Endpoint{"a/m": {low, cached, crushed}}, time.Time{})
	if len(got) != 1 || got[0].Provider != "LowPrompt" {
		t.Errorf("endpointDiscounts(explicit-only caching) = %+v, want LowPrompt", got)
	}
}

func TestForRepicksByToolsAndContext(t *testing.T) {
	yes, no := true, false
	cheap := ep("Cheap", "0.0000001", "0.0000004", 0.5) // no tools, short context
	cheap.Tag, cheap.ContextLength = "cheap/fp8", 128_000
	tools := ep("Tools", "0.0000002", "0.0000008", 0.5)
	tools.Tag, tools.ContextLength, tools.Parameters = "tools/fp8", 1_000_000, []string{"tools", "tool_choice"}
	tools.ToolChoice.Auto = &yes
	refuses := ep("Refuses", "0.00000015", "0.0000006", 0.5) // lists tools, refuses tool_choice auto
	refuses.Tag, refuses.Parameters, refuses.ContextLength = "refuses/fp8", []string{"tools"}, 128_000
	refuses.ToolChoice.Auto = &no
	got := endpointDiscounts(map[string][]Endpoint{"a/m": {cheap, tools, refuses}}, time.Time{})
	if len(got) != 1 || got[0].Endpoint != "cheap/fp8" {
		t.Fatalf("endpointDiscounts() = %+v, want cheap/fp8 for the report", got)
	}
	if p, ok := got[0].For(Require{Tools: true}, DefaultWorkload); !ok || p.Endpoint != "tools/fp8" {
		t.Errorf("For(Tools) = %+v, %v, want tools/fp8", p, ok)
	}
	if p, ok := got[0].For(Require{}, LongWorkload); !ok || p.Endpoint != "tools/fp8" {
		t.Errorf("For(LongWorkload) = %+v, %v, want tools/fp8 (304k does not fit 128k)", p, ok)
	}
	if p, ok := got[0].For(Require{MinContext: 2_000_000}, DefaultWorkload); ok {
		t.Errorf("For(MinContext 2M) = %+v, want none", p)
	}
	// the only tool endpoint is undiscounted: no discount for a tool preset
	got[0].Offers[1].List = got[0].Offers[1].Avg
	if p, ok := got[0].For(Require{Tools: true}, DefaultWorkload); ok {
		t.Errorf("For(Tools, undiscounted) = %+v, want none", p)
	}
}

func TestEndpointDiscountNeverPicksFlex(t *testing.T) {
	// live deepseek-v4.1-flash on DeepInfra, 2026-11: flex lists below
	// standard; only the standard endpoint may be pinned
	flex := ep("DeepInfra", "0.000000112", "0.0000004", 0.3)
	flex.Tag, flex.ServiceTier, flex.Quantization = "deepinfra/fp8/flex", "flex", "fp8"
	std := ep("DeepInfra", "0.00000014", "0.0000005", 0.3)
	std.Tag, std.Quantization = "deepinfra/fp8", "fp8"
	got := endpointDiscounts(map[string][]Endpoint{"a/m": {flex, std}}, time.Time{})
	if len(got) != 1 || got[0].Endpoint != "deepinfra/fp8" {
		t.Errorf("endpointDiscounts() = %+v, want deepinfra/fp8 pinned, not flex", got)
	}
	// the flex endpoint's precision does not count against 4-bit peers
	flex.Quantization = "bf16"
	crushed := ep("Other", "0.0000001", "0.0000004", 0.5)
	crushed.Tag, crushed.Quantization = "other/fp4", "fp4"
	if n := len(usable([]Endpoint{flex, crushed})); n != 1 {
		t.Errorf("usable(flex bf16 + fp4) kept %d, want 1 (fp4 stays)", n)
	}
}

func TestEndpointDiscountTiePrefersDiscounted(t *testing.T) {
	byID := map[string][]Endpoint{"a/m": {
		ep("List", "0.0000001", "0.0000004", 0),
		ep("Promo", "0.0000001", "0.0000004", 0.5),
	}}
	if got := endpointDiscounts(byID, time.Time{}); len(got) != 1 || got[0].Provider != "Promo" {
		t.Errorf("endpointDiscounts() = %+v, want Promo (same price, discounted)", got)
	}
}
