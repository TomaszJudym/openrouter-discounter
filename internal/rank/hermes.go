package rank

import (
	"cmp"
	"slices"
	"strings"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

// Hermes role sectors: one preset per Hermes Agent model slot.
const (
	HermesMain     Sector = "hermes-main"
	HermesDelegate Sector = "hermes-delegate"
	HermesVision   Sector = "hermes-vision"
	HermesCompress Sector = "hermes-compress"
	HermesReview   Sector = "hermes-review"
	HermesLight    Sector = "hermes-light"
)

// Quality sources named in SectorResult.Source for the hermes roles.
const (
	SourceIntelligence = "AA Intelligence Index"
	SourceCode         = "Epoch WebDev Arena"
	SourceCheapness    = "price per Mtok"
)

// Role tuning.
const (
	// diversityPenalty is subtracted from the composite of a model already
	// #1 in an earlier role, so it repeats only when clearly best.
	diversityPenalty = 15.0
	mainMinContext   = 256_000
	compressMinCtx   = 1_000_000
	lightMinII       = 20.0
	// minCodeScored is how many code-scored candidates the delegate role
	// needs before it ranks by code score instead of intelligence.
	minCodeScored = 3
	roleTopN      = 3
)

// blendedPrice weights prompt over completion: agent traffic is
// input-heavy. Prices are USD per Mtok.
func blendedPrice(prompt, completion float64) float64 {
	return 0.8*prompt + 0.2*completion
}

// blendedCeiling is the blended price at both affordability ceilings.
var blendedCeiling = blendedPrice(discounts.MaxPrompt, discounts.MaxCompletion)

// candidate is one discounted, catalog-listed model eligible for roles.
type candidate struct {
	model discounts.Model
	disc  discounts.Discount
	ii    float64
}

// role selects and scores candidates for one Hermes slot.
type role struct {
	sector Sector
	keep   func(c candidate) bool
}

// hermesRoles lists the roles in pick order: earlier roles claim their #1
// first, later roles pay the diversity penalty for reusing it.
var hermesRoles = []role{
	{HermesMain, func(c candidate) bool {
		return c.model.ToolCalling() && c.model.Thinking() && c.model.ContextLength >= mainMinContext
	}},
	{HermesDelegate, func(c candidate) bool { return c.model.ToolCalling() }},
	{HermesVision, func(c candidate) bool { return c.model.Vision() }},
	{HermesCompress, func(c candidate) bool { return c.model.ContextLength >= compressMinCtx }},
	{HermesReview, func(c candidate) bool { return c.model.ToolCalling() }},
	{HermesLight, func(c candidate) bool { return c.ii >= lightMinII }},
}

// SelectHermes picks the top 3 models for each Hermes role from the
// discounted rows (callers pass the affordable ones). Every role ranks by
// composite (0.75 quality, 0.25 discount) minus the diversity penalty.
// Quality is the min-max scaled AA Intelligence Index, except: delegate
// uses the scaled code score when at least 3 of its candidates have one;
// light uses cheapness, 100·(1 − blended/ceiling). Roles with no eligible
// model are omitted.
func SelectHermes(models []discounts.Model, discs []discounts.Discount, intel, code map[string]float64) []SectorResult {
	pool := hermesPool(models, discs, intel)
	taken := make(map[string]bool)
	var out []SectorResult
	for _, r := range hermesRoles {
		var cands []candidate
		for _, c := range pool {
			if r.keep(c) {
				cands = append(cands, c)
			}
		}
		sr := scoreRole(r.sector, cands, code, taken)
		if len(sr.Rows) == 0 {
			continue
		}
		taken[sr.Rows[0].ModelID] = true
		out = append(out, sr)
	}
	return out
}

// hermesPool keeps discounted chat models with a positive intelligence
// index, excluding :free and :batch variants; one row per model id.
func hermesPool(models []discounts.Model, discs []discounts.Discount, intel map[string]float64) []candidate {
	catalog := make(map[string]discounts.Model, len(models))
	for _, m := range models {
		catalog[m.ID] = m
	}
	seen := make(map[string]bool, len(discs))
	var pool []candidate
	for _, d := range discs {
		if seen[d.ModelID] || strings.HasSuffix(d.ModelID, ":free") || strings.HasSuffix(d.ModelID, ":batch") {
			continue
		}
		m, ok := catalog[d.ModelID]
		if !ok || !m.ChatText() {
			continue
		}
		ii := intel[d.ModelID]
		if ii <= 0 {
			continue
		}
		seen[d.ModelID] = true
		pool = append(pool, candidate{model: m, disc: d, ii: ii})
	}
	return pool
}

// scoreRole ranks one role's candidates and keeps the top 3.
func scoreRole(sec Sector, cands []candidate, code map[string]float64, taken map[string]bool) SectorResult {
	sr := SectorResult{Sector: sec, Source: SourceIntelligence}
	raw := make(map[string]float64, len(cands))
	switch sec {
	case HermesLight:
		sr.Source = SourceCheapness
		for _, c := range cands {
			raw[c.disc.ModelID] = 100 * (1 - blendedPrice(c.disc.Price, c.disc.Completion)/blendedCeiling)
		}
	case HermesDelegate:
		var scored []candidate
		for _, c := range cands {
			if code[c.disc.ModelID] > 0 {
				scored = append(scored, c)
			}
		}
		if len(scored) >= minCodeScored {
			sr.Source, cands = SourceCode, scored
			for _, c := range cands {
				raw[c.disc.ModelID] = code[c.disc.ModelID]
			}
			minMax(raw)
			break
		}
		fallthrough
	default:
		for _, c := range cands {
			raw[c.disc.ModelID] = c.ii
		}
		minMax(raw)
	}
	type ranked struct {
		Row
		value float64
	}
	rows := make([]ranked, 0, len(cands))
	for _, c := range cands {
		row := Row{ModelID: c.disc.ModelID, Price: c.disc.Price, Was: c.disc.Was, Pct: c.disc.Pct, Score: raw[c.disc.ModelID], Scored: true}
		v := composite(row)
		if taken[row.ModelID] {
			v -= diversityPenalty
		}
		rows = append(rows, ranked{row, v})
	}
	slices.SortStableFunc(rows, func(a, b ranked) int {
		if c := cmp.Compare(b.value, a.value); c != 0 {
			return c
		}
		return byPct(a.Row, b.Row)
	})
	for i := range min(roleTopN, len(rows)) {
		sr.Rows = append(sr.Rows, rows[i].Row)
	}
	return sr
}

// minMax rescales the values to 0-100 in place; a single distinct value
// maps to 100.
func minMax(v map[string]float64) {
	if len(v) == 0 {
		return
	}
	lo, hi := 0.0, 0.0
	first := true
	for _, x := range v {
		if first {
			lo, hi, first = x, x, false
			continue
		}
		lo, hi = min(lo, x), max(hi, x)
	}
	for k, x := range v {
		if hi == lo {
			v[k] = 100
			continue
		}
		v[k] = 100 * (x - lo) / (hi - lo)
	}
}
