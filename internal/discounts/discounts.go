// Package discounts fetches the OpenRouter catalog and per-provider endpoint
// pricing, and detects active price discounts.
//
// Discount definition (verified 2026-11 against the live API): a model is
// discounted iff one of its provider endpoints on
// GET /api/v1/models/{author}/{slug}/endpoints is cheaper now than at its
// list price — either pricing.discount > 0 (the listed prices are already
// discounted; list = price/(1-discount)) or an active pricing.overrides time
// window (utc_start/utc_end HHMM, optional utc_days) priced below the base.
//
// Prices compare as blended USD per Mtok under a request workload (see
// Workload), averaged over the 24 hours the run's picks stay pinned (see
// Horizon): cache-read, cache-write and reasoning rates count, not only the
// prompt rate. Per model the pick is the single cheapest endpoint that
// passes the flex and quantization filters (see usable) and meets the
// preset's needs (see Discount.For); the model is a discount only when that
// endpoint is discounted. Presets pin exactly that endpoint. Models without
// endpoint data fall back to the catalog's own windows. :batch variants are
// skipped: they serve asynchronous jobs, not chat.
package discounts

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
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
	batchSuffix  = ":batch"
	maxBodyBytes = 16 << 20
	maxAttempts  = 3
	workers      = 8
)

// Benchmarks holds the catalog's third-party benchmark scores.
type Benchmarks struct {
	ArtificialAnalysis struct {
		IntelligenceIndex float64 `json:"intelligence_index"`
	} `json:"artificial_analysis"`
}

// Architecture describes the model's input/output capabilities.
type Architecture struct {
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
}

// TextOnly reports whether the model is a pure text model: no image or video
// input (vision) and no output besides text. Unlisted modalities count as text.
func (m Model) TextOnly() bool {
	for _, in := range m.Architecture.InputModalities {
		if in == "image" || in == "video" {
			return false
		}
	}
	for _, out := range m.Architecture.OutputModalities {
		if out != "text" {
			return false
		}
	}
	return true
}

// Vision reports whether the model accepts image input.
func (m Model) Vision() bool {
	return slices.Contains(m.Architecture.InputModalities, "image")
}

// Reasoning is the catalog's reasoning-capability marker.
type Reasoning struct {
	Mandatory bool `json:"mandatory"`
}

// ChatText reports whether the model can produce text output.
// An empty or nil OutputModalities is treated as unknown but text-capable.
func (m Model) ChatText() bool {
	if m.Architecture.OutputModalities == nil {
		return true
	}
	if slices.Contains(m.Architecture.OutputModalities, "text") {
		return true
	}
	return len(m.Architecture.OutputModalities) == 0
}

// Thinking reports whether the model advertises reasoning support in the
// catalog: a non-null reasoning object or the reasoning parameter.
func (m Model) Thinking() bool {
	if m.Reasoning != nil {
		return true
	}
	return slices.Contains(m.SupportedParameters, "reasoning")
}

// ToolCalling reports whether the model accepts function/tool definitions:
// the catalog lists "tools" in supported_parameters.
func (m Model) ToolCalling() bool {
	return slices.Contains(m.SupportedParameters, "tools")
}

// WebSearch reports whether the model can search the web: natively
// (web_search_options) or through the openrouter:web_search server tool,
// which needs tool calling. Searches are billed even on :free models.
func (m Model) WebSearch() bool {
	return m.ToolCalling() || slices.Contains(m.SupportedParameters, "web_search_options")
}

// Model is one catalog entry.
type Model struct {
	ID                  string       `json:"id"`
	CanonicalSlug       string       `json:"canonical_slug"`
	Pricing             *Pricing     `json:"pricing"`
	Benchmarks          Benchmarks   `json:"benchmarks"`
	Reasoning           *Reasoning   `json:"reasoning"`
	SupportedParameters []string     `json:"supported_parameters"`
	Architecture        Architecture `json:"architecture"`
	ContextLength       int          `json:"context_length"`
}

type response struct {
	Data []Model `json:"data"`
}

// Endpoint is one provider's offer for a model. Tag is the endpoint slug
// ("gmicloud/fp8") that provider.only accepts; unique per model.
// ServiceTier is "flex", "priority", … for tier endpoints, empty otherwise.
type Endpoint struct {
	Provider        string   `json:"provider_name"`
	Tag             string   `json:"tag"`
	ServiceTier     string   `json:"service_tier"`
	Pricing         Pricing  `json:"pricing"`
	Quantization    string   `json:"quantization"`
	ContextLength   int      `json:"context_length"`
	Parameters      []string `json:"supported_parameters"`
	ImplicitCaching bool     `json:"supports_implicit_caching"`
	ToolChoice      struct {
		Auto *bool `json:"auto"`
	} `json:"supports_tool_choice"`
}

// tools reports whether the endpoint accepts tool definitions: tools is a
// supported parameter and tool_choice auto (the default) is not refused.
func (e Endpoint) tools() bool {
	return slices.Contains(e.Parameters, "tools") && (e.ToolChoice.Auto == nil || *e.ToolChoice.Auto)
}

type endpointsResponse struct {
	Data struct {
		ID        string     `json:"id"`
		Endpoints []Endpoint `json:"endpoints"`
	} `json:"data"`
}

// Discount is a model whose cheapest fitting endpoint is discounted. The
// pick fields describe one offer under one workload — from FetchDiscounts,
// the cheapest offer for Require{} under DefaultWorkload; For re-picks for
// a preset. Price and Was are blended USD per Mtok and Pct their change
// (negative for a discount); Prompt and Completion are the average rates
// and PeakPrompt/PeakCompletion the highest within the Horizon, USD per
// Mtok, at the workload's context tier.
type Discount struct {
	ModelID        string
	Serves         []string // every endpoint tag of the model, for pin conflicts
	Offers         []Offer  // every usable endpoint, discounted or not
	Provider       string   // picked endpoint's provider; empty for catalog rows
	Endpoint       string   // picked endpoint tag to pin; empty for catalog rows
	Window         string   // price windows within the Horizon; empty for flat discounts
	Price          float64
	Was            float64
	Pct            float64
	Prompt         float64
	Completion     float64
	PeakPrompt     float64
	PeakCompletion float64
}

// For re-picks the cheapest offer meeting r under w (a discounted offer
// wins a tie); ok is false when no offer meets r or the cheapest is not
// discounted. A row without offers (built by hand) is returned as is.
func (d Discount) For(r Require, w Workload) (Discount, bool) {
	if len(d.Offers) == 0 {
		return d, true
	}
	best, bestPrice := -1, math.Inf(1)
	for i, o := range d.Offers {
		if !o.meets(r, w) {
			continue
		}
		p := o.Avg.Blended(w)
		if p < bestPrice || (p == bestPrice && o.discounted(w) && !d.Offers[best].discounted(w)) {
			best, bestPrice = i, p
		}
	}
	if best < 0 || !d.Offers[best].discounted(w) {
		return Discount{}, false
	}
	o := d.Offers[best]
	now, was := o.Avg.Blended(w), o.List.Blended(w)
	avg, peak := o.Avg.For(w), o.Peak.For(w)
	d.Provider, d.Endpoint, d.Window = o.Provider, o.Endpoint, o.Window
	d.Price, d.Was, d.Pct = now, was, (now/was-1)*100
	d.Prompt, d.Completion = avg.Prompt, avg.Completion
	d.PeakPrompt, d.PeakCompletion = peak.Prompt, peak.Completion
	return d, true
}

// Price ceilings for model selection, USD per million tokens of the
// discounted price. Rows above either one are never picked.
const (
	MaxPrompt     = 0.5
	MaxCompletion = 1.0
)

// Affordable reports whether the discounted price is within both ceilings.
func (d Discount) Affordable() bool {
	return d.Prompt <= MaxPrompt && d.Completion <= MaxCompletion
}

// Affordable returns the rows within the price ceilings, order kept.
func Affordable(ds []Discount) []Discount {
	out := make([]Discount, 0, len(ds))
	for _, d := range ds {
		if d.Affordable() {
			out = append(out, d)
		}
	}
	return out
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
		if strings.HasSuffix(m.ID, freeSuffix) || strings.HasSuffix(m.ID, batchSuffix) {
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
	discounts := endpointDiscounts(byID, now)
	// catalog windows only for models without endpoint data: with it, the
	// endpoint scan already saw the windows and the cheaper alternatives
	for _, d := range Detect(models, now) {
		if _, ok := byID[d.ModelID]; !ok {
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
			return r.Data.Endpoints, nil
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

// endpointDiscounts prices every usable endpoint of each model over the
// Horizon from at and reports the model when its cheapest offer (Require{},
// DefaultWorkload) is discounted. A discount undercut by an undiscounted
// endpoint is no saving and yields no row.
func endpointDiscounts(byID map[string][]Endpoint, at time.Time) []Discount {
	out := make([]Discount, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		eps := byID[id]
		d := Discount{ModelID: id, Serves: make([]string, 0, len(eps))}
		for _, e := range eps {
			if e.Tag != "" {
				d.Serves = append(d.Serves, e.Tag)
			}
		}
		for _, e := range usable(eps) {
			if o, ok := newOffer(e.Provider, e.Tag, e.Pricing, e.ImplicitCaching, at); ok {
				o.Tools, o.Context = e.tools(), e.ContextLength
				d.Offers = append(d.Offers, o)
			}
		}
		if len(d.Offers) == 0 {
			continue
		}
		if pick, ok := d.For(Require{}, DefaultWorkload); ok {
			out = append(out, pick)
		}
	}
	return out
}

// Detect returns the models discounted by time windows in their catalog
// pricing.overrides over the Horizon from at (priced below the base under
// DefaultWorkload). Catalog offers carry the model's own tool and context
// support and no implicit caching. Zero, null and dynamic (negative)
// prices and :batch variants are excluded.
func Detect(models []Model, at time.Time) []Discount {
	var out []Discount
	for _, m := range models {
		if m.Pricing == nil || strings.HasSuffix(m.ID, batchSuffix) {
			continue
		}
		o, ok := newOffer("", "", *m.Pricing, false, at)
		if !ok {
			continue
		}
		o.Tools, o.Context = m.ToolCalling(), m.ContextLength
		d := Discount{ModelID: m.ID, Offers: []Offer{o}}
		if pick, ok := d.For(Require{}, DefaultWorkload); ok {
			out = append(out, pick)
		}
	}
	return out
}

// perMtok converts a decimal USD-per-token string to USD per million
// tokens; unparseable or empty yields 0.
func perMtok(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v * 1e6
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
