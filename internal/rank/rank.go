// Package rank orders discounted models per sector, using per-sector scores
// (benchmarks or popularity) when available and price-only order otherwise.
package rank

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

// Sector is one ranked benchmark domain.
type Sector string

// The ranked sectors, in report order.
const (
	Math                          Sector = "math"
	LCR                           Sector = "long-context-reasoning"
	Finance                       Sector = "finance"
	Code                          Sector = "code"
	CodeFree                      Sector = "code-free"
	ExpensiveLong                 Sector = "expensive-long"
	StrongestDiscountHalflight    Sector = "strongest-discount-halflight"
	StrongestDiscountVolition     Sector = "strongest-discount-volition"
	StrongestDiscountInlandEmpire Sector = "strongest-discount-inlandempire"
	StrongestDiscountShivers      Sector = "strongest-discount-shivers"
	FreeHalflight                 Sector = "free-halflight"
	FreeVolition                  Sector = "free-volition"
	FreeInlandEmpire              Sector = "free-inlandempire"
	FreeShivers                   Sector = "free-shivers"
)

// Label is the display header for the sector.
func (s Sector) Label() string {
	switch s {
	case Math:
		return "MATH — top 10 discounted (Epoch OTIS Mock AIME)"
	case LCR:
		return "LONG-CONTEXT — top 10 discounted (OpenRouter popularity)"
	case Finance:
		return "FINANCE — top 10 discounted (AA Finance & Accounting)"
	case Code:
		return "CODE — top 10 discounted (Epoch WebDev Arena)"
	case CodeFree:
		return "CODE-FREE — top 10 free coding models (Epoch WebDev Arena)"
	case ExpensiveLong:
		return "EXPENSIVE-LONG — top 3 strongest thinking frontier"
	case StrongestDiscountHalflight:
		return "STRONGEST-DISCOUNT-HALFLIGHT — top 3 strongest-thinking discounted"
	case StrongestDiscountVolition:
		return "STRONGEST-DISCOUNT-VOLITION — top 3 strongest-thinking discounted"
	case StrongestDiscountInlandEmpire:
		return "STRONGEST-DISCOUNT-INLANDEMPIRE — top 3 strongest-thinking discounted"
	case StrongestDiscountShivers:
		return "STRONGEST-DISCOUNT-SHIVERS — top 3 strongest-thinking discounted"
	case FreeHalflight:
		return "FREE-HALFLIGHT — top 3 free models (AA Intelligence Index)"
	case FreeVolition:
		return "FREE-VOLITION — top 3 free models (AA Intelligence Index)"
	case FreeInlandEmpire:
		return "FREE-INLANDEMPIRE — top 3 free models (AA Intelligence Index)"
	case FreeShivers:
		return "FREE-SHIVERS — top 3 free models (AA Intelligence Index)"
	}
	return string(s)
}

// Sectors lists the ranked sectors in report order.
var Sectors = []Sector{Math, LCR, Finance, Code, CodeFree}

// scoreWeight is the share of the sector score in the composite ranking
// score; the remainder weights the discount (−Δ%). Both terms are 0-100, so
// 0.65 means the benchmark counts ~2× the price.
const scoreWeight = 0.65

// Row is one ranked model. Price and Was are USD per Mtok; Pct is negative
// for a discount. Score is the sector score (0-100) when Scored.
type Row struct {
	ModelID string
	Price   float64
	Was     float64
	Pct     float64
	Score   float64
	Scored  bool
}

// SectorResult is one sector's ranking. A non-empty Err marks the sector as
// price-only and carries the sanitized error for the notice line; Source
// names the score source the error came from.
type SectorResult struct {
	Sector Sector
	Rows   []Row
	Source string
	Err    string
}

// SectorScores is one source's scores for a sector, keyed by OpenRouter
// model id, 0-100. A non-empty Err (already sanitized for display) marks the
// source as failed for this sector.
type SectorScores struct {
	Source  string
	ByModel map[string]float64
	Err     string
}

// Provider supplies per-sector scores. Implementations return only the
// sectors they cover; Scores is called once per run.
type Provider interface {
	Scores(ctx context.Context) map[Sector]SectorScores
}

// Providers combines several sources into one Provider. The sources are
// fetched concurrently, so wall time is bounded by the slowest one. When two
// sources cover the same sector, the first one listed wins.
type Providers []Provider

// Scores implements Provider.
func (ps Providers) Scores(ctx context.Context) map[Sector]SectorScores {
	results := make([]map[Sector]SectorScores, len(ps))
	var wg sync.WaitGroup
	for i, p := range ps {
		wg.Go(func() { results[i] = p.Scores(ctx) })
	}
	wg.Wait()
	out := make(map[Sector]SectorScores)
	for _, r := range results {
		for sec, sc := range r {
			if _, ok := out[sec]; !ok {
				out[sec] = sc
			}
		}
	}
	return out
}

// noScoresMessage is the synthetic error for a sector whose source returned
// no usable scores. It contains no secrets.
const noScoresMessage = "no scores returned for this sector"

// NoSourceName labels the notice of a sector no provider covers, or whose
// provider named no source.
const NoSourceName = "score source"

// Result is the full ranking.
type Result struct {
	Sectors []SectorResult
}

// SelectExpensiveLong picks the 3 strongest thinking-capable frontier models
// from the catalog (highest AA Intelligence Index, non-free, non-batch, with
// reasoning support). Discounted rows lead, deepest discount first; the rest
// keep their strength order.
func SelectExpensiveLong(models []discounts.Model, discs []discounts.Discount) SectorResult {
	cands := make([]Row, 0, 8)
	for _, m := range models {
		if strings.HasSuffix(m.ID, ":free") || strings.HasSuffix(m.ID, ":batch") {
			continue
		}
		ii := m.Benchmarks.ArtificialAnalysis.IntelligenceIndex
		if ii <= 0 || !m.Thinking() {
			continue
		}
		cands = append(cands, Row{ModelID: m.ID, Score: ii, Scored: true})
	}
	if len(cands) == 0 {
		return SectorResult{Sector: ExpensiveLong}
	}
	slices.SortStableFunc(cands, byScoreDesc)
	if len(cands) > 3 {
		cands = cands[:3]
	}
	pct := make(map[string]float64, len(discs))
	for _, d := range discs {
		pct[d.ModelID] = d.Pct
	}
	// one stable sort: discounted rows first by depth, ties keep strength order
	slices.SortStableFunc(cands, func(a, b Row) int {
		pa, aOk := pct[a.ModelID]
		pb, bOk := pct[b.ModelID]
		switch {
		case aOk != bOk:
			if aOk {
				return -1
			}
			return 1
		case aOk:
			return cmp.Compare(pa, pb)
		}
		return 0
	})
	return SectorResult{Sector: ExpensiveLong, Rows: cands}
}

// SelectStrongestDiscount returns the top 3 discounted models with positive
// thinking scores that can produce text output. Pool is the discounted rows
// whose model satisfies ChatText() and whose id does not end in :free or
// :batch. Only ids with a positive thinking score are included. Order is
// strongest (highest score) to weakest, stable.
func SelectStrongestDiscount(models []discounts.Model, discs []discounts.Discount, thinking map[string]float64) SectorResult {
	if len(thinking) == 0 {
		return SectorResult{Sector: StrongestDiscountHalflight}
	}
	catalog := make(map[string]discounts.Model, len(models))
	for _, m := range models {
		catalog[m.ID] = m
	}
	rows := make([]Row, 0, len(discs))
	for _, d := range discs {
		if strings.HasSuffix(d.ModelID, ":free") || strings.HasSuffix(d.ModelID, ":batch") {
			continue
		}
		m, ok := catalog[d.ModelID]
		if !ok || !m.ChatText() {
			continue
		}
		score, ok := thinking[d.ModelID]
		if !ok || score <= 0 {
			continue
		}
		rows = append(rows, Row{ModelID: d.ModelID, Price: d.Price, Was: d.Was, Pct: d.Pct, Score: score, Scored: true})
	}
	if len(rows) == 0 {
		return SectorResult{Sector: StrongestDiscountHalflight}
	}
	slices.SortStableFunc(rows, byScoreDesc)
	if len(rows) > 3 {
		rows = rows[:3]
	}
	return SectorResult{Sector: StrongestDiscountHalflight, Rows: rows}
}

// harnessOnly lists :free endpoints restricted to agentic harnesses: they
// reject OpenRouter web-chat use, so free presets must not route to them.
// The API carries no flag for this; the source is the model page's
// warning_message ("only available for use with agentic harnesses"),
// verified 2026-11-04.
var harnessOnly = map[string]struct{}{
	"thinkingmachines/inkling:free":       {},
	"thinkingmachines/inkling-small:free": {},
}

// HarnessOnly reports whether a :free model is restricted to agentic
// harnesses and unusable from the OpenRouter web chat.
func HarnessOnly(id string) bool {
	_, ok := harnessOnly[id]
	return ok
}

// SelectFreeDisco returns the top 3 free models ranked by AA Intelligence
// Index. Only models whose id ends in :free, are usable from web chat, and
// have a positive intelligence score are included. Order is strongest
// (highest score) to weakest, stable.
func SelectFreeDisco(freeIDs []string, models []discounts.Model, intelligence map[string]float64) SectorResult {
	if len(intelligence) == 0 {
		return SectorResult{Sector: FreeHalflight}
	}
	catalog := make(map[string]discounts.Model, len(models))
	for _, m := range models {
		catalog[m.ID] = m
	}
	rows := make([]Row, 0, len(freeIDs))
	for _, id := range freeIDs {
		if !strings.HasSuffix(id, ":free") || HarnessOnly(id) {
			continue
		}
		m, ok := catalog[id]
		if !ok || !m.ChatText() {
			continue
		}
		score, ok := intelligence[id]
		if !ok || score <= 0 {
			continue
		}
		rows = append(rows, Row{ModelID: id, Score: score, Scored: true})
	}
	if len(rows) == 0 {
		return SectorResult{Sector: FreeHalflight}
	}
	slices.SortStableFunc(rows, byScoreDesc)
	if len(rows) > 3 {
		rows = rows[:3]
	}
	return SectorResult{Sector: FreeHalflight, Rows: rows}
}

// Rank builds the per-sector top-10 rankings. With scores available, scored
// models rank by composite descending; unscored models rank after scored
// ones by discount magnitude with score n/a. Without scores for a sector
// (source error, or no scores for that sector), the sector ranks by
// discount magnitude only and carries the notice error.
//
// The CodeFree sector ranks free models (freeIDs) by score only; price
// columns are omitted. When scores are unavailable the sector lists free
// models alphabetically with a notice.
func Rank(ctx context.Context, discs []discounts.Discount, freeIDs []string, prov Provider) Result {
	scores := prov.Scores(ctx)
	var res Result
	for _, sec := range Sectors {
		sr, byModel := sectorBase(sec, scores)
		if sec == CodeFree {
			sr.Rows = freeRows(freeIDs, byModel)
		} else {
			sr.Rows = discountRows(discs, byModel)
		}
		res.Sectors = append(res.Sectors, sr)
	}
	return res
}

// sectorBase resolves the sector's source and notice error. The returned
// scores are nil when the sector is price-only.
func sectorBase(sec Sector, scores map[Sector]SectorScores) (SectorResult, map[string]float64) {
	sc, ok := scores[sec]
	sr := SectorResult{Sector: sec, Source: sc.Source}
	switch {
	case !ok:
		sr.Source, sr.Err = NoSourceName, noScoresMessage
	case sc.Err != "":
		sr.Err = sc.Err
	case len(sc.ByModel) == 0:
		sr.Err = noScoresMessage
	default:
		return sr, sc.ByModel
	}
	return sr, nil
}

func discountRows(discs []discounts.Discount, byModel map[string]float64) []Row {
	rows := make([]Row, 0, len(discs))
	for _, d := range discs {
		row := Row{ModelID: d.ModelID, Price: d.Price, Was: d.Was, Pct: d.Pct}
		if s, ok := byModel[d.ModelID]; ok {
			row.Score, row.Scored = s, true
		}
		rows = append(rows, row)
	}
	sortRows(rows, byModel == nil)
	return top10(rows)
}

func freeRows(freeIDs []string, byModel map[string]float64) []Row {
	rows := make([]Row, 0, len(freeIDs))
	for _, id := range freeIDs {
		if HarnessOnly(id) {
			continue
		}
		row := Row{ModelID: id}
		if s, ok := byModel[id]; ok {
			row.Score, row.Scored = s, true
		}
		rows = append(rows, row)
	}
	return top10(sortRowsFree(rows))
}

// sortRows orders scored models by composite descending; unscored models
// rank after scored ones, by discount magnitude (Pct ascending).
func sortRows(rows []Row, priceOnly bool) {
	if priceOnly {
		slices.SortStableFunc(rows, byPct)
		return
	}
	slices.SortStableFunc(rows, func(a, b Row) int {
		switch {
		case a.Scored != b.Scored:
			if a.Scored {
				return -1
			}
			return 1
		case a.Scored:
			if c := cmp.Compare(composite(b), composite(a)); c != 0 {
				return c
			}
		}
		return byPct(a, b)
	})
}

// sortRowsFree orders free models: scored by benchmark descending, unscored
// alphabetically after.
func sortRowsFree(rows []Row) []Row {
	slices.SortStableFunc(rows, func(a, b Row) int {
		switch {
		case a.Scored != b.Scored:
			if a.Scored {
				return -1
			}
			return 1
		case a.Scored:
			return byScoreDesc(a, b)
		}
		return cmp.Compare(a.ModelID, b.ModelID)
	})
	return rows
}

// byScoreDesc orders rows by benchmark score, highest first.
func byScoreDesc(a, b Row) int { return cmp.Compare(b.Score, a.Score) }

// byPct orders rows by discount magnitude, deepest discount first.
func byPct(a, b Row) int { return cmp.Compare(a.Pct, b.Pct) }

// composite is the 0-100 ranking value for a scored row: benchmark weighted
// by scoreWeight, the remainder discount.
func composite(r Row) float64 {
	return scoreWeight*r.Score + (1-scoreWeight)*(-r.Pct)
}

// Candidate is a scored model submitted to ScoreModels: its OpenRouter id,
// benchmark score (0-100), and effective price per Mtok.
type Candidate struct {
	ModelID     string
	CodingScore float64
	Price       float64
}

// ScoreModels orders candidates by composite score (benchmark weighted by
// scoreWeight, price weighted by priceWeight). Price score is 0-100,
// higher for cheaper models.
func ScoreModels(cands []Candidate, scoreWeight, priceWeight float64) []string {
	if len(cands) == 0 {
		return nil
	}
	maxP, minP := cands[0].Price, cands[0].Price
	for _, c := range cands[1:] {
		if c.Price > maxP {
			maxP = c.Price
		}
		if c.Price < minP {
			minP = c.Price
		}
	}
	rangeP := maxP - minP
	scores := make([]float64, len(cands))
	for i, c := range cands {
		priceScore := 100.0
		if rangeP > 0 {
			priceScore = 100.0 * (maxP - c.Price) / rangeP
		}
		scores[i] = scoreWeight*c.CodingScore + priceWeight*priceScore
	}
	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int {
		return cmp.Compare(scores[j], scores[i])
	})
	out := make([]string, len(cands))
	for i, idx := range order {
		out[i] = cands[idx].ModelID
	}
	return out
}

func top10(rows []Row) []Row {
	if len(rows) > 10 {
		return rows[:10]
	}
	return rows
}
