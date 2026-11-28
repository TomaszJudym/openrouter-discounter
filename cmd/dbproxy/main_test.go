package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/pipeline"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
)

// The refresh and logRoutes tests need the pipeline's HTTP surface — the
// OpenRouter catalog and endpoints, Darkbloom's model list and the score
// sources — answering from one place, because refresh builds its own client
// (the default transport) and cannot take an injected one. stubAPI serves
// every request from one httptest server after the transport rewrites its
// URL to it, dispatching by method and path; unscripted paths get a 404,
// which every pipeline caller treats as a degradation, never a retry.

// catalogJSON is one tool-calling chat model with an intelligence index and
// no endpoint data (the endpoints fetch 404s), so the OpenRouter side finds
// no discounts.
const catalogJSON = `{"data":[{"id":"mistralai/mistral-small","canonical_slug":"mistralai/mistral-small",` +
	`"hugging_face_id":"mistralai/Mistral-Small-3","supported_parameters":["tools"],` +
	`"benchmarks":{"artificial_analysis":{"intelligence_index":50}},"context_length":2000000}]}`

// darkbloomJSON is the same model served directly at a fifth of a cheap
// list price, routable now.
const darkbloomJSON = `{"data":[{"id":"mistral-small-4bit","hugging_face_id":"mistralai/Mistral-Small-3",` +
	`"input_modalities":["text"],"context_length":2000000,"max_output_length":100000,` +
	`"pricing":{"prompt":"0.0000001","completion":"0.0000004"},"metadata":{"can_accept":true,` +
	`"routable_providers":1}}]}`

// darkbloomTwoBuildsJSON serves the catalog model twice in one list, the
// 4-bit build cheaper, so the direct row picks that build.
const darkbloomTwoBuildsJSON = `{"data":[` +
	`{"id":"mistral-small-4bit","hugging_face_id":"mistralai/Mistral-Small-3",` +
	`"input_modalities":["text"],"context_length":2000000,"max_output_length":100000,` +
	`"pricing":{"prompt":"0.0000001","completion":"0.0000004"},"metadata":{"can_accept":true,` +
	`"routable_providers":1}},` +
	`{"id":"mistral-small-8bit","hugging_face_id":"mistralai/Mistral-Small-3",` +
	`"input_modalities":["text"],"context_length":2000000,"max_output_length":100000,` +
	`"pricing":{"prompt":"0.0000002","completion":"0.0000008"},"metadata":{"can_accept":true,` +
	`"routable_providers":1}}]}`

// darkbloomTwoBuildsSwappedJSON is the same list with the prices flipped: the
// 8-bit build is now the row's cheapest offer.
const darkbloomTwoBuildsSwappedJSON = `{"data":[` +
	`{"id":"mistral-small-4bit","hugging_face_id":"mistralai/Mistral-Small-3",` +
	`"input_modalities":["text"],"context_length":2000000,"max_output_length":100000,` +
	`"pricing":{"prompt":"0.0000004","completion":"0.0000016"},"metadata":{"can_accept":true,` +
	`"routable_providers":1}},` +
	`{"id":"mistral-small-8bit","hugging_face_id":"mistralai/Mistral-Small-3",` +
	`"input_modalities":["text"],"context_length":2000000,"max_output_length":100000,` +
	`"pricing":{"prompt":"0.0000002","completion":"0.0000008"},"metadata":{"can_accept":true,` +
	`"routable_providers":1}}]}`

// stubAPI is the scripted pipeline surface: it records requests and answers
// from script (keys "METHOD path"), defaulting to a 404.
type stubAPI struct {
	mu     sync.Mutex
	script map[string]reply
	got    []string
	srv    *httptest.Server
	base   http.RoundTripper
}

func newStubAPI(t *testing.T, script map[string]reply) *stubAPI {
	t.Helper()
	s := &stubAPI{script: maps.Clone(script), base: http.DefaultTransport}
	if s.script == nil {
		s.script = map[string]reply{}
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		s.mu.Lock()
		s.got = append(s.got, r.Method+" "+r.URL.Path)
		rep, ok := s.script[r.Method+" "+r.URL.Path]
		s.mu.Unlock()
		if !ok {
			rep = reply{status: http.StatusNotFound, body: `{"error":{"code":"not_scripted"}}`}
		}
		w.Header().Set("Content-Type", cmpOr(rep.ctype, "application/json"))
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// transport rewrites every request onto the stub server, keeping the
// original method, headers and path.
func (s *stubAPI) transport() http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		rr := r.Clone(r.Context())
		rr.Body = r.Body // Clone does not carry the body
		rr.URL.Scheme = "http"
		rr.URL.Host = s.srv.Listener.Addr().String()
		return s.base.RoundTrip(rr)
	})
}

// set replaces one scripted reply, so a test can change an upstream price
// between two refreshes.
func (s *stubAPI) set(key string, rep reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script[key] = rep
}

// called reports whether the stub saw a request for key.
func (s *stubAPI) called(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.got, key)
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

// TestRefreshRoutesDarkbloomFirst: a successful run repoints the proxy at
// the Darkbloom-direct picks. With no affordable OpenRouter discount, every
// route goes darkbloom first, and each route's direct list holds the
// Darkbloom model ids in rank order.
func TestRefreshRoutesDarkbloomFirst(t *testing.T) {
	api := newStubAPI(t, map[string]reply{
		"GET /api/v1/models": {status: http.StatusOK, body: catalogJSON},
		"GET /v1/models":     {status: http.StatusOK, body: darkbloomJSON},
	})
	hc := &http.Client{Transport: api.transport()}
	p := &Proxy{HC: hc}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		refresh(ctx, p, pipeline.Keys{Darkbloom: dbKey}, hc, time.Hour)
	}()
	waitFor(t, 30*time.Second, func() bool {
		rt := p.routes.Load()
		return rt != nil && len(*rt) > 0
	})
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("refresh did not exit after cancel")
	}
	if !api.called("GET /api/v1/models") {
		t.Error("pipeline never fetched the OpenRouter catalog")
	}
	if !api.called("GET /api/v1/models/mistralai/mistral-small/endpoints") {
		t.Error("pipeline never scanned the model's endpoints")
	}
	rt := *p.routes.Load()
	if len(rt) == 0 {
		t.Fatal("no routes set")
	}
	for slug, r := range rt {
		if !slices.Equal(r.direct, []string{"mistral-small-4bit"}) {
			t.Errorf("%s direct = %v, want the Darkbloom model id", slug, r.direct)
		}
		if r.dbFirst {
			continue
		}
		t.Errorf("%s routes openrouter first although OpenRouter has no pick", slug)
	}
}

// TestRefreshDarkbloomFailureClearsRoutes: when the Darkbloom side fails the
// proxy serves OpenRouter only, and the error text never carries the key.
func TestRefreshDarkbloomFailureClearsRoutes(t *testing.T) {
	api := newStubAPI(t, map[string]reply{
		"GET /api/v1/models": {status: http.StatusOK, body: catalogJSON},
		"GET /v1/models":     {status: http.StatusForbidden, body: `{"error":{"message":"bad key ` + dbKey + `"}}`},
	})
	hc := &http.Client{Transport: api.transport()}
	p := &Proxy{HC: hc}
	p.SetRoutes(routes{"hermes-review": {direct: []string{"stale-1"}, dbFirst: true}})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		refresh(ctx, p, pipeline.Keys{Darkbloom: dbKey}, hc, time.Hour)
	}()
	waitFor(t, 30*time.Second, func() bool {
		rt := p.routes.Load()
		return rt != nil && len(*rt) == 0
	})
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("refresh did not exit after cancel")
	}
	out := buf.String()
	if !strings.Contains(out, "Darkbloom unavailable") {
		t.Errorf("log = %q, want the Darkbloom-unavailable notice", out)
	}
	if strings.Contains(out, dbKey) || !strings.Contains(out, "REDACTED") {
		t.Errorf("log = %q, want the key redacted", out)
	}
}

// TestRefreshKeepsRoutesWhenRunFails: a failed run keeps the previous
// routes; the proxy never serves an empty table because one refresh failed.
func TestRefreshKeepsRoutesWhenRunFails(t *testing.T) {
	p := &Proxy{HC: &http.Client{}}
	p.SetRoutes(routes{"hermes-review": {direct: []string{"kept-1"}, dbFirst: true}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // every fetch fails at once, so the first run errors out
	done := make(chan struct{})
	go func() {
		defer close(done)
		refresh(ctx, p, pipeline.Keys{Darkbloom: dbKey}, &http.Client{}, time.Hour)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("refresh did not return after a failed run")
	}
	rt := *p.routes.Load()
	if r := rt["hermes-review"]; !r.dbFirst || !slices.Equal(r.direct, []string{"kept-1"}) {
		t.Errorf("hermes-review = %+v, want the previous route kept", r)
	}
}

// TestLogRoutes: one line per managed slug — first side and the
// Darkbloom picks for routed slugs, "openrouter only" for the rest.
func TestLogRoutes(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	logRoutes(routes{
		"admech-math": {direct: []string{"db-a", "db-b"}, dbFirst: true},
		"admech-long": {direct: []string{"db-c"}},
	})
	out := buf.String()
	want := []string{
		"admech-math: darkbloom first, darkbloom [db-a db-b]",
		"admech-long: openrouter first, darkbloom [db-c]",
		"hermes-delegate: openrouter only",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("log = %q, want the line %q", out, w)
		}
	}
	if lines := strings.Count(strings.TrimSpace(out), "\n") + 1; lines != len(preset.SortedSlugs()) {
		t.Errorf("log has %d lines, want one per managed slug (%d)", lines, len(preset.SortedSlugs()))
	}
}

// TestRefreshPicksUpPriceFlip: a refresh reruns discovery, so a price change
// at the upstream moves the route without a restart. The ticker drives the
// run (here 20 ms; production takes DBPROXY_REFRESH), and the new table is
// swapped in atomically at the end of the run.
func TestRefreshPicksUpPriceFlip(t *testing.T) {
	api := newStubAPI(t, map[string]reply{
		"GET /api/v1/models": {status: http.StatusOK, body: catalogJSON},
		"GET /v1/models":     {status: http.StatusOK, body: darkbloomTwoBuildsJSON},
	})
	hc := &http.Client{Transport: api.transport()}
	p := &Proxy{HC: hc}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		refresh(ctx, p, pipeline.Keys{Darkbloom: dbKey}, hc, 20*time.Millisecond)
	}()
	waitFor(t, 30*time.Second, func() bool {
		r, ok := p.route("hermes-light")
		return ok && slices.Equal(r.direct, []string{"mistral-small-4bit"})
	})
	api.set("GET /v1/models", reply{status: http.StatusOK, body: darkbloomTwoBuildsSwappedJSON})
	waitFor(t, 30*time.Second, func() bool {
		r, ok := p.route("hermes-light")
		return ok && slices.Equal(r.direct, []string{"mistral-small-8bit"})
	})
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("refresh did not exit after cancel")
	}
}
