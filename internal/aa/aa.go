// Package aa fetches Artificial Analysis benchmark scores and maps them to
// OpenRouter model ids. It implements rank.Provider.
package aa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"encoding/json/v2"

	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// BaseURL is the Artificial Analysis data API endpoint.
const BaseURL = "https://artificialanalysis.ai/api/v2/data/llms/models"

const (
	maxBodyBytes  = 16 << 20
	maxErrRunes   = 120
	retryAttempts = 3
	baseBackoff   = 2 * time.Second
)

// Sector field names in the AA evaluations object. Math Index is already
// 0-100; LCR and tau_banking are 0-1 fractions reported as 0-100.
const (
	fieldMath = "artificial_analysis_math_index"
	fieldLCR  = "lcr"
	fieldBank = "tau_banking"
	fracToPct = 100
)

type evaluations struct {
	ArtificialAnalysisMathIndex float64 `json:"artificial_analysis_math_index"`
	LCR                         float64 `json:"lcr"`
	TauBanking                  float64 `json:"tau_banking"`
}

type model struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Slug        string      `json:"slug"`
	Creator     creator     `json:"model_creator"`
	Evaluations evaluations `json:"evaluations"`
}

type creator struct {
	Slug string `json:"slug"`
}

type response struct {
	Status int     `json:"status"`
	Data   []model `json:"data"`
}

// Provider fetches AA scores once per run and matches them to the OpenRouter
// model ids given at construction.
type Provider struct {
	hc       *http.Client
	key      string
	ids      []string
	BaseURL  string        // overridable for tests
	Attempts int           // retry attempts; 0 = default
	Backoff  time.Duration // first backoff step; 0 = default
}

// New builds a provider that matches AA scores to orIDs (OpenRouter ids).
func New(hc *http.Client, key string, orIDs []string) *Provider {
	return &Provider{hc: hc, key: key, ids: orIDs, BaseURL: BaseURL, Attempts: retryAttempts, Backoff: baseBackoff}
}

// Scores implements rank.Provider. A fetch-level error is returned once for
// all sectors; a successful fetch with no scores for a sector simply omits
// that sector's key. Returned error text is sanitized: the API key value is
// removed and the message is truncated.
func (p *Provider) Scores(ctx context.Context) (map[rank.Sector]map[string]float64, error) {
	if p.key == "" {
		return nil, errors.New(sanitize("AA_API_KEY not set", ""))
	}
	matches := p.matchTable()
	body, err := p.get(ctx, p.BaseURL)
	if err != nil {
		return nil, err // already sanitized
	}
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errors.New(sanitize(fmt.Sprintf("unparseable response: %v", err), p.key))
	}
	out := map[rank.Sector]map[string]float64{
		rank.Math:    {},
		rank.LCR:     {},
		rank.Finance: {},
	}
	for _, m := range r.Data {
		id, ok := matches[normalize(m.Slug)]
		if !ok {
			id, ok = matches[normalize(m.Name)]
		}
		if !ok {
			id, ok = matches[normalize(m.Creator.Slug+"-"+m.Slug)]
		}
		if !ok {
			continue
		}
		if v := m.Evaluations.ArtificialAnalysisMathIndex; v != 0 {
			out[rank.Math][id] = v
		}
		if v := m.Evaluations.LCR; v != 0 {
			out[rank.LCR][id] = v * fracToPct
		}
		if v := m.Evaluations.TauBanking; v != 0 {
			out[rank.Finance][id] = v * fracToPct
		}
	}
	return out, nil
}

// get fetches the endpoint with bounded retries on transient failures.
func (p *Provider) get(ctx context.Context, url string) ([]byte, error) {
	attempts := p.Attempts
	if attempts <= 0 {
		attempts = retryAttempts
	}
	backoff := p.Backoff
	if backoff <= 0 {
		backoff = baseBackoff
	}
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, errors.New(sanitize(ctx.Err().Error(), p.key))
			case <-time.After(backoff):
				backoff *= 2
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, errors.New(sanitize("build request: "+err.Error(), p.key))
		}
		req.Header.Set("x-api-key", p.key)
		resp, err := p.hc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("GET %s: %w", url, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		switch {
		case readErr != nil:
			lastErr = fmt.Errorf("read response: %w", readErr)
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case isTransient(resp.StatusCode):
			lastErr = fmt.Errorf("status %d: %.200s", resp.StatusCode, body)
		default:
			return nil, errors.New(sanitize(fmt.Sprintf("status %d: %.200s", resp.StatusCode, body), p.key))
		}
	}
	return nil, errors.New(sanitize(fmt.Sprintf("retries exhausted: %v", lastErr), p.key))
}

func isTransient(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// matchTable maps normalized AA identifiers to OpenRouter model ids.
func (p *Provider) matchTable() map[string]string {
	m := make(map[string]string, len(p.ids))
	for _, id := range p.ids {
		id = strings.TrimPrefix(id, "~")
		m[normalize(id)] = id
		if _, tail, ok := strings.Cut(id, "/"); ok {
			m[normalize(tail)] = id
		}
	}
	return m
}

// normalize strips punctuation so OpenRouter ids (dots, slashes, suffixes)
// line up with AA slugs (dashes).
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		}
	}
	return b.String()
}

// sanitize removes the API key value from s and truncates to maxErrRunes.
func sanitize(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "REDACTED")
	}
	return truncate(s, maxErrRunes)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	r = append(r[:n-1], '…')
	return string(r)
}
