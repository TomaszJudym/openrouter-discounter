package main

import (
	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/pipeline"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// route is one preset slug's Darkbloom side: up to preset.TopN Darkbloom
// model ids in rank order, and whether they go before the OpenRouter
// preset.
type route struct {
	direct  []string
	dbFirst bool
}

// routes maps preset slugs to their Darkbloom side. A slug without a route
// goes to OpenRouter only.
type routes map[string]route

// buildRoutes takes each preset's Darkbloom picks by the rule the
// OpenRouter presets use (preset.Picks). Darkbloom goes first when its #1
// costs no more than the OpenRouter preset's #1 (blended per Mtok under the
// preset's own workload; a tie goes to the prepaid Darkbloom credits), or
// when the OpenRouter preset routes nothing today.
func buildRoutes(res pipeline.Result) routes {
	orFirst := make(map[string]float64, len(res.OpenRouter))
	for _, sr := range res.OpenRouter {
		if picks := preset.Picks(sr.Rows); len(picks) > 0 {
			orFirst[preset.Slugs[sr.Sector]] = picks[0].Price
		}
	}
	out := make(routes, len(res.Direct))
	for _, sr := range res.Direct {
		slug, ok := preset.Slugs[sr.Sector]
		if !ok {
			continue
		}
		picks := preset.Picks(sr.Rows)
		ids := directIDs(picks)
		if len(ids) == 0 {
			continue
		}
		orPrice, hasOR := orFirst[slug]
		out[slug] = route{direct: ids, dbFirst: !hasOR || picks[0].Price <= orPrice}
	}
	return out
}

func directIDs(rows []rank.Row) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if id, ok := discounts.DirectID(r.Endpoint); ok {
			ids = append(ids, id)
		}
	}
	return ids
}
