// Package preset keeps the admech-* OpenRouter presets pointing at the top 3
// ranked models of each category. Presets are versioned server-side: a POST
// to the chat-completions skin creates a new designated version, so updates
// are skipped when nothing changed.
package preset

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"slices"
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
		Models    []string `json:"models"`
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
	models := topScored(sr.Rows, TopN)
	if len(models) == 0 {
		return false, nil
	}
	cur, err := c.current(ctx, slug)
	if err != nil {
		return false, err
	}
	if cur != nil && sameVersion(*cur, models, spec) {
		return false, nil
	}
	if err := c.post(ctx, slug, models, spec); err != nil {
		return false, err
	}
	return true, nil
}

// topScored takes the first n scored rows in ranking order.
func topScored(rows []rank.Row, n int) []string {
	out := make([]string, 0, n)
	for _, r := range rows {
		if len(out) == n {
			break
		}
		if r.Scored {
			out = append(out, r.ModelID)
		}
	}
	return out
}

// sameVersion reports whether the designated version already matches.
func sameVersion(v version, models []string, spec presetSpec) bool {
	if !slices.Equal(v.Config.Models, models) {
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

func makeBody(spec presetSpec, models []string) map[string]any {
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

func (c *Client) post(ctx context.Context, slug string, models []string, spec presetSpec) error {
	body := makeBody(spec, models)
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
