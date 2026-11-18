package rank

import (
	"context"
	"maps"
)

// minFit is the fewest models scored by both sources for the calibration
// fit; below it, or with a non-positive slope, fallback values are used
// unscaled.
const minFit = 10

// Backfill wraps a Provider and fills the models it leaves unscored in
// Sectors from Scores, a second 0-100 source keyed by OpenRouter id. The
// fallback values are mapped onto the primary scale by a least-squares
// line fitted over the models both sources score, then clamped to 0-100.
// A sector whose primary source failed (Err set, or no scores) stays
// price-only: there is nothing to calibrate against.
type Backfill struct {
	Provider Provider
	Sectors  []Sector
	Fallback map[string]float64
}

// Scores implements Provider.
func (b Backfill) Scores(ctx context.Context) map[Sector]SectorScores {
	out := b.Provider.Scores(ctx)
	for _, sec := range b.Sectors {
		sc, ok := out[sec]
		if !ok || sc.Err != "" || len(sc.ByModel) == 0 {
			continue
		}
		sc.ByModel = backfill(sc.ByModel, b.Fallback)
		out[sec] = sc
	}
	return out
}

// backfill returns primary plus every fallback id it lacks, calibrated.
// primary is not modified; sectors may share one map.
func backfill(primary, fallback map[string]float64) map[string]float64 {
	a, k := fit(primary, fallback)
	out := make(map[string]float64, len(primary)+len(fallback))
	maps.Copy(out, primary)
	for id, v := range fallback {
		if _, ok := out[id]; !ok {
			out[id] = min(100, max(0, a+k*v))
		}
	}
	return out
}

// fit returns intercept a and slope k of primary ≈ a + k·fallback over the
// shared ids, or the identity (0, 1) when the fit is unusable.
func fit(primary, fallback map[string]float64) (a, k float64) {
	var n, sx, sy, sxx, sxy float64
	for id, x := range fallback {
		y, ok := primary[id]
		if !ok {
			continue
		}
		n++
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	den := n*sxx - sx*sx
	if n < minFit || den <= 0 {
		return 0, 1
	}
	k = (n*sxy - sx*sy) / den
	if k <= 0 {
		return 0, 1
	}
	return (sy - k*sx) / n, k
}
