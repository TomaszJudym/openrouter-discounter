// Package rank orders discounted models per sector, using Artificial
// Analysis benchmark scores when available and price-only order otherwise.
package rank

import (
	"cmp"
	"context"
	"slices"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

// Sector is one ranked benchmark domain.
type Sector string

// The ranked sectors, in report order.
const (
	Math    Sector = "math"
	LCR     Sector = "long-context-reasoning"
	Finance Sector = "finance"
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
	}
	return string(s)
}

// Sectors lists the ranked sectors in report order.
var Sectors = []Sector{Math, LCR, Finance}

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

// scoreWeight is the share of the AA benchmark in the composite ranking
// score; the remainder weights the discount (−Δ%). Both terms are 0-100, so
// 0.5 means performance and price count equally.
const scoreWeight = 0.5

// Result is the full ranking. A non-empty GlobalErr marks every sector as
// price-only and is surfaced once at the top of the message.
type Result struct {
	Sectors   []SectorResult
	GlobalErr string
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

// Rank builds the per-sector top-10 rankings. With scores available, scored
// models rank by score descending, then by discount magnitude; unscored
// models rank after scored ones by discount magnitude with score n/a.
// Without scores for a sector (provider error, or no scores for that
// sector), the sector ranks by discount magnitude only.
func Rank(ctx context.Context, discs []discounts.Discount, prov Provider) Result {
	res := Result{}
	scores, err := prov.Scores(ctx)
	if err != nil {
		res.GlobalErr = err.Error()
	}
	for _, sec := range Sectors {
		sr := SectorResult{Sector: sec}
		var secScores map[string]float64
		switch {
		case err != nil:
			// global failure: price-only, notice at top of message
		default:
			secScores = scores[sec]
			if len(secScores) == 0 {
				sr.Err = noScoresMessage
			}
		}
		rows := make([]Row, 0, len(discs))
		for _, d := range discs {
			row := Row{ModelID: d.ModelID, Price: d.Price, Was: d.Was, Pct: d.Pct}
			if secScores != nil {
				if s, ok := secScores[d.ModelID]; ok {
					row.Score, row.Scored = s, true
				}
			}
			rows = append(rows, row)
		}
		sortRows(rows, secScores == nil || sr.Err != "")
		sr.Rows = top10(rows)
		res.Sectors = append(res.Sectors, sr)
	}
	return res
}

// sortRows orders scored models by the composite score (scoreWeight ×
// benchmark + (1−scoreWeight) × discount) descending; unscored models rank
// after scored ones by discount magnitude (Pct ascending).
func sortRows(rows []Row, priceOnly bool) {
	if priceOnly {
		slices.SortStableFunc(rows, func(a, b Row) int { return cmp.Compare(a.Pct, b.Pct) })
		return
	}
	slices.SortStableFunc(rows, func(a, b Row) int {
		if a.Scored != b.Scored {
			if b.Scored {
				return 1
			}
			return -1
		}
		if a.Scored {
			if c := cmp.Compare(final(b), final(a)); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.Pct, b.Pct)
	})
}

// final is the composite 0-100 ranking value for a scored row.
func final(r Row) float64 {
	return scoreWeight*r.Score + (1-scoreWeight)*(-r.Pct)
}

func top10(rows []Row) []Row {
	if len(rows) > 10 {
		return rows[:10]
	}
	return rows
}
