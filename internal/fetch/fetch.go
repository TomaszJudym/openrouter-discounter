// Package fetch performs bounded, retried HTTP GETs for the score sources and
// sanitizes error text for display in logs and the Telegram notice.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Defaults for Getter fields left zero.
const (
	DefaultAttempts = 3
	DefaultBackoff  = 2 * time.Second
	DefaultMaxBytes = 32 << 20

	// MaxErrRunes caps sanitized error text.
	MaxErrRunes = 120
)

// Getter fetches a URL with retries on transient failures (network error,
// 429, 5xx). Zero fields take the package defaults.
type Getter struct {
	HC       *http.Client
	Attempts int
	Backoff  time.Duration
	MaxBytes int64
}

// Get returns the body of a 200 response. hdr is added to the request.
// Every returned error is sanitized: secret is removed and the text is
// truncated to MaxErrRunes.
func (g Getter) Get(ctx context.Context, target string, hdr http.Header, secret string) ([]byte, error) {
	attempts := cmpOr(g.Attempts, DefaultAttempts)
	backoff := cmpOr(g.Backoff, DefaultBackoff)
	maxBytes := cmpOr(g.MaxBytes, DefaultMaxBytes)
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, errors.New(Sanitize(ctx.Err().Error(), secret))
			case <-time.After(backoff):
				backoff *= 2
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, errors.New(Sanitize("build request: "+err.Error(), secret))
		}
		for k, vs := range hdr {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err := g.HC.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		_ = resp.Body.Close() // read path: a close error is not actionable
		switch {
		case readErr != nil:
			lastErr = fmt.Errorf("read response: %w", readErr)
		case resp.StatusCode == http.StatusOK && int64(len(body)) > maxBytes:
			// a cut-off body would fail later with a misleading parse error
			return nil, fmt.Errorf("response exceeds %d bytes", maxBytes)
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("status %d: %.200s", resp.StatusCode, body)
		default:
			return nil, errors.New(Sanitize(fmt.Sprintf("status %d: %.200s", resp.StatusCode, body), secret))
		}
	}
	return nil, errors.New(Sanitize(fmt.Sprintf("retries exhausted: %v", lastErr), secret))
}

// Sanitize removes secret from s (exact match) and truncates to MaxErrRunes.
func Sanitize(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "REDACTED")
	}
	if utf8.RuneCountInString(s) <= MaxErrRunes {
		return s
	}
	r := []rune(s)
	return string(append(r[:MaxErrRunes-1], '…'))
}

func cmpOr[T int | int64 | time.Duration](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}
