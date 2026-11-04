package aa

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

const key = "test-key-123"

var pages = map[string]string{
	"1": `{"tier":"free","pagination":{"page":1,"has_more":true},"data":[
 {"name":"Model A (Reasoning)","slug":"model-a","model_creator":{"name":"Vendor-A"},
  "evaluations":{"artificial_analysis_finance_and_accounting_index":47.2}},
 {"name":"Model A (Non-reasoning)","slug":"model-a-non-reasoning","model_creator":{"name":"Vendor-A"},
  "evaluations":{"artificial_analysis_finance_and_accounting_index":30.1}},
 {"name":"Model B","slug":"model-b","model_creator":{"name":"Vendor-B"},
  "evaluations":{"artificial_analysis_finance_and_accounting_index":null}}
]}`,
	"2": `{"tier":"free","pagination":{"page":2,"has_more":false},"data":[
 {"name":"Model C","slug":"model-c","model_creator":{"name":"Vendor-C"},
  "evaluations":{"artificial_analysis_finance_and_accounting_index":55.5}}
]}`,
}

func provider(t *testing.T, h http.HandlerFunc, apiKey string, ids []string) *Provider {
	t.Helper()
	srv := httptest.NewTestServer(t, h)
	p := New(fetch.Getter{HC: srv.Client(), Attempts: 1, Backoff: time.Millisecond}, apiKey, ids)
	p.baseURL = srv.URL
	return p
}

func TestScoresPaginatesAndMatches(t *testing.T) {
	var calls int
	p := provider(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("x-api-key") != key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(pages[r.URL.Query().Get("page")]))
	}, key, []string{"vendor-a/model-a", "vendor-a/model-a:free", "vendor-b/model-b", "vendor-c/model.c"})
	got := p.Scores(t.Context())[rank.Finance]
	if got.Err != "" || got.Source != SourceName {
		t.Fatalf("Finance = %+v, want no error from %s", got, SourceName)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 pages", calls)
	}
	want := map[string]float64{"vendor-a/model-a": 47.2, "vendor-a/model-a:free": 47.2, "vendor-c/model.c": 55.5}
	if len(got.ByModel) != len(want) {
		t.Errorf("ByModel = %v, want %v", got.ByModel, want)
	}
	for id, v := range want {
		if got.ByModel[id] != v {
			t.Errorf("ByModel[%s] = %v, want %v", id, got.ByModel[id], v)
		}
	}
}

func TestScoresMissingKey(t *testing.T) {
	p := provider(t, func(http.ResponseWriter, *http.Request) { t.Error("request sent without key") }, "", nil)
	if got := p.Scores(t.Context())[rank.Finance]; !strings.Contains(got.Err, "AA_API_KEY not set") {
		t.Errorf("Err = %q, want missing-key error", got.Err)
	}
}

func TestScoresProErrorRedactedAndTruncated(t *testing.T) {
	p := provider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"key ` + key + ` requires a Pro subscription ` + strings.Repeat("x", 200) + `"}`))
	}, key, nil)
	got := p.Scores(t.Context())[rank.Finance]
	if !strings.HasPrefix(got.Err, "status 403") {
		t.Errorf("Err = %q, want status 403", got.Err)
	}
	if strings.Contains(got.Err, key) || !strings.Contains(got.Err, "REDACTED") {
		t.Errorf("Err = %q, want key redacted", got.Err)
	}
	if n := utf8.RuneCountInString(got.Err); n > fetch.MaxErrRunes {
		t.Errorf("Err length = %d, want <= %d", n, fetch.MaxErrRunes)
	}
}

func TestScoresUnparseable(t *testing.T) {
	p := provider(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }, key, nil)
	if got := p.Scores(t.Context())[rank.Finance]; !strings.Contains(got.Err, "unparseable") {
		t.Errorf("Err = %q, want unparseable-response error", got.Err)
	}
}
