// Package rank orders discounted models per sector, using Artificial
// Analysis benchmark scores when available and price-only order otherwise.
package rank

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

// Sector is one ranked benchmark domain.
type Sector string

// The ranked sectors, in report order.
const (
	Math          Sector = "math"
	LCR           Sector = "long-context-reasoning"
	Finance       Sector = "finance"
	Code          Sector = "code"
	CodeFree      Sector = "code-free"
	ExpensiveLong Sector = "expensive-long"
)

// Label is the display header for the sector.
func (s Sector) Label() string {
	switch s {
	case Math:
		return "MATH — top 10 discounted (AA Math Index)"
	case LCR:
		return "LONG-CONTEXT REASONING — top 10 discounted (AA-LCR)"
	case Finance:
		return "FINANCE — top 10 discounted (τ³-Banking)"
	case Code:
		return "CODE — top 10 discounted (AA Coding Index)"
	case CodeFree:
		return "CODE-FREE — top 10 free coding models (AA Coding Index)"
	case ExpensiveLong:
		return "EXPENSIVE-LONG — top 3 strongest thinking frontier"
	}
	return string(s)
}

// Sectors lists the ranked sectors in report order.
var Sectors = []Sector{Math, LCR, Finance, Code, CodeFree}

// scoreWeight is the share of the AA benchmark in the composite ranking
// score; the remainder weights the discount (−Δ%). Both terms are 0-100, so
// 0.65 means the benchmark counts ~2× the price.
const scoreWeight = 0.65

// Row is one ranked model. Price and Was are USD per Mtok; Pct is negative
// for a discount. Score is the sector benchmark value (0-100) when Scored.
type Row struct {
	ModelID string
	Price   float64
	Was     float64
	Pct     float64
	Score   float64
	Scored  bool
}

// SectorResult is one sector's ranking. A non-empty Err marks the sector as
// price-only and carries the sanitized error for the notice line.
type SectorResult struct {
	Sector Sector
	Rows   []Row
	Err    string
}

// Provider supplies per-sector benchmark scores keyed by OpenRouter model
// id. Errors returned must already be sanitized for display. Implementations
// may fetch lazily; Scores is called once per run.
type Provider interface {
	Scores(ctx context.Context) (map[Sector]map[string]float64, error)
}

// noScoresMessage is the synthetic error for a sector whose source returned
// no usable scores. It contains no secrets.
const noScoresMessage = "no scores returned for this sector"

// Result is the full ranking. A non-empty GlobalErr marks every sector as
// price-only and is surfaced once at the top of the message.
type Result struct {
	Sectors   []SectorResult
	GlobalErr string
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

// Rank builds the per-sector top-10 rankings. With scores available, scored
// models rank by score descending, then by discount magnitude; unscored
// models rank after scored ones by discount magnitude with score n/a.
// Without scores for a sector (provider error, or no scores for that
// sector), the sector ranks by discount magnitude only.
//
// The CodeFree sector ranks free models (freeIDs) by AA Coding Index score
// only; price columns are omitted. When scores are unavailable the sector
// lists free models alphabetically with a notice.
func Rank(ctx context.Context, discs []discounts.Discount, freeIDs []string, prov Provider) Result {
	scores, err := prov.Scores(ctx)
	res := Result{GlobalErr: errString(err)}
	for _, sec := range Sectors {
		if sec == CodeFree {
			res.Sectors = append(res.Sectors, codeFreeResult(freeIDs, scores, err))
		} else {
			res.Sectors = append(res.Sectors, sectorResult(sec, discs, scores, err))
		}
	}
	return res
}

func sectorResult(sec Sector, discs []discounts.Discount, scores map[Sector]map[string]float64, globalErr error) SectorResult {
	sr := SectorResult{Sector: sec}
	secScores := scores[sec]
	if globalErr == nil && len(secScores) == 0 {
		sr.Err = noScoresMessage
	}
	rows := make([]Row, 0, len(discs))
	for _, d := range discs {
		row := Row{ModelID: d.ModelID, Price: d.Price, Was: d.Was, Pct: d.Pct}
		if s, ok := secScores[d.ModelID]; ok {
			row.Score, row.Scored = s, true
		}
		rows = append(rows, row)
	}
	sortRows(rows, secScores == nil || sr.Err != "")
	sr.Rows = top10(rows)
	return sr
}

func codeFreeResult(freeIDs []string, scores map[Sector]map[string]float64, globalErr error) SectorResult {
	sr := SectorResult{Sector: CodeFree}
	secScores := scores[CodeFree]
	if globalErr == nil && len(secScores) == 0 {
		sr.Err = noScoresMessage
	}
	rows := make([]Row, 0, len(freeIDs))
	for _, id := range freeIDs {
		row := Row{ModelID: id}
		if s, ok := secScores[id]; ok {
			row.Score, row.Scored = s, true
		}
		rows = append(rows, row)
	}
	sr.Rows = top10(sortRowsFree(rows))
	return sr
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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

func top10(rows []Row) []Row {
	if len(rows) > 10 {
		return rows[:10]
	}
	return rows
}
