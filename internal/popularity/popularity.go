// Package popularity scores models by OpenRouter token volume over the
// recent window shown on openrouter.ai/rankings. It implements rank.Provider
// for the long-context sector.
package popularity

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// RankingsURL is the (undocumented, unauthenticated) data feed behind
// openrouter.ai/rankings: one row per model variant per day.
const RankingsURL = "https://openrouter.ai/api/frontend/v1/rankings/models"

// SourceName labels the sector in the price-only notice.
const SourceName = "OpenRouter rankings"

type row struct {
	Permaslug  string  `json:"model_permaslug"`
	Variant    string  `json:"variant"`
	Metric     float64 `json:"rankingMetricValue"`
	Prompt     float64 `json:"total_prompt_tokens"`
	Completion float64 `json:"total_completion_tokens"`
}

type response struct {
	Data []row `json:"data"`
}

// Provider maps ranking rows to catalog ids via canonical_slug + variant.
type Provider struct {
	get    fetch.Getter
	url    string
	sector rank.Sector
	ids    map[key]string
}

type key struct{ slug, variant string }

// New builds a provider scoring sector from the catalog models.
func New(g fetch.Getter, models []discounts.Model, sector rank.Sector) *Provider {
	ids := make(map[key]string, len(models))
	for _, m := range models {
		if m.CanonicalSlug == "" {
			continue
		}
		variant := "standard"
		if _, v, ok := strings.Cut(m.ID, ":"); ok {
			variant = v
		}
		ids[key{m.CanonicalSlug, variant}] = m.ID
	}
	return &Provider{get: g, url: RankingsURL, sector: sector, ids: ids}
}

// Scores implements rank.Provider. Each id's tokens are summed over the
// window and log-scaled to 0-100 between the least and most used matched
// models, so a 10× usage gap is a fixed step rather than a cliff.
func (p *Provider) Scores(ctx context.Context) map[rank.Sector]rank.SectorScores {
	tokens, err := p.tokens(ctx)
	if err != nil {
		return map[rank.Sector]rank.SectorScores{p.sector: {Source: SourceName, Err: err.Error()}}
	}
	return map[rank.Sector]rank.SectorScores{p.sector: {Source: SourceName, ByModel: logScale(tokens)}}
}

func (p *Provider) tokens(ctx context.Context) (map[string]float64, error) {
	body, err := p.get.Get(ctx, p.url, nil, "")
	if err != nil {
		return nil, err
	}
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errors.New(fetch.Sanitize(fmt.Sprintf("unparseable response: %v", err), ""))
	}
	out := make(map[string]float64)
	for _, rw := range r.Data {
		id, ok := p.ids[key{rw.Permaslug, rw.Variant}]
		if !ok {
			continue
		}
		v := rw.Metric
		if v <= 0 {
			v = rw.Prompt + rw.Completion
		}
		if v > 0 {
			out[id] += v
		}
	}
	return out, nil
}

func logScale(tokens map[string]float64) map[string]float64 {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range tokens {
		l := math.Log10(v)
		lo, hi = min(lo, l), max(hi, l)
	}
	out := make(map[string]float64, len(tokens))
	for id, v := range tokens {
		if hi <= lo {
			out[id] = 100
			continue
		}
		out[id] = 100 * (math.Log10(v) - lo) / (hi - lo)
	}
	return out
}
