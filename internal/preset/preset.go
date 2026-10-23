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

// systemPrompt is the system instruction written into every preset version.
var systemPrompt = strings.TrimSpace(systemPromptFile)

// halflightPrompt is the HALF-LIGHT persona used only for strongest-discount.
var halflightPrompt = strings.TrimSpace(halflightPromptFile)

// verbosity is requested on every update even when the routed model does not
// support it: the API stores it in the preset config, and frontier models
// that understand it pick it up.
const verbosity = "low"
const reasoningEffort = "max"

// presetSpec holds per-preset configuration.
type presetSpec struct {
	systemPrompt     string
	reasoningEnabled bool
	sampling         bool // whether to include sampling params
	temperature      float64
	topP             float64
	frequencyPenalty float64
	presencePenalty  float64
}

// specs maps sectors to their preset specifications.
var specs = map[rank.Sector]presetSpec{
	rank.Math:          {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.LCR:           {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.Finance:       {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.Code:          {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.CodeFree:      {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.ExpensiveLong: {systemPrompt: systemPrompt, reasoningEnabled: false},
	rank.StrongestDiscount: {
		systemPrompt:     halflightPrompt,
		reasoningEnabled: true,
		sampling:         true,
		temperature:      1.15,
		topP:             0.95,
		frequencyPenalty: 0.4,
		presencePenalty:  0.4,
	},
}

// Slugs maps ranked sectors to the user's preset slugs.
var Slugs = map[rank.Sector]string{
	rank.Math:              "admech-math",
	rank.LCR:               "admech-long",
	rank.Finance:           "admech-finance",
	rank.Code:              "admech-code",
	rank.CodeFree:          "admech-code-free",
	rank.ExpensiveLong:     "expensive-long",
	rank.StrongestDiscount: "strongest-discount",
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
	models := topScored(sr.Rows, TopN)
	if len(models) == 0 {
		return false, nil
	}
	cur, err := c.current(ctx, slug)
	if err != nil {
		return false, err
	}
	if cur != nil && sameVersion(*cur, models, sr.Sector) {
		return false, nil
	}
	if err := c.post(ctx, slug, models, sr.Sector); err != nil {
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
func sameVersion(v version, models []string, sector rank.Sector) bool {
	spec, ok := specs[sector]
	if !ok {
		return false
	}
	if !slices.Equal(v.Config.Models, models) {
		return false
	}
	if strings.TrimSpace(v.SystemPrompt) != spec.systemPrompt {
		return false
	}
	if !reasoningMatches(v, spec) {
		return false
	}
	if v.Config.Verbosity != verbosity {
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
	return v.Config.Reasoning.Effort == reasoningEffort
}

func samplingMatches(v version, spec presetSpec) bool {
	if v.Config.Temperature == nil || *v.Config.Temperature != spec.temperature {
		return false
	}
	if v.Config.TopP == nil || *v.Config.TopP != spec.topP {
		return false
	}
	if v.Config.FrequencyPenalty == nil || *v.Config.FrequencyPenalty != spec.frequencyPenalty {
		return false
	}
	if v.Config.PresencePenalty == nil || *v.Config.PresencePenalty != spec.presencePenalty {
		return false
	}
	return true
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
	reasoning := map[string]any{"effort": reasoningEffort}
	if spec.reasoningEnabled {
		reasoning["enabled"] = true
	}
	body := map[string]any{
		"messages":  []map[string]string{{"role": "system", "content": spec.systemPrompt}},
		"models":    models,
		"verbosity": verbosity,
		"reasoning": reasoning,
	}
	if spec.sampling {
		body["temperature"] = spec.temperature
		body["top_p"] = spec.topP
		body["frequency_penalty"] = spec.frequencyPenalty
		body["presence_penalty"] = spec.presencePenalty
	}
	return body
}

func (c *Client) post(ctx context.Context, slug string, models []string, sector rank.Sector) error {
	spec, ok := specs[sector]
	if !ok {
		return fmt.Errorf("no spec for sector %q", sector)
	}
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
