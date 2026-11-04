package epoch

import (
	"archive/zip"
	"bytes"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

const mathCSV = "Model version,mean_score,Release date\n" +
	"vendor-model-4-5_high,0.9,2026-10-01\n" +
	"vendor-model-4-5_low,0.6,2026-10-01\n" +
	"other-model-2026-05-28,0.5,2026-05-28\n" +
	"broken,,2026-01-01\n"

const codeCSV = "Model version,Arena Score,Votes\n" +
	"vendor-model-4-5_max,1500,10\n" +
	"tiny-model,1000,10\n" +
	"other-model,1250,10\n"

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func provider(t *testing.T, status int, body []byte, ids []string) *Provider {
	t.Helper()
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	p := New(fetch.Getter{HC: srv.Client(), Attempts: 1, Backoff: time.Millisecond}, ids)
	p.url = srv.URL
	return p
}

var ids = []string{"vendor/model-4.5", "vendor/model-4.5:free", "vendor/model-4.5:batch", "x/other-model", "x/tiny-model"}

func TestScoresMathAndCode(t *testing.T) {
	body := zipOf(t, map[string]string{"data/" + mathFile: mathCSV, codeFile: codeCSV})
	got := provider(t, http.StatusOK, body, ids).Scores(t.Context())

	m := got[rank.Math]
	if m.Err != "" || m.Source != SourceName {
		t.Fatalf("Math = %+v, want no error", m)
	}
	// best effort variant wins; fraction scaled to 0-100; date tail stripped
	for id, want := range map[string]float64{"vendor/model-4.5": 90, "vendor/model-4.5:free": 90, "x/other-model": 50} {
		if !approx(m.ByModel[id], want) {
			t.Errorf("Math[%s] = %v, want %v", id, m.ByModel[id], want)
		}
	}
	if _, ok := m.ByModel["vendor/model-4.5:batch"]; ok {
		t.Error("Math[:batch] present, want unscored (no duplicate rows)")
	}

	c := got[rank.Code]
	// Elo min-max scaled: 1000 → 0, 1250 → 50, 1500 → 100
	for id, want := range map[string]float64{"vendor/model-4.5": 100, "x/other-model": 50, "x/tiny-model": 0} {
		if !approx(c.ByModel[id], want) {
			t.Errorf("Code[%s] = %v, want %v", id, c.ByModel[id], want)
		}
	}
	if got[rank.CodeFree].ByModel["vendor/model-4.5:free"] != 100 {
		t.Errorf("CodeFree[:free] = %v, want 100", got[rank.CodeFree].ByModel["vendor/model-4.5:free"])
	}
}

func TestScoresDownloadFailureMarksAllSectors(t *testing.T) {
	got := provider(t, http.StatusNotFound, []byte("gone"), ids).Scores(t.Context())
	for _, s := range []rank.Sector{rank.Math, rank.Code, rank.CodeFree} {
		if !strings.HasPrefix(got[s].Err, "status 404") || got[s].Source != SourceName {
			t.Errorf("%s = %+v, want status 404 error from %s", s, got[s], SourceName)
		}
	}
}

func TestScoresMissingFileMarksOnlyItsSector(t *testing.T) {
	body := zipOf(t, map[string]string{mathFile: mathCSV})
	got := provider(t, http.StatusOK, body, ids).Scores(t.Context())
	if got[rank.Math].Err != "" {
		t.Errorf("Math Err = %q, want empty", got[rank.Math].Err)
	}
	if !strings.Contains(got[rank.Code].Err, "missing from zip") {
		t.Errorf("Code Err = %q, want missing-file error", got[rank.Code].Err)
	}
}

func TestScoresMissingColumn(t *testing.T) {
	body := zipOf(t, map[string]string{mathFile: "Model version,other\nx,1\n", codeFile: codeCSV})
	got := provider(t, http.StatusOK, body, ids).Scores(t.Context())
	if !strings.Contains(got[rank.Math].Err, "lacks") {
		t.Errorf("Math Err = %q, want missing-column error", got[rank.Math].Err)
	}
}

func TestScoresUnreadableZip(t *testing.T) {
	got := provider(t, http.StatusOK, []byte("not a zip"), ids).Scores(t.Context())
	if !strings.Contains(got[rank.Math].Err, "unreadable zip") {
		t.Errorf("Math Err = %q, want unreadable-zip error", got[rank.Math].Err)
	}
}

func TestCandidates(t *testing.T) {
	got := candidates("doubao-seed-2.1-turbo-260628_high")
	want := []string{"doubao-seed-2.1-turbo-260628_high", "doubao-seed-2.1-turbo-260628", "doubao-seed-2.1-turbo"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
