// Package preset keeps the admech-* OpenRouter presets pointing at the top 3
// ranked models of each category. Each paid model is pinned to its single
// cheapest endpoint: provider.only lists those endpoint tags with
// allow_fallbacks off, so a failing provider falls through to the next
// model, never to another provider; provider.max_price caps the request at
// the highest rate a pinned endpoint bills in the day, so a discount that
// ends early falls through too instead of billing list price. Presets are versioned server-side: a
// POST to the chat-completions skin creates a new designated version, so
// updates are skipped when nothing changed.
package preset

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	_ "embed"

	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

//go:embed systemprompt.txt
var systemPromptFile string

//go:embed halflight.txt
var halflightPromptFile string

//go:embed inlandempire.txt
var inlandempirePromptFile string

//go:embed volition.txt
var volitionPromptFile string

//go:embed shivers.txt
var shiversPromptFile string

// systemPrompt is the system instruction written into every preset version.
var systemPrompt = strings.TrimSpace(systemPromptFile)

// halflightPrompt is the HALF-LIGHT persona used only for strongest-discount-halflight.
var halflightPrompt = strings.TrimSpace(halflightPromptFile)

// volitionPrompt is the VOLITION persona used only for strongest-discount-volition.
var volitionPrompt = strings.TrimSpace(volitionPromptFile)

// inlandempirePrompt is the INLAND EMPIRE persona used only for strongest-discount-inlandempire.
var inlandempirePrompt = strings.TrimSpace(inlandempirePromptFile)

// shiversPrompt is the SHIVERS persona used only for strongest-discount-shivers.
var shiversPrompt = strings.TrimSpace(shiversPromptFile)

// verbosity is requested on every update even when the routed model does not
// support it: the API stores it in the preset config, and frontier models
// that understand it pick it up.
const verbosity = "low"
const reasoningEffort = "max"

// presetSpec holds per-preset configuration.
type presetSpec struct {
	systemPrompt string
	// hermes marks a Hermes Agent preset: no verbosity, and no system
	// message at all when systemPrompt is empty (Hermes sends its own).
	hermes bool
	// effort overrides reasoningEffort when non-empty.
	effort           string
	reasoningEnabled bool
	sampling         bool // whether to include sampling params
	temperature      float64
	topP             float64
	frequencyPenalty float64
	presencePenalty  float64
	minP             float64
	maxTokens        int
}

// persona is the spec shared by the Disco Elysium persona presets.
// Frequency penalty stays 0: it grows with every repeat of a token, so over a
// long reply (reasoning tokens included) every common token ends up penalized
// and the output collapses into rare-word salad. Temperature stays at 1 and
// presence penalty low for the same reason; top_p and min_p cut the
// low-probability tail the salad is drawn from. Effort high instead of max
// keeps reasoning short; max_tokens bounds a runaway reply.
func persona(prompt string) presetSpec {
	return presetSpec{
		systemPrompt:     prompt,
		effort:           "high",
		reasoningEnabled: true,
		sampling:         true,
		minP:             0.05,
		maxTokens:        16384,
		temperature:      1.0,
		topP:             0.95,
		frequencyPenalty: 0,
		presencePenalty:  0.1,
	}
}

// specs maps sectors to their preset specifications.
var specs = map[rank.Sector]presetSpec{
	rank.Math:                          {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.LCR:                           {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.Finance:                       {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.Code:                          {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.CodeFree:                      {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.ExpensiveLong:                 {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.StrongestDiscountHalflight:    persona(halflightPrompt),
	rank.StrongestDiscountVolition:     persona(volitionPrompt),
	rank.StrongestDiscountInlandEmpire: persona(inlandempirePrompt),
	rank.StrongestDiscountShivers:      persona(shiversPrompt),
	rank.FreeHalflight:                 persona(halflightPrompt),
	// Task presets: none talks to the user, so none carries a prompt.
	rank.HermesDelegate: {hermes: true, effort: "medium"},
	rank.HermesReview:   {hermes: true, effort: "medium"},
	rank.HermesCompress: {hermes: true, effort: "low"},
	rank.HermesVision:   {hermes: true, effort: "none"},
	rank.HermesLight:    {hermes: true, effort: "none"},
}

// effortFor is the spec's reasoning effort, defaulting to reasoningEffort.
func effortFor(spec presetSpec) string {
	if spec.effort != "" {
		return spec.effort
	}
	return reasoningEffort
}

// Slugs maps ranked sectors to the user's preset slugs.
var Slugs = map[rank.Sector]string{
	rank.Math:                          "admech-math",
	rank.LCR:                           "admech-long",
	rank.Finance:                       "admech-finance",
	rank.Code:                          "admech-code",
	rank.CodeFree:                      "admech-code-free",
	rank.ExpensiveLong:                 "expensive-long",
	rank.StrongestDiscountHalflight:    "strongest-discount-half-light",
	rank.StrongestDiscountVolition:     "strongest-discount-volition",
	rank.StrongestDiscountInlandEmpire: "strongest-discount-inlandempire",
	rank.StrongestDiscountShivers:      "strongest-discount-shivers",
	rank.FreeHalflight:                 "halflight-free",
	rank.HermesDelegate:                "hermes-delegate",
	rank.HermesVision:                  "hermes-vision",
	rank.HermesCompress:                "hermes-compress",
	rank.HermesReview:                  "hermes-review",
	rank.HermesLight:                   "hermes-light",
}

// TopN is how many models each preset routes.
const TopN = 3

type version struct {
	Version      int    `json:"version"`
	SystemPrompt string `json:"system_prompt"`
	Config       struct {
		Models   []string `json:"models"`
		Provider struct {
			Only           []string `json:"only,omitzero"`
			AllowFallbacks *bool    `json:"allow_fallbacks,omitzero"`
			MaxPrice       struct {
				Prompt     price `json:"prompt,omitzero"`
				Completion price `json:"completion,omitzero"`
			} `json:"max_price,omitzero"`
		} `json:"provider,omitzero"`
		Reasoning struct {
			Effort  string `json:"effort,omitzero"`
			Enabled *bool  `json:"enabled,omitzero"`
		} `json:"reasoning,omitzero"`
		Verbosity        string   `json:"verbosity"`
		Temperature      *float64 `json:"temperature,omitempty"`
		TopP             *float64 `json:"top_p,omitempty"`
		FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
		PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
		MinP             *float64 `json:"min_p,omitempty"`
		MaxTokens        *int     `json:"max_tokens,omitempty"`
	} `json:"config"`
}

type presetResponse struct {
	Data struct {
		Slug              string  `json:"slug"`
		DesignatedVersion version `json:"designated_version"`
	} `json:"data"`
}

// Client updates presets with the OpenRouter API key. Callers skip the
// client entirely when no key is configured.
type Client struct {
	hc   *http.Client
	key  string
	base string // overridable for tests
}

// New builds a client authenticating with the OpenRouter API key.
func New(hc *http.Client, key string) *Client {
	return &Client{hc: hc, key: key, base: "https://openrouter.ai/api/v1"}
}

// Update ensures the sector's preset lists the top-3 scored models with the
// shared system prompt and verbosity. It reports whether a new version was
// created. Presets with no scored models are left untouched.
func (c *Client) Update(ctx context.Context, sr rank.SectorResult) (bool, error) {
	slug, ok := Slugs[sr.Sector]
	if !ok {
		return false, fmt.Errorf("no preset slug for sector %q", sr.Sector)
	}
	spec, ok := specs[sr.Sector]
	if !ok {
		return false, fmt.Errorf("no spec for sector %q", sr.Sector)
	}
	models, p := topScored(sr.Rows, TopN)
	if len(models) == 0 {
		return false, nil
	}
	cur, err := c.current(ctx, slug)
	if err != nil {
		return false, err
	}
	if cur != nil && sameVersion(*cur, models, p, spec) {
		return false, nil
	}
	if err := c.post(ctx, slug, models, p, spec); err != nil {
		return false, err
	}
	return true, nil
}

// pin is a preset's provider routing: the endpoint tags allowed, and the
// per-Mtok price caps. A zero pin routes unpinned.
type pin struct {
	only                     []string
	maxPrompt, maxCompletion float64
}

// capMargin is the headroom over the highest pinned rate, so float noise in
// the listed prices cannot reject a request at the expected price.
const capMargin = 1.01

// ceilCap rounds a cap up to 4 decimals of USD per Mtok; the product is
// first rounded to 8 decimals so float noise (0.8×1.01 = 0.80800…01) does
// not round up a whole step.
func ceilCap(v float64) float64 { return math.Ceil(math.Round(v*capMargin*1e8)/1e4) / 1e4 }

// topScored takes the first n scored rows in ranking order. When the rows
// carry endpoints (paid discounts), only rows that pin without conflict are
// taken (see rank.Pinnable); the pin lists their endpoint tags and caps the
// price at their highest peak rates. Free rows have no endpoint and route
// unpinned.
func topScored(rows []rank.Row, n int) (models []string, p pin) {
	scored := make([]rank.Row, 0, len(rows))
	for _, r := range rows {
		if r.Scored {
			scored = append(scored, r)
		}
	}
	pinned := slices.ContainsFunc(scored, func(r rank.Row) bool { return r.Endpoint != "" })
	if pinned {
		scored = rank.Pinnable(scored, n)
	}
	scored = scored[:min(n, len(scored))]
	for _, r := range scored {
		models = append(models, r.ModelID)
		if !pinned {
			continue
		}
		if !slices.Contains(p.only, r.Endpoint) {
			p.only = append(p.only, r.Endpoint)
		}
		p.maxPrompt = max(p.maxPrompt, ceilCap(r.PeakPrompt))
		p.maxCompletion = max(p.maxCompletion, ceilCap(r.PeakCompletion))
	}
	return models, p
}

// sameVersion reports whether the designated version already matches.
func sameVersion(v version, models []string, p pin, spec presetSpec) bool {
	pv := v.Config.Provider
	if !slices.Equal(v.Config.Models, models) || !slices.Equal(pv.Only, p.only) {
		return false
	}
	// an absent allow_fallbacks counts as false: the API may drop zero
	// values (see floatIs), and a version without the pin fails on Only
	if fb := pv.AllowFallbacks; len(p.only) > 0 && fb != nil && *fb {
		return false
	}
	if float64(pv.MaxPrice.Prompt) != p.maxPrompt || float64(pv.MaxPrice.Completion) != p.maxCompletion {
		return false
	}
	wantPrompt, wantVerbosity := spec.systemPrompt, verbosity
	if spec.hermes {
		wantVerbosity = ""
	}
	if strings.TrimSpace(v.SystemPrompt) != wantPrompt {
		return false
	}
	if !reasoningMatches(v, spec) {
		return false
	}
	if v.Config.Verbosity != wantVerbosity {
		return false
	}
	if spec.sampling && !samplingMatches(v, spec) {
		return false
	}
	return true
}

func reasoningMatches(v version, spec presetSpec) bool {
	if spec.reasoningEnabled {
		if v.Config.Reasoning.Enabled == nil || !*v.Config.Reasoning.Enabled {
			return false
		}
	}
	return v.Config.Reasoning.Effort == effortFor(spec)
}

func samplingMatches(v version, spec presetSpec) bool {
	return floatIs(v.Config.Temperature, spec.temperature) &&
		floatIs(v.Config.TopP, spec.topP) &&
		floatIs(v.Config.FrequencyPenalty, spec.frequencyPenalty) &&
		floatIs(v.Config.PresencePenalty, spec.presencePenalty) &&
		floatIs(v.Config.MinP, spec.minP) &&
		intIs(v.Config.MaxTokens, spec.maxTokens)
}

// floatIs reports whether a stored value equals want; an absent value counts
// as 0, since the API may drop zero-valued sampling fields.
func floatIs(p *float64, want float64) bool {
	if p == nil {
		return want == 0
	}
	return *p == want
}

// intIs is floatIs for integer fields.
func intIs(p *int, want int) bool {
	if p == nil {
		return want == 0
	}
	return *p == want
}

// price is a stored max_price value: a JSON number or a decimal string
// (OpenRouter prices are strings elsewhere), so either form round-trips.
type price float64

// UnmarshalJSON accepts 0.303 and "0.303".
func (p *price) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseFloat(strings.Trim(string(b), `"`), 64)
	if err != nil {
		return fmt.Errorf("max_price %s: %w", b, err)
	}
	*p = price(v)
	return nil
}

func (c *Client) current(ctx context.Context, slug string) (*version, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/presets/"+slug, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET preset %s: %w", slug, err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only: a close error is not actionable
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil // preset missing: the POST below creates it
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GET preset %s: status %d: %.300s", slug, resp.StatusCode, body)
	}
	var r presetResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse preset %s: %w", slug, err)
	}
	return &r.Data.DesignatedVersion, nil
}

func makeBody(spec presetSpec, models []string, p pin) map[string]any {
	reasoning := map[string]any{"effort": effortFor(spec)}
	if spec.reasoningEnabled {
		reasoning["enabled"] = true
	}
	// The endpoint validates as a chat completion, so messages is always
	// sent. Only a system message is stored as the preset prompt; a
	// prompt-less preset gets a transient user message that is not stored.
	msg := map[string]string{"role": "system", "content": spec.systemPrompt}
	if spec.hermes && spec.systemPrompt == "" {
		msg = map[string]string{"role": "user", "content": "."}
	}
	body := map[string]any{
		"messages":  []map[string]string{msg},
		"models":    models,
		"reasoning": reasoning,
	}
	if len(p.only) > 0 {
		prov := map[string]any{"only": p.only, "allow_fallbacks": false}
		if caps := maxPrice(p); len(caps) > 0 {
			prov["max_price"] = caps
		}
		body["provider"] = prov
	}
	if !spec.hermes {
		body["verbosity"] = verbosity
	}
	if spec.sampling {
		body["temperature"] = spec.temperature
		body["top_p"] = spec.topP
		body["frequency_penalty"] = spec.frequencyPenalty
		body["presence_penalty"] = spec.presencePenalty
		body["min_p"] = spec.minP
		body["max_tokens"] = spec.maxTokens
	}
	return body
}

// maxPrice is the provider.max_price object; unknown (zero) caps are left
// out rather than blocking every request.
func maxPrice(p pin) map[string]float64 {
	caps := make(map[string]float64, 2)
	if p.maxPrompt > 0 {
		caps["prompt"] = p.maxPrompt
	}
	if p.maxCompletion > 0 {
		caps["completion"] = p.maxCompletion
	}
	return caps
}

func (c *Client) post(ctx context.Context, slug string, models []string, p pin, spec presetSpec) error {
	body := makeBody(spec, models, p)
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/presets/"+slug+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("POST preset %s: %w", slug, err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only: a close error is not actionable
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST preset %s: status %d: %.300s", slug, resp.StatusCode, respBody)
	}
	return nil
}
