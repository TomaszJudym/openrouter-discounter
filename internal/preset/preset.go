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

// systemPrompt is the system instruction written into every preset version.
var systemPrompt = strings.TrimSpace(systemPromptFile)

// verbosity is requested on every update even when the routed model does not
// support it: the API stores it in the preset config, and frontier models
// that understand it pick it up.
const verbosity = "low"

// Slugs maps ranked sectors to the user's preset slugs.
var Slugs = map[rank.Sector]string{
	rank.Math:    "admech-math",
	rank.LCR:     "admech-long",
	rank.Finance: "admech-finance",
}

const TopN = 3

type version struct {
	Version      int    `json:"version"`
	SystemPrompt string `json:"system_prompt"`
	Config       struct {
		Models    []string `json:"models"`
		Verbosity string   `json:"verbosity"`
	} `json:"config"`
}

type presetResponse struct {
	Data struct {
		Slug              string  `json:"slug"`
		DesignatedVersion version `json:"designated_version"`
	} `json:"data"`
}

// Client updates presets with the OpenRouter API. A zero key disables
// nothing here — callers skip the client entirely when no key is configured.
type Client struct {
	hc   *http.Client
	key  string
	Base string // overridable for tests
}

// New builds a client authenticating with the OpenRouter API key.
func New(hc *http.Client, key string) *Client {
	return &Client{hc: hc, key: key, Base: "https://openrouter.ai/api/v1"}
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
	if cur != nil && sameVersion(*cur, models) {
		return false, nil
	}
	if err := c.post(ctx, slug, models); err != nil {
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
func sameVersion(v version, models []string) bool {
	return strings.TrimSpace(v.SystemPrompt) == systemPrompt &&
		v.Config.Verbosity == verbosity &&
		slices.Equal(v.Config.Models, models)
}

func (c *Client) current(ctx context.Context, slug string) (*version, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/presets/"+slug, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET preset %s: %w", slug, err)
	}
	defer resp.Body.Close()
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

func (c *Client) post(ctx context.Context, slug string, models []string) error {
	body, err := json.Marshal(map[string]any{
		"messages":  []map[string]string{{"role": "system", "content": systemPrompt}},
		"models":    models,
		"verbosity": verbosity,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.Base+"/presets/"+slug+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("POST preset %s: %w", slug, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST preset %s: status %d: %.300s", slug, resp.StatusCode, respBody)
	}
	return nil
}
