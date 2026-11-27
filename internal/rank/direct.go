package rank

import (
	"cmp"
	"slices"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
)

// DarkbloomDirect is the report's Darkbloom-direct price list; no preset
// follows it.
const DarkbloomDirect Sector = "darkbloom-direct"

// SelectDirect lists the Darkbloom-direct rows (see discounts.Direct) by
// blended price, cheapest first, scored by the intelligence index where
// known. Was is the cheapest other provider's price, so Δ% shows the saving.
func SelectDirect(priced []discounts.Discount, intel map[string]float64) SectorResult {
	rows := make([]Row, 0, len(priced))
	for _, d := range priced {
		row := rowOf(d)
		if s, ok := intel[d.ModelID]; ok && s > 0 {
			row.Score, row.Scored = s, true
		}
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(a, b Row) int { return cmp.Compare(a.Price, b.Price) })
	return SectorResult{Sector: DarkbloomDirect, Rows: top10(rows)}
}
