package discounts

import (
	"testing"
	"time"
)

func flatOffer(provider, tag string, prompt, completion float64) Offer {
	t := Tariff{Base: Rates{Prompt: prompt, Completion: completion}}
	return Offer{Provider: provider, Endpoint: tag, Avg: t, Peak: t, List: t, Tools: true}
}

// TestDarkbloomEndpointPricedAgainstOthers: OpenRouter's darkbloom/*
// endpoint lists no discount; undercutting every other provider counts as
// one, with was = the cheapest other provider.
func TestDarkbloomEndpointPricedAgainstOthers(t *testing.T) {
	d := Discount{ModelID: "m/x", Offers: []Offer{
		flatOffer(DarkbloomProvider, "darkbloom/fp4", 0.05, 0.2),
		flatOffer("Other", "other/bf16", 0.1, 0.4),
		flatOffer("Dear", "dear/bf16", 0.5, 2),
	}}
	got, ok := d.For(Require{}, DefaultWorkload)
	if !ok || got.Endpoint != "darkbloom/fp4" || !floatEq(got.Pct, -50) {
		t.Fatalf("For = %+v, %v; want darkbloom at -50%% vs Other", got, ok)
	}
	if !floatEq(got.Was, flat(0.1, 0.4)) {
		t.Errorf("Was = %v, want Other's %v", got.Was, flat(0.1, 0.4))
	}
	// equal to the cheapest other provider: no saving
	d.Offers[1] = flatOffer("Other", "other/bf16", 0.05, 0.2)
	if got, ok := d.For(Require{}, DefaultWorkload); ok {
		t.Errorf("tie with Other: For = %+v, want no discount", got)
	}
	// the only provider: nothing to compare with, ForAny still prices it
	solo := Discount{ModelID: "m/x", Offers: d.Offers[:1]}
	if _, ok := solo.For(Require{}, DefaultWorkload); ok {
		t.Error("darkbloom alone: want no discount")
	}
	if got, ok := solo.ForAny(Require{}, DefaultWorkload); !ok || got.Pct != 0 {
		t.Errorf("darkbloom alone ForAny = %+v, %v; want Pct 0", got, ok)
	}
}

// TestDarkbloomReferenceIgnoresCeilings: a dear alternative above the price
// ceilings is still what the request would cost elsewhere.
func TestDarkbloomReferenceIgnoresCeilings(t *testing.T) {
	d := Discount{ModelID: "m/x", Offers: []Offer{
		flatOffer(DarkbloomProvider, "darkbloom/fp4", 0.05, 0.2),
		flatOffer("Dear", "dear/bf16", 1, 4),
	}}
	got, ok := d.For(Require{Capped: true}, DefaultWorkload)
	if !ok || got.Pct >= 0 {
		t.Errorf("For(Capped) = %+v, %v; want a discount vs the dear provider", got, ok)
	}
}

// TestDarkbloomOwnDiscountCounts: a discount OpenRouter lists on the
// darkbloom endpoint itself counts even without a cheaper rival.
func TestDarkbloomOwnDiscountCounts(t *testing.T) {
	o := flatOffer(DarkbloomProvider, "darkbloom/fp4", 0.05, 0.2)
	o.List = Tariff{Base: Rates{Prompt: 0.1, Completion: 0.4}}
	d := Discount{ModelID: "m/x", Offers: []Offer{o, flatOffer("Other", "other/bf16", 0.07, 0.28)}}
	got, ok := d.For(Require{}, DefaultWorkload)
	if !ok || !floatEq(got.Pct, -50) {
		t.Errorf("For = %+v, %v; want -50%% vs its own list price", got, ok)
	}
}

func TestBlindOfferFailsVisionNeed(t *testing.T) {
	blind := flatOffer(DarkbloomProvider, "db:text-only", 0.05, 0.2)
	blind.Blind = true
	d := Discount{ModelID: "m/x", Offers: []Offer{blind, flatOffer("Other", "other/bf16", 0.1, 0.4)}}
	if got, _ := d.ForAny(Require{Vision: true}, DefaultWorkload); got.Endpoint != "other/bf16" {
		t.Errorf("vision pick = %s, want other/bf16 (the Darkbloom build is text-only)", got.Endpoint)
	}
	if got, _ := d.ForAny(Require{}, DefaultWorkload); got.Endpoint != "db:text-only" {
		t.Errorf("text pick = %s, want the cheaper text-only offer", got.Endpoint)
	}
}

func TestUsableKeepsDarkbloomFP4(t *testing.T) {
	got := usable([]Endpoint{
		{Provider: DarkbloomProvider, Tag: "darkbloom/fp4", Quantization: "fp4"},
		{Provider: "Lowbit", Tag: "lowbit/int4", Quantization: "int4"},
		{Provider: "Full", Tag: "full/bf16", Quantization: "bf16"},
	})
	tags := make([]string, len(got))
	for i, e := range got {
		tags[i] = e.Tag
	}
	if len(tags) != 2 || tags[0] != "darkbloom/fp4" || tags[1] != "full/bf16" {
		t.Errorf("usable = %v, want [darkbloom/fp4 full/bf16]", tags)
	}
}

func TestDirectRows(t *testing.T) {
	at := time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC)
	priced := []Discount{
		{ModelID: "m/cheap", Offers: []Offer{
			flatOffer(DarkbloomProvider, "darkbloom/fp4", 0.05, 0.2), // never a reference
			flatOffer("Other", "other/bf16", 0.1, 0.4),
		}},
		{ModelID: "m/dear", Offers: []Offer{flatOffer("Other", "other/bf16", 0.01, 0.04)}},
	}
	models := []DirectModel{
		{ID: "cheap-4bit", BaseID: "m/cheap", Pricing: Pricing{Prompt: "0.00000005", Completion: "0.0000002"}, Tools: true, Context: 262144},
		{ID: "cheap-8bit", BaseID: "m/cheap", Pricing: Pricing{Prompt: "0.00000006", Completion: "0.0000002"}},
		{ID: "dear", BaseID: "m/dear", Pricing: Pricing{Prompt: "0.00000005", Completion: "0.0000002"}},
		{ID: "solo", BaseID: "m/solo", Pricing: Pricing{Prompt: "0.00000005", Completion: "0.0000002"}},
		{ID: "unpriced", BaseID: "m/unpriced", Pricing: Pricing{Prompt: "0"}},
	}
	discs, all := Direct(models, priced, at)
	if len(all) != 3 || all[0].ModelID != "m/cheap" || all[1].ModelID != "m/dear" || all[2].ModelID != "m/solo" {
		t.Fatalf("priced = %+v, want rows for m/cheap, m/dear, m/solo", all)
	}
	if len(discs) != 1 || discs[0].ModelID != "m/cheap" {
		t.Fatalf("discs = %+v, want only m/cheap (undercuts Other)", discs)
	}
	checkCheapRow(t, discs[0])
	if all[1].Pct != 0 || all[2].Pct != 0 {
		t.Errorf("dear/solo Pct = %v/%v, want 0 (no cheaper than others / nothing to compare)", all[1].Pct, all[2].Pct)
	}
}

func checkCheapRow(t *testing.T, c Discount) {
	t.Helper()
	if c.Endpoint != DirectPrefix+"cheap-4bit" || c.Provider != DirectProvider || !floatEq(c.Pct, -50) {
		t.Errorf("m/cheap = %+v, want cheap-4bit at -50%%", c)
	}
	if len(c.Serves) != 2 || !IsDirect(c.Serves[0]) || !IsDirect(c.Serves[1]) {
		t.Errorf("Serves = %v, want both direct tags only", c.Serves)
	}
	if id, ok := DirectID(c.Endpoint); !ok || id != "cheap-4bit" {
		t.Errorf("DirectID = %q, %v", id, ok)
	}
	if !c.Offers[0].Direct || c.Offers[0].Context != 262144 || !c.Offers[0].Tools {
		t.Errorf("offer = %+v, want direct with its own limits", c.Offers[0])
	}
}
