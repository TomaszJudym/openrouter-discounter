// Package discounts fetches the OpenRouter catalog and per-provider endpoint
// pricing, and detects active price discounts.
//
// Discount definition (verified 2026-09 against the live API and the
// /collections/discounted-models page, which is built from the same data):
// a model is discounted iff at least one provider endpoint advertises
// pricing.discount > 0 on GET /api/v1/models/{author}/{slug}/endpoints.
// On such an endpoint pricing.prompt is already the discounted price; the
// list price is prompt/(1-discount) — matching StreamLake's 0.0000006426 =
// 0.0000014 × (1-0.541) against the standard $1.40/Mtok list. The reported
// price is the cheapest effective prompt price among discounted endpoints.
//
// As a secondary signal, time-limited catalog windows on the model's own
// pricing.overrides (utc_days weekday windows priced below the base) are
// also reported; endpoint rows take precedence.
package discounts

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// URL is the OpenRouter model catalog endpoint; endpointsURLFormat is the
// per-provider endpoint pricing path (var so tests can point it at a stub).
const (
	URL = "https://openrouter.ai/api/v1/models"
)

var endpointsURLFormat = "https://openrouter.ai/api/v1/models/%s/%s/endpoints"

const (
	freeSuffix   = ":free"
	maxBodyBytes = 16 << 20
	maxAttempts  = 3
	workers      = 8
)

// Pricing is a model's base pricing. Prompt and Completion are decimal USD
// per token.
type Pricing struct {
	Prompt     string     `json:"prompt"`
	Completion string     `json:"completion"`
	Overrides  []Override `json:"overrides"`
}

// Override is one tiered-pricing entry.
type Override struct {
	Days         []string `json:"utc_days"`          // UTC weekdays the tier applies
	StartTok     float64  `json:"utc_start"`         // bucket start in Ktok; 0 = from zero
	EndTok       float64  `json:"utc_end"`           // bucket end in Ktok; 0 = open-ended
	MinPromptTok float64  `json:"min_prompt_tokens"` // volume tier bound; no time window
	Prompt       string   `json:"prompt"`
	Completion   string   `json:"completion"`
}

// Model is one catalog entry.
type Model struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Context int      `json:"context_length"`
	Pricing *Pricing `json:"pricing"`
}

type response struct {
	Data []Model `json:"data"`
}

// Endpoint is one provider's pricing for a model.
type Endpoint struct {
	Provider   string
	Prompt     string
	Completion string
	Discount   float64 // fraction; 0 = none
}

type endpointsResponse struct {
	Data struct {
		ID        string `json:"id"`
		Endpoints []struct {
			ProviderName string `json:"provider_name"`
			Pricing      struct {
				Prompt     string  `json:"prompt"`
				Completion string  `json:"completion"`
				Discount   float64 `json:"discount"`
			} `json:"pricing"`
		} `json:"endpoints"`
	} `json:"data"`
}

// Discount is one active price drop. Price and Was are USD per million
// prompt tokens; Pct is negative for a discount.
type Discount struct {
	ModelID  string
	Provider string // discounted provider, empty for catalog-window rows
	Price    float64
	Was      float64
	Pct      float64
}

// Fetch retrieves the catalog.
func Fetch(ctx context.Context, hc *http.Client) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", URL, err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only: a close error is not actionable
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d: %.500s", URL, resp.StatusCode, body)
	}
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	return r.Data, nil
}

// FetchDiscounts scans every non-free model's provider endpoints for
// discounts and merges in catalog-window discounts. Models whose endpoint
// fetch fails are skipped and the failure is returned joined, without
// aborting the scan.
func FetchDiscounts(ctx context.Context, hc *http.Client, models []Model, now time.Time) ([]Discount, []string, error) {
	targets := make([]string, 0, len(models))
	for _, m := range models {
		if strings.HasSuffix(m.ID, freeSuffix) {
			continue
		}
		targets = append(targets, m.ID)
	}
	endpoints := make([][]Endpoint, len(targets))
	failures := make([]error, 0, 4)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, id := range targets {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			eps, err := fetchEndpoints(ctx, hc, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", id, err))
				return
			}
			endpoints[i] = eps
		})
	}
	wg.Wait()

	byID := make(map[string][]Endpoint, len(targets))
	for i, eps := range endpoints {
		if len(eps) > 0 {
			byID[targets[i]] = eps
		}
	}
	discounts := endpointDiscounts(byID)
	hasRow := make(map[string]struct{}, len(discounts))
	for _, d := range discounts {
		hasRow[d.ModelID] = struct{}{}
	}
	for _, d := range Detect(models, now) {
		if _, ok := hasRow[d.ModelID]; !ok {
			discounts = append(discounts, d)
		}
	}
	return discounts, freeModels(models), errors.Join(failures...)
}

// fetchEndpoints retrieves one model's provider endpoint pricing with
// bounded retries on transient failures. A 404 yields no endpoints and no
// error (catalog entries can lack providers).
func fetchEndpoints(ctx context.Context, hc *http.Client, id string) ([]Endpoint, error) {
	author, slug, _ := strings.Cut(id, "/")
	target := fmt.Sprintf(endpointsURLFormat, url.PathEscape(author), url.PathEscape(slug))
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		resp, err := hc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("GET %s: %w", target, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close() // read path: a close error is not actionable
		switch {
		case readErr != nil:
			lastErr = fmt.Errorf("read response: %w", readErr)
		case resp.StatusCode == http.StatusNotFound:
			return nil, nil
		case resp.StatusCode == http.StatusOK:
			var r endpointsResponse
			if err := json.Unmarshal(body, &r); err != nil {
				return nil, fmt.Errorf("parse endpoints: %w", err)
			}
			eps := make([]Endpoint, 0, len(r.Data.Endpoints))
			for _, e := range r.Data.Endpoints {
				eps = append(eps, Endpoint{
					Provider:   e.ProviderName,
					Prompt:     e.Pricing.Prompt,
					Completion: e.Pricing.Completion,
					Discount:   e.Pricing.Discount,
				})
			}
			return eps, nil
		case isTransient(resp.StatusCode):
			lastErr = fmt.Errorf("GET %s: status %d: %.200s", target, resp.StatusCode, body)
		default:
			return nil, fmt.Errorf("GET %s: status %d: %.200s", target, resp.StatusCode, body)
		}
	}
	return nil, lastErr
}

func isTransient(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// endpointDiscounts picks, per model, the cheapest discounted endpoint: on a
// discounted endpoint the listed prompt price is already the discounted
// price and the list price is prompt/(1-discount).
func endpointDiscounts(byID map[string][]Endpoint) []Discount {
	out := make([]Discount, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		best := -1
		var bestEff float64
		for i, e := range byID[id] {
			if e.Discount <= 0 || e.Discount >= 1 {
				continue
			}
			p, err := strconv.ParseFloat(e.Prompt, 64)
			if err != nil || p <= 0 {
				continue
			}
			if best < 0 || p < bestEff {
				best, bestEff = i, p
			}
		}
		if best < 0 {
			continue
		}
		e := byID[id][best]
		was := bestEff / (1 - e.Discount)
		out = append(out, Discount{
			ModelID:  id,
			Provider: e.Provider,
			Price:    bestEff * 1e6,
			Was:      was * 1e6,
			Pct:      -e.Discount * 100,
		})
	}
	return out
}

// Detect returns models discounted under a currently active catalog-window
// override (an override whose utc_days contains the current UTC weekday and
// whose prompt price undercuts the base), plus the :free model ids. Base
// prices of zero (including null pricing) are excluded.
func Detect(models []Model, now time.Time) []Discount {
	day := strings.ToLower(now.UTC().Weekday().String())
	var discounts []Discount
	for _, m := range models {
		if m.Pricing == nil {
			continue
		}
		base, err := strconv.ParseFloat(m.Pricing.Prompt, 64)
		if err != nil || base <= 0 {
			continue
		}
		best := minTodayPrice(m.Pricing.Overrides, day)
		if best >= base || best <= 0 {
			continue
		}
		discounts = append(discounts, Discount{
			ModelID: m.ID,
			Price:   best * 1e6,
			Was:     base * 1e6,
			Pct:     (best/base - 1) * 100,
		})
	}
	return discounts
}

// minTodayPrice is the cheapest prompt price among overrides whose utc_days
// window contains day.
func minTodayPrice(ovs []Override, day string) float64 {
	best := 0.0
	for _, ov := range ovs {
		if len(ov.Days) == 0 || !slices.ContainsFunc(ov.Days, func(d string) bool { return strings.EqualFold(d, day) }) {
			continue
		}
		p, err := strconv.ParseFloat(ov.Prompt, 64)
		if err != nil || p <= 0 {
			continue
		}
		if best == 0 || p < best {
			best = p
		}
	}
	return best
}

func freeModels(models []Model) []string {
	var free []string
	for _, m := range models {
		if strings.HasSuffix(m.ID, freeSuffix) {
			free = append(free, m.ID)
		}
	}
	return free
}
