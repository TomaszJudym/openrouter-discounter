package popularity

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

const payload = `{"data":[
 {"date":"2026-11-02 00:00:00","model_permaslug":"v/big-20261001","variant":"standard","rankingMetricValue":600000},
 {"date":"2026-11-03 00:00:00","model_permaslug":"v/big-20261001","variant":"standard","rankingMetricValue":400000},
 {"date":"2026-11-03 00:00:00","model_permaslug":"v/big-20261001","variant":"free","total_prompt_tokens":900,"total_completion_tokens":100},
 {"date":"2026-11-03 00:00:00","model_permaslug":"v/mid-20261001","variant":"standard","rankingMetricValue":31622.776601683792},
 {"date":"2026-11-03 00:00:00","model_permaslug":"v/unknown","variant":"standard","rankingMetricValue":5}
]}`

var models = []discounts.Model{
	{ID: "v/big", CanonicalSlug: "v/big-20261001"},
	{ID: "v/big:free", CanonicalSlug: "v/big-20261001"},
	{ID: "v/mid", CanonicalSlug: "v/mid-20261001"},
	{ID: "~v/mid-latest", CanonicalSlug: "v/mid-20261001"},
	{ID: "v/quiet", CanonicalSlug: "v/quiet-20261001"},
}

func provider(t *testing.T, status int, body string) *Provider {
	t.Helper()
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	p := New(fetch.Getter{HC: srv.Client(), Attempts: 1, Backoff: time.Millisecond}, models, rank.LCR)
	p.url = srv.URL
	return p
}

func TestScoresSumsWindowAndLogScales(t *testing.T) {
	got := provider(t, http.StatusOK, payload).Scores(t.Context())[rank.LCR]
	if got.Err != "" || got.Source != SourceName {
		t.Fatalf("LCR = %+v, want no error", got)
	}
	// tokens: big 1e6, big:free 1e3 (prompt+completion fallback), mid 10^4.5
	// the ~alias shares v/mid's canonical slug and gets the same score
	want := map[string]float64{"v/big": 100, "v/big:free": 0, "v/mid": 50, "~v/mid-latest": 50}
	if len(got.ByModel) != len(want) {
		t.Errorf("ByModel = %v, want %v", got.ByModel, want)
	}
	for id, w := range want {
		if math.Abs(got.ByModel[id]-w) > 1e-6 {
			t.Errorf("ByModel[%s] = %v, want %v", id, got.ByModel[id], w)
		}
	}
}

func TestScoresError(t *testing.T) {
	got := provider(t, http.StatusNotFound, `{"error":"Not Found"}`).Scores(t.Context())[rank.LCR]
	if !strings.HasPrefix(got.Err, "status 404") || got.Source != SourceName {
		t.Errorf("LCR = %+v, want status 404 error", got)
	}
}

func TestScoresUnparseable(t *testing.T) {
	got := provider(t, http.StatusOK, "<html>").Scores(t.Context())[rank.LCR]
	if !strings.Contains(got.Err, "unparseable") {
		t.Errorf("Err = %q, want unparseable", got.Err)
	}
}
