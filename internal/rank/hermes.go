package rank

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

// Hermes role sectors: one preset per Hermes Agent model slot.
const (
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
	SourceCheapness    = "blended price per Mtok"
)

// Role tuning.
const (
	// diversityPenalty is subtracted from the composite of a model already
	// #1 in an earlier role, so it repeats only when clearly best — unless
	// it is cheapEnough for this role, then it may serve many roles.
	diversityPenalty = 15.0
	// cheapEnough is the cheapness (0-100, see cheapness) at or above which
	// the diversity penalty is waived: blended price ≤ 20% of the ceiling.
	cheapEnough    = 80.0
	compressMinCtx = 1_000_000
	lightMinII     = 20.0
	// reviewQualityPercent weights quality in the review composite; the
	// rest weights cheapness. Review output is acted on, so quality leads.
	reviewQualityPercent = 80
	// minCodeScored is how many code-scored candidates the delegate role
	// needs before it ranks by code score instead of intelligence.
	minCodeScored = 3
	roleTopN      = 3
)

// blendedCeiling is the blended price under w, without cache discount, at
// both affordability ceilings.
func blendedCeiling(w discounts.Workload) float64 {
	return discounts.Rates{Prompt: discounts.MaxPrompt, Completion: discounts.MaxCompletion}.Blended(w)
}

// candidate is one discounted, catalog-listed model eligible for roles.
type candidate struct {
	model discounts.Model
	disc  discounts.Discount
	ii    float64
}

// pricing is how a role's composite prices its candidates.
type pricing int

const (
	// byDiscount ranks discounted models only, by quality and discount
	// depth (the shared composite, see weights.go).
	byDiscount pricing = iota
	// byCost ranks every priced model, discounted or not, by absolute
	// cheapness: a list-price model that costs less beats a pricier
	// discounted one.
	byCost
)

// role selects and scores candidates for one Hermes slot.
type role struct {
	sector  Sector
	keep    func(c candidate) bool
	pricing pricing
}

// hermesRoles lists the roles in pick order: earlier roles claim their #1
// first, later roles pay the diversity penalty for reusing it.
var hermesRoles = []role{
	{HermesDelegate, func(c candidate) bool { return c.model.ToolCalling() }, byDiscount},
	{HermesVision, func(c candidate) bool { return c.model.Vision() }, byDiscount},
	{HermesCompress, func(c candidate) bool { return c.model.ContextLength >= compressMinCtx }, byDiscount},
	{HermesReview, func(c candidate) bool { return c.model.ToolCalling() }, byCost},
	// light answers titles and approvals inline: a model that always
	// reasons is too slow for it, whatever its price
	{HermesLight, func(c candidate) bool { return c.ii >= lightMinII && !c.model.MandatoryReasoning() }, byCost},
}

// SelectHermes picks the top 3 models for each Hermes role. byDiscount
// roles draw from the discounted rows (callers pass the affordable ones)
// and rank by the shared composite (weights.go: quality vs discount
// depth). byCost roles draw from every priced row and rank by absolute
// cost: review by reviewQualityPercent quality plus cheapness, light by
// cheapness alone, 100·(1 − blended/ceiling). Quality is the min-max
// scaled AA Intelligence Index, except delegate uses the scaled code score
// when at least 3 of its candidates have one. A model already #1 in an
// earlier role loses diversityPenalty unless it is cheapEnough for the
// role. Roles with no eligible model are omitted.
func SelectHermes(models []discounts.Model, discs, priced []discounts.Discount, intel, code map[string]float64) []SectorResult {
	pools := map[pricing][]candidate{
		byDiscount: hermesPool(models, discs, intel),
		byCost:     hermesPool(models, priced, intel),
	}
	taken := make(map[string]bool)
	var out []SectorResult
	for _, r := range hermesRoles {
		var cands []candidate
		for _, c := range pools[r.pricing] {
			if r.keep(c) {
				cands = append(cands, c)
			}
		}
		sr := scoreRole(r, cands, code, taken)
		if len(sr.Rows) == 0 {
			continue
		}
		taken[sr.Rows[0].ModelID] = true
		out = append(out, sr)
	}
	return out
}

// hermesPool keeps chat models with a positive intelligence index from
// rows, excluding :free and :batch variants; one row per model id.
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
func scoreRole(r role, pool []candidate, code map[string]float64, taken map[string]bool) SectorResult {
	need := r.sector.Need()
	cands := repick(r, pool)
	cheap := make(map[string]float64, len(cands))
	for _, c := range cands {
		cheap[c.disc.ModelID] = cheapness(c.disc.Price, need.Workload)
	}
	source, cands, raw := roleScores(r.sector, cands, cheap, code)
	type ranked struct {
		Row
		value float64
	}
	rows := make([]ranked, 0, len(cands))
	for _, c := range cands {
		row := rowOf(c.disc)
		row.Score, row.Scored = raw[c.disc.ModelID], true
		v := roleValue(r.sector, r.pricing, row, cheap[row.ModelID])
		if taken[row.ModelID] && cheap[row.ModelID] < cheapEnough {
			v -= diversityPenalty
		}
		rows = append(rows, ranked{row, v})
	}
	slices.SortStableFunc(rows, func(a, b ranked) int {
		if c := cmp.Compare(b.value, a.value); c != 0 {
			return c
		}
		if r.pricing == byCost {
			return cmp.Compare(a.Price, b.Price)
		}
		return byPct(a.Row, b.Row)
	})
	ordered := make([]Row, len(rows))
	for i, r := range rows {
		ordered[i] = r.Row
	}
	return SectorResult{Sector: r.sector, Source: source, Rows: Pinnable(ordered, roleTopN)}
}

// repick re-picks each candidate's endpoint for the role's Need; byCost
// roles may pick an undiscounted endpoint. Candidates without a fitting
// endpoint drop out.
func repick(r role, pool []candidate) []candidate {
	need := r.sector.Need()
	out := make([]candidate, 0, len(pool))
	for _, c := range pool {
		pick := c.disc.For
		if r.pricing == byCost {
			pick = c.disc.ForAny
		}
		if p, ok := pick(need.Require, need.Workload); ok {
			c.disc = p
			out = append(out, c)
		}
	}
	return out
}

// roleScores is the role's quality score per model and its source: light
// scores cheapness; delegate the scaled code score when at least
// minCodeScored candidates have one (keeping only those); the rest the
// scaled intelligence index.
func roleScores(sec Sector, cands []candidate, cheap, code map[string]float64) (string, []candidate, map[string]float64) {
	raw := make(map[string]float64, len(cands))
	if sec == HermesLight {
		maps.Copy(raw, cheap)
		return SourceCheapness, cands, raw
	}
	if sec == HermesDelegate {
		var scored []candidate
		for _, c := range cands {
			if code[c.disc.ModelID] > 0 {
				scored = append(scored, c)
			}
		}
		if len(scored) >= minCodeScored {
			for _, c := range scored {
				raw[c.disc.ModelID] = code[c.disc.ModelID]
			}
			minMax(raw)
			return SourceCode, scored, raw
		}
	}
	for _, c := range cands {
		raw[c.disc.ModelID] = c.ii
	}
	minMax(raw)
	return SourceIntelligence, cands, raw
}

// roleValue is a candidate's ranking value before the diversity penalty.
// byDiscount roles use the shared composite; light ranks by its score
// (cheapness); review mixes quality and cheapness.
func roleValue(sec Sector, p pricing, row Row, cheap float64) float64 {
	switch {
	case p == byDiscount:
		return composite(row)
	case sec == HermesLight:
		return row.Score
	default:
		q := reviewQualityPercent / 100.0
		return q*row.Score + (1-q)*cheap
	}
}

// cheapness scores a blended per-Mtok price under w against the ceiling,
// 0-100: 100 is free, 0 at or above the ceiling.
func cheapness(price float64, w discounts.Workload) float64 {
	return min(100, max(0, 100*(1-price/blendedCeiling(w))))
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
