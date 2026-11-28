package main

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/darkbloom"
)

const (
	// defaultAddr keeps the proxy on loopback: it holds the Darkbloom key
	// and forwards the client's own OpenRouter key.
	defaultAddr = "127.0.0.1:8787"
	// defaultOpenRouterBase is production; OPENROUTER_BASE_URL overrides it
	// (staging, or an httptest stub in the tests).
	defaultOpenRouterBase = "https://openrouter.ai/api/v1"
	// defaultDarkbloomBase is production; DARKBLOOM_BASE_URL overrides it.
	defaultDarkbloomBase = darkbloom.BaseURL
	// defaultRefresh matches the tracker's intra-day preset reruns.
	defaultRefresh = 2 * time.Hour
	// pipelineTimeout bounds one fetch inside a run; the run itself has
	// runTimeout.
	pipelineTimeout = 60 * time.Second
)

// config is dbproxy's environment. Every field has a production default;
// the base URLs let a staging stack or a test run the whole pipeline
// against stubs instead of the live APIs.
type config struct {
	addr           string        // DBPROXY_ADDR: loopback host:port to listen on
	openRouterBase string        // OPENROUTER_BASE_URL: OpenRouter API root
	darkbloomBase  string        // DARKBLOOM_BASE_URL: Darkbloom API root
	refresh        time.Duration // DBPROXY_REFRESH: re-discovery interval
}

// configFromEnv reads and validates the environment. A non-loopback
// DBPROXY_ADDR is refused because the proxy spends both keys; a bad base
// URL or refresh interval is refused rather than silently defaulted.
func configFromEnv() (config, error) {
	cfg := config{
		addr:           cmp.Or(os.Getenv("DBPROXY_ADDR"), defaultAddr),
		openRouterBase: cmp.Or(os.Getenv("OPENROUTER_BASE_URL"), defaultOpenRouterBase),
		darkbloomBase:  cmp.Or(os.Getenv("DARKBLOOM_BASE_URL"), defaultDarkbloomBase),
		refresh:        defaultRefresh,
	}
	var err error
	if cfg.openRouterBase, err = apiRoot("OPENROUTER_BASE_URL", cfg.openRouterBase); err != nil {
		return config{}, err
	}
	if cfg.darkbloomBase, err = apiRoot("DARKBLOOM_BASE_URL", cfg.darkbloomBase); err != nil {
		return config{}, err
	}
	if host, _, err := net.SplitHostPort(cfg.addr); err != nil || !net.ParseIP(host).IsLoopback() {
		return config{}, fmt.Errorf("DBPROXY_ADDR %q must be a loopback address: the proxy spends your keys", cfg.addr)
	}
	if v := os.Getenv("DBPROXY_REFRESH"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, fmt.Errorf("DBPROXY_REFRESH %q: %w", v, err)
		}
		if d <= 0 {
			return config{}, errors.New("DBPROXY_REFRESH must be a positive duration")
		}
		cfg.refresh = d
	}
	return cfg, nil
}

// apiRoot validates an API root: an absolute http(s) URL with no query or
// fragment, and no trailing slash so request paths concatenate cleanly.
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

// pipelineClient is the client pipeline.Run fetches through. It rewrites
// the pipeline's fixed production API roots onto the configured ones:
// internal/discounts builds the catalog and endpoint URLs from constants
// and internal/pipeline hands darkbloom.BaseURL to internal/darkbloom, so
// an override has to happen at the transport. Every other host and path
// (the score sources) passes through untouched; with the production
// defaults the rewrite is a no-op.
func pipelineClient(cfg config) (*http.Client, error) {
	rules := make([]rootRewrite, 0, 2)
	for _, pair := range [][2]string{
		{defaultOpenRouterBase, cfg.openRouterBase},
		{defaultDarkbloomBase, cfg.darkbloomBase},
	} {
		from, err := url.Parse(pair[0])
		if err != nil {
			return nil, fmt.Errorf("production API root %q: %w", pair[0], err)
		}
		to, err := url.Parse(pair[1])
		if err != nil {
			return nil, fmt.Errorf("configured API root %q: %w", pair[1], err)
		}
		rules = append(rules, rootRewrite{from: from, to: to})
	}
	return &http.Client{
		Timeout:   pipelineTimeout,
		Transport: baseRewriter{base: http.DefaultTransport, rules: rules},
	}, nil
}

// rootRewrite maps one production API root onto the configured one.
type rootRewrite struct {
	from *url.URL
	to   *url.URL
}

// baseRewriter rewrites requests whose URL starts with a production API
// root, keeping the rest of the path and the method, headers and body.
type baseRewriter struct {
	base  http.RoundTripper
	rules []rootRewrite
}

// RoundTrip implements http.RoundTripper.
func (t baseRewriter) RoundTrip(r *http.Request) (*http.Response, error) {
	for _, rw := range t.rules {
		if rw.from.Host != r.URL.Host {
			continue
		}
		rest, ok := strings.CutPrefix(r.URL.Path, rw.from.Path)
		if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
			continue
		}
		rr := r.Clone(r.Context())
		rr.Body = r.Body // Clone does not carry the body
		rr.URL.Scheme, rr.URL.Host = rw.to.Scheme, rw.to.Host
		rr.URL.Path = strings.TrimSuffix(rw.to.Path, "/") + rest
		rr.Host = rw.to.Host
		return t.base.RoundTrip(rr)
	}
	return t.base.RoundTrip(r)
}
