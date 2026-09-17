// Package discounts fetches the OpenRouter model catalog and detects active
// price discounts from pricing.overrides windows.
//
// The live OpenRouter schema (verified 2026-09 against /api/v1/models):
//
//	"pricing": {
//	  "prompt": "0.00000066",           // base USD per token
//	  "completion": "0.00000198",
//	  "overrides": [
//	    {
//	      "utc_days": ["saturday","sunday"],  // time window (UTC weekdays)
//	      "utc_start": 100,                   // optional prompt-size bucket
//	      "utc_end": 400,                     // bounds in Ktok, end==0 = open
//	      "prompt": "0.00000066",             // override price while active
//	      "completion": "0.00000198"
//	    },
//	    { "min_prompt_tokens": 272000, "prompt": "..." }  // volume tier, no time window
//	  ]
//	}
//
// There is no discount-percentage field, so the baseline is the top-level
// prompt price: a model is discounted iff an override whose utc_days window
// contains the current UTC weekday prices prompt below the base price. The
// reported price is the cheapest prompt price among today's windows for that
// model. Volume-only tiers (min_prompt_tokens, no utc_days) have no time
// window and are ignored.
package discounts

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// URL is the OpenRouter model catalog endpoint.
const URL = "https://openrouter.ai/api/v1/models"

const freeSuffix = ":free"

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

// Discount is one active price drop. Price and Was are USD per million
// prompt tokens; Pct is negative for a discount.
type Discount struct {
	ModelID string
	Price   float64
	Was     float64
	Pct     float64
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
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
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

// Detect returns models discounted under a currently active override window,
// plus the ids of :free models. Base prices of zero (including null pricing)
// are excluded. now supplies the reference time for the UTC weekday window.
func Detect(models []Model, now time.Time) (discounts []Discount, free []string) {
	day := strings.ToLower(now.UTC().Weekday().String())
	for _, m := range models {
		if strings.HasSuffix(m.ID, freeSuffix) {
			free = append(free, m.ID)
			continue
		}
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
	return discounts, free
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
