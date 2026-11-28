package fetch

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Production API roots the selection pipeline builds its URLs from.
// OPENROUTER_BASE_URL and DARKBLOOM_BASE_URL — the same names cmd/dbproxy
// reads — override them for a staging stack or a local end-to-end test.
// The literals repeat what internal/discounts and internal/darkbloom hard
// code: this package is imported by both, so importing them back would
// cycle.
const (
	openRouterBase = "https://openrouter.ai/api/v1"
	darkbloomBase  = "https://api.darkbloom.dev/v1"
)

// EnvClient returns hc with its transport rewriting requests to the
// production API roots onto the env overrides, keeping the path after the
// root (endpoint and per-model URLs follow). Every other host passes
// through untouched, so the score sources keep their production URLs. With
// neither variable set, hc is returned unchanged.
func EnvClient(hc *http.Client) (*http.Client, error) {
	rules := make([]rootRewrite, 0, 2)
	for _, pair := range [][2]string{
		{"OPENROUTER_BASE_URL", openRouterBase},
		{"DARKBLOOM_BASE_URL", darkbloomBase},
	} {
		name, prod := pair[0], pair[1]
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		root, err := apiRoot(name, value)
		if err != nil {
			return nil, err
		}
		if root == prod {
			continue
		}
		rules = append(rules, rootRewrite{from: prod, to: root})
	}
	if len(rules) == 0 {
		return hc, nil
	}
	if hc == nil {
		hc = &http.Client{}
	}
	c := *hc
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = baseRewriter{base: base, rules: rules}
	return &c, nil
}

// apiRoot validates an override: an absolute http(s) root without query or
// fragment and without a trailing slash, so request paths concatenate
// cleanly.
func apiRoot(name, value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", name, value, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s %q must be an absolute http(s) API root without query or fragment", name, value)
	}
	return strings.TrimSuffix(value, "/"), nil
}

// rootRewrite maps one production API root onto its override.
type rootRewrite struct {
	from string // production root, no trailing slash
	to   string // override root, no trailing slash
}

// baseRewriter rewrites requests whose URL starts with a production root,
// keeping method, headers, body and the rest of the path.
type baseRewriter struct {
	base  http.RoundTripper
	rules []rootRewrite
}

// RoundTrip implements http.RoundTripper.
func (t baseRewriter) RoundTrip(r *http.Request) (*http.Response, error) {
	target := r.URL.Scheme + "://" + r.URL.Host + r.URL.Path
	for _, rw := range t.rules {
		rest, ok := strings.CutPrefix(target, rw.from)
		if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
			continue
		}
		to, err := url.Parse(rw.to + rest)
		if err != nil {
			return nil, fmt.Errorf("rewrite %s: %w", target, err)
		}
		rr := r.Clone(r.Context())
		rr.Body = r.Body // Clone does not carry the body
		rr.URL = to
		rr.Host = to.Host
		return t.base.RoundTrip(rr)
	}
	return t.base.RoundTrip(r)
}
