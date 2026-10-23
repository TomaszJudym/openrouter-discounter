package aa

import (
	"encoding/json/v2"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

const aaPayload = `{"status":200,"data":[
 {"id":"1","name":"Model A","slug":"model-a","model_creator":{"slug":"vendor-a"},
  "evaluations":{"artificial_analysis_math_index":68.25,"lcr":0.8533,"tau_banking":0.4722,"artificial_analysis_coding_index":71.4}},
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
	p.baseURL = srv.URL
	p.attempts = 1
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
	if got := scores[rank.Code]["vendor-a/model.a"]; got != 71.4 {
		t.Errorf("Code[model.a] = %v, want 71.4", got)
	}
	if _, ok := scores[rank.Code]["vendor-b/model-b"]; ok {
		t.Error("Code[model-b] present, want absent")
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

func TestMatchTableFreeSuffix(t *testing.T) {
	p := New(nil, "k", []string{
		"google/gemini-2.5-flash:free",
		"google/gemini-2.5-flash",
	})
	m := p.matchTable()
	// AA slug "gemini-2-5-flash" → normalized "gemini25flash" should match the :free id
	if got := m["gemini25flash"]; got != "google/gemini-2.5-flash:free" && got != "google/gemini-2.5-flash" {
		t.Errorf("matchTable[gemini25flash] = %q, want either free or base id", got)
	}
	// AA creator+slug "google-gemini-2-5-flash" → "googlegemini25flash"
	if got := m["googlegemini25flash"]; got == "" {
		t.Error("matchTable[googlegemini25flash] missing")
	}
}

func TestIntelligenceMapsSlugsToIDs(t *testing.T) {
	p, _ := providerFor(t, `{"status":200,"data":[
	 {"id":"1","name":"Model A","slug":"model-a","model_creator":{"slug":"vendor"},
	  "evaluations":{"artificial_analysis_intelligence_index":85.5}},
	 {"id":"2","name":"Model B","slug":"model-b","model_creator":{"slug":"vendor"},
	  "evaluations":{"artificial_analysis_intelligence_index":70.0}}
	]}`, http.StatusOK, []string{"vendor/model.a", "vendor/model-b"})
	got, err := p.Intelligence(t.Context())
	if err != nil {
		t.Fatalf("Intelligence() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Errorf("Intelligence() = %d entries, want 2", len(got))
	}
	if got["vendor/model.a"] != 85.5 {
		t.Errorf("Intelligence[vendor/model.a] = %v, want 85.5", got["vendor/model.a"])
	}
	if got["vendor/model-b"] != 70.0 {
		t.Errorf("Intelligence[vendor/model-b] = %v, want 70.0", got["vendor/model-b"])
	}
}

func TestIntelligenceThinkingSuffixStripsAndTakesPrecedence(t *testing.T) {
	p, _ := providerFor(t, `{"status":200,"data":[
	 {"id":"1","slug":"model-a","model_creator":{"slug":"vendor"},
	  "evaluations":{"artificial_analysis_intelligence_index":60.0}},
	 {"id":"2","slug":"model-a-thinking","model_creator":{"slug":"vendor"},
	  "evaluations":{"artificial_analysis_intelligence_index":90.0}}
	]}`, http.StatusOK, []string{"vendor/model-a"})
	got, err := p.Intelligence(t.Context())
	if err != nil {
		t.Fatalf("Intelligence() error = %v, want nil", err)
	}
	if got["vendor/model-a"] != 90.0 {
		t.Errorf("Intelligence[vendor/model-a] = %v, want 90.0 (thinking variant)", got["vendor/model-a"])
	}
}

func TestIntelligenceBaseFillsWithoutThinkingVariant(t *testing.T) {
	p, _ := providerFor(t, `{"status":200,"data":[
	 {"id":"1","slug":"model-a","model_creator":{"slug":"vendor"},
	  "evaluations":{"artificial_analysis_intelligence_index":75.0}}
	]}`, http.StatusOK, []string{"vendor/model-a"})
	got, err := p.Intelligence(t.Context())
	if err != nil {
		t.Fatalf("Intelligence() error = %v, want nil", err)
	}
	if got["vendor/model-a"] != 75.0 {
		t.Errorf("Intelligence[vendor/model-a] = %v, want 75.0 (base)", got["vendor/model-a"])
	}
}

func TestIntelligenceOmitsZeroAndNull(t *testing.T) {
	p, _ := providerFor(t, `{"status":200,"data":[
	 {"id":"1","slug":"model-a","model_creator":{"slug":"vendor"},
	  "evaluations":{"artificial_analysis_intelligence_index":0}},
	 {"id":"2","slug":"model-b","model_creator":{"slug":"vendor"}}
	]}`, http.StatusOK, []string{"vendor/model-a", "vendor/model-b"})
	got, err := p.Intelligence(t.Context())
	if err != nil {
		t.Fatalf("Intelligence() error = %v, want nil", err)
	}
	if _, ok := got["vendor/model-a"]; ok {
		t.Error("Intelligence[vendor/model-a] present, want absent (zero)")
	}
	if _, ok := got["vendor/model-b"]; ok {
		t.Error("Intelligence[vendor/model-b] present, want absent (null)")
	}
}

func TestFetchSharedBetweenScoresAndIntelligence(t *testing.T) {
	var count int
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":200,"data":[
		 {"id":"1","slug":"model-a","model_creator":{"slug":"vendor"},
		  "evaluations":{"artificial_analysis_math_index":68.25,"artificial_analysis_intelligence_index":85.0}}
		]}`))
	}))
	p := New(srv.Client(), "key", []string{"vendor/model-a"})
	p.baseURL = srv.URL
	p.attempts = 1

	_, err1 := p.Scores(t.Context())
	if err1 != nil {
		t.Fatalf("Scores() error = %v", err1)
	}
	_, err2 := p.Intelligence(t.Context())
	if err2 != nil {
		t.Fatalf("Intelligence() error = %v", err2)
	}
	if count != 1 {
		t.Errorf("server hit %d times, want 1 (shared fetch)", count)
	}
}

func TestIntelligenceErrorSanitized(t *testing.T) {
	secret := "aa_secret_value_xyz"
	p, _ := providerFor(t, "error with "+secret, http.StatusForbidden, []string{"a/b"})
	p.key = secret
	_, err := p.Intelligence(t.Context())
	if err == nil {
		t.Fatal("Intelligence() error = nil, want non-200 error")
	}
	if contains := strings.Contains(err.Error(), secret); contains {
		t.Errorf("error contains the API key: %q", err.Error())
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

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
