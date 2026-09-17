package aa

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

const aaPayload = `{"status":200,"data":[
 {"id":"1","name":"Model A","slug":"model-a","model_creator":{"slug":"vendor-a"},
  "evaluations":{"artificial_analysis_math_index":68.25,"lcr":0.8533,"tau_banking":0.4722}},
 {"id":"2","name":"Model B","slug":"model-b","model_creator":{"slug":"vendor-b"},
  "evaluations":{"artificial_analysis_math_index":55.0}},
 {"id":"3","name":"Unrelated Model","slug":"other-model","model_creator":{"slug":"vendor-c"},
  "evaluations":{"lcr":0.9}}
]}`

func providerFor(t *testing.T, body string, status int, orIDs []string) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	p := New(srv.Client(), "test-key", orIDs)
	p.BaseURL = srv.URL
	p.Attempts = 1
	return p, srv
}

func TestScoresMatchesOpenRouterIDs(t *testing.T) {
	p, _ := providerFor(t, aaPayload, http.StatusOK, []string{
		"vendor-a/model.a", "vendor-b/model-b", "nomatch/model-x",
	})
	scores, err := p.Scores(t.Context())
	if err != nil {
		t.Fatalf("Scores() error = %v, want nil", err)
	}
	if got := scores[rank.Math]["vendor-a/model.a"]; got != 68.25 {
		t.Errorf("Math[model.a] = %v, want 68.25", got)
	}
	if got := scores[rank.LCR]["vendor-a/model.a"]; !approx(got, 85.33) {
		t.Errorf("LCR[model.a] = %v, want 85.33 (fraction scaled to 0-100)", got)
	}
	if got := scores[rank.Finance]["vendor-a/model.a"]; !approx(got, 47.22) {
		t.Errorf("Finance[model.a] = %v, want 47.22", got)
	}
	if _, ok := scores[rank.Math]["vendor-b/model-b"]; !ok {
		t.Error("Math[model-b] missing, want 55.0")
	}
	if _, ok := scores[rank.LCR]["vendor-b/model-b"]; ok {
		t.Error("LCR[model-b] present, want absent")
	}
	if _, ok := scores[rank.LCR]["nomatch/model-x"]; ok {
		t.Error("LCR[unmatched id] present, want absent")
	}
}

func TestScoresMissingKey(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	p := New(srv.Client(), "", []string{"a/b"})
	if _, err := p.Scores(t.Context()); err == nil || !strings.Contains(err.Error(), "AA_API_KEY not set") {
		t.Fatalf("Scores() error = %v, want missing-key error", err)
	}
}

func TestScoresErrorIsSanitizedAndTruncated(t *testing.T) {
	secret := "aa_secret_value_123"
	body := strings.Repeat("leak "+secret+" ", 40)
	p, _ := providerFor(t, body, http.StatusForbidden, []string{"a/b"})
	p.key = secret
	_, err := p.Scores(t.Context())
	if err == nil {
		t.Fatal("Scores() error = nil, want non-200 error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error contains the API key: %q", err.Error())
	}
	if got := len([]rune(err.Error())); got > maxErrRunes {
		t.Errorf("error length = %d runes, want <= %d", got, maxErrRunes)
	}
}

func TestScoresUnparseableResponse(t *testing.T) {
	p, _ := providerFor(t, "{not json", http.StatusOK, []string{"a/b"})
	if _, err := p.Scores(t.Context()); err == nil || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("Scores() error = %v, want unparseable-response error", err)
	}
}

func TestSanitizeRedactsAndTruncates(t *testing.T) {
	got := sanitize("prefix "+strings.Repeat("x", 200), "secret")
	if strings.Contains(got, "secret") {
		t.Errorf("sanitize() kept the secret: %q", got)
	}
	if n := len([]rune(got)); n > maxErrRunes {
		t.Errorf("sanitize() length = %d, want <= %d", n, maxErrRunes)
	}
}

func TestNormalizeMatching(t *testing.T) {
	if got := normalize("Vendor-A/Model.B"); got != "vendoramodelb" {
		t.Errorf("normalize = %q, want vendoramodelb", got)
	}
	if got := normalize("openai/gpt-5.2"); got != "openaigpt52" {
		t.Errorf("normalize = %q, want openaigpt52", got)
	}
}

func TestResponseDecode(t *testing.T) {
	var r response
	if err := json.Unmarshal([]byte(aaPayload), &r); err != nil {
		t.Fatalf("Unmarshal error = %v, want nil", err)
	}
	if r.Status != 200 || len(r.Data) != 3 {
		t.Errorf("decoded status/data = %d/%d, want 200/3", r.Status, len(r.Data))
	}
}

func approx(a, b float64) bool { return abs(a-b) < 1e-9 }

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
