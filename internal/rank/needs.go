package rank

import "github.com/tomaszjudym/openrouter-discounter/internal/discounts"

// PersonaMaxTokens is the strongest-discount persona presets' max_tokens.
// Hermes sends none, so it caps every main-model reply, including long tool
// calls such as file writes; pinned endpoints must allow at least this much.
const PersonaMaxTokens = 65536

// Need is what a preset asks of its pinned endpoints and the request shape
// its prices are compared under.
type Need struct {
	Require  discounts.Require
	Workload discounts.Workload
}

// Request profiles per preset. Token counts are typical per-request sizes;
// tune them to observed usage.
var (
	// personaWorkload is a chat turn with high reasoning effort.
	personaWorkload = discounts.Workload{Input: 8_000, Output: 3_000, CacheHit: 0.7, Reasoning: 0.6}
	// delegateWorkload is an agent subtask: long cached context, tool calls.
	delegateWorkload = discounts.Workload{Input: 40_000, Output: 2_000, CacheHit: 0.85, Reasoning: 0.5}
	// reviewWorkload reads a fresh transcript slice, so less of it is cached.
	reviewWorkload = discounts.Workload{Input: 60_000, Output: 1_500, CacheHit: 0.5, Reasoning: 0.5}
	// compressWorkload summarizes a long, mostly uncached history.
	compressWorkload = discounts.Workload{Input: 200_000, Output: 4_000, CacheHit: 0.1, Reasoning: 0.1}
	// visionWorkload describes one image without reasoning.
	visionWorkload = discounts.Workload{Input: 5_000, Output: 500}
	// lightWorkload is a short one-shot (titles, approvals) without reasoning.
	lightWorkload = discounts.Workload{Input: 2_000, Output: 200}
)

// needs maps the preset sectors that re-pick endpoints; the others (the
// report's top-10 sectors) use the report pick: Require{}, DefaultWorkload.
// Capped marks pools drawn from the affordable rows, so the re-pick stays
// within the price ceilings too.
var needs = map[Sector]Need{
	ExpensiveLong:              {discounts.Require{}, discounts.LongWorkload},
	StrongestDiscountHalflight: {discounts.Require{Tools: true, Capped: true, MinOutput: PersonaMaxTokens}, personaWorkload},
	HermesDelegate:             {discounts.Require{Tools: true, Capped: true}, delegateWorkload},
	HermesReview:               {discounts.Require{Tools: true, Capped: true}, reviewWorkload},
	HermesCompress:             {discounts.Require{MinContext: compressMinCtx, Capped: true}, compressWorkload},
	HermesVision:               {discounts.Require{Capped: true}, visionWorkload},
	HermesLight:                {discounts.Require{Capped: true}, lightWorkload},
}

// Need is the sector's endpoint requirement and workload.
func (s Sector) Need() Need {
	if n, ok := needs[s]; ok {
		return n
	}
	return Need{Workload: discounts.DefaultWorkload}
}

// pick re-picks d's endpoint for the sector's need.
func (s Sector) pick(d discounts.Discount) (discounts.Discount, bool) {
	n := s.Need()
	return d.For(n.Require, n.Workload)
}

// rowOf renders a picked discount as a ranking row.
func rowOf(d discounts.Discount) Row {
	return Row{
		ModelID: d.ModelID, Provider: d.Provider, Endpoint: d.Endpoint, Serves: d.Serves,
		Price: d.Price, Was: d.Was, Pct: d.Pct, PeakPrompt: d.PeakPrompt, PeakCompletion: d.PeakCompletion,
	}
}
