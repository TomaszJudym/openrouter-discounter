// Package aa fetches the Artificial Analysis Finance & Accounting index from
// the free-tier model list and maps it to OpenRouter model ids. It
// implements rank.Provider for the Finance sector.
package aa

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/match"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// baseURL is the free-tier model list; any valid key works. The Pro list
// (/api/v2/language/models) returns 403 for free keys.
const baseURL = "https://artificialanalysis.ai/api/v2/language/models/free"

// SourceName labels the Finance sector in the price-only notice.
const SourceName = "Artificial Analysis"

// maxPages bounds pagination (200 models per page; free quota 100 req/day).
const maxPages = 10

type evaluations struct {
	FinanceIndex *float64 `json:"artificial_analysis_finance_and_accounting_index"`
}

type model struct {
	Name        string      `json:"name"`
	Slug        string      `json:"slug"`
	Creator     creator     `json:"model_creator"`
	Evaluations evaluations `json:"evaluations"`
}

type creator struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type response struct {
	Data       []model `json:"data"`
	Pagination struct {
		HasMore bool `json:"has_more"`
	} `json:"pagination"`
}

// Provider fetches the AA free model list and maps scores to the OpenRouter
// ids given at construction.
type Provider struct {
	get     fetch.Getter
	key     string
	table   match.Table
	baseURL string
}

// New builds a provider for the OpenRouter ids.
func New(g fetch.Getter, key string, orIDs []string) *Provider {
	return &Provider{get: g, key: key, table: match.New(orIDs), baseURL: baseURL}
}

// Scores implements rank.Provider for Finance. Error text is sanitized: the
// API key value is removed and the message is truncated.
func (p *Provider) Scores(ctx context.Context) map[rank.Sector]rank.SectorScores {
	models, err := p.fetch(ctx)
	if err != nil {
		return map[rank.Sector]rank.SectorScores{rank.Finance: {Source: SourceName, Err: err.Error()}}
	}
	by := make(map[string]float64)
	for _, m := range models {
		v := m.Evaluations.FinanceIndex
		if v == nil || *v <= 0 {
			continue
		}
		ids := p.table.Lookup(m.Slug, m.Name, m.Creator.Slug+"-"+m.Slug, m.Creator.Name+"-"+m.Slug)
		match.SetMax(by, ids, *v)
	}
	return map[rank.Sector]rank.SectorScores{rank.Finance: {Source: SourceName, ByModel: by}}
}

// fetch loads every page of the free model list.
func (p *Provider) fetch(ctx context.Context) ([]model, error) {
	if p.key == "" {
		return nil, errors.New("AA_API_KEY not set")
	}
	hdr := http.Header{"X-Api-Key": {p.key}}
	var all []model
	for page := 1; page <= maxPages; page++ {
		body, err := p.get.Get(ctx, p.baseURL+"?page="+strconv.Itoa(page), hdr, p.key)
		if err != nil {
			return nil, err
		}
		var r response
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, errors.New(fetch.Sanitize(fmt.Sprintf("unparseable response: %v", err), p.key))
		}
		all = append(all, r.Data...)
		if !r.Pagination.HasMore {
			break
		}
	}
	return all, nil
}
