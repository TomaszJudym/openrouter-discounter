package preset

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

func mathRows() rank.SectorResult {
	return rank.SectorResult{Sector: rank.Math, Rows: []rank.Row{
		{ModelID: "z-ai/glm-4.7", Score: 95, Scored: true},
		{ModelID: "deepseek/deepseek-v3.1-terminus", Score: 53.7, Scored: true},
		{ModelID: "deepseek/deepseek-v4-flash", Scored: false},
		{ModelID: "minimax/minimax-m2", Score: 78.3, Scored: true},
	}}
}

type recorder struct {
	getPaths   []string
	postBodies []map[string]any
	postSlugs  []string
}

func serverFor(t *testing.T, rec *recorder, current *presetResponse, getStatus int) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/presets/{slug}", func(w http.ResponseWriter, r *http.Request) {
		rec.getPaths = append(rec.getPaths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer test-or-key" {
			t.Errorf("missing auth header on GET %s", r.URL.Path)
		}
		if getStatus != http.StatusOK {
			w.WriteHeader(getStatus)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"slug":"x","designated_version":` + mustJSON(current.Data.DesignatedVersion) + `}}`))
	})
	mux.HandleFunc("POST /api/v1/presets/{slug}/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		rec.postSlugs = append(rec.postSlugs, r.URL.Path)
		var body map[string]any
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		rec.postBodies = append(rec.postBodies, body)
		if r.Header.Get("Authorization") != "Bearer test-or-key" {
			t.Errorf("missing auth header on POST %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"slug":"x","designated_version":{"version":9}}}`))
	})
	srv := httptest.NewTestServer(t, mux)
	_ = srv.Client()
	return &Client{hc: srv.Client(), key: "test-or-key", Base: srv.URL + "/api/v1"}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func matchingVersion() presetResponse {
	var r presetResponse
	r.Data.DesignatedVersion.SystemPrompt = systemPrompt
	r.Data.DesignatedVersion.Config.Models = []string{"z-ai/glm-4.7", "deepseek/deepseek-v3.1-terminus", "minimax/minimax-m2"}
	r.Data.DesignatedVersion.Config.Verbosity = verbosity
	return r
}

func TestSlugsCoverAllSectors(t *testing.T) {
	for _, sec := range rank.Sectors {
		if got := Slugs[sec]; !strings.HasPrefix(got, "admech-") {
			t.Errorf("Slugs[%s] = %q, want an admech-* slug", sec, got)
		}
	}
}

func TestUpdatePostsTop3Scored(t *testing.T) {
	rec := &recorder{}
	c := serverFor(t, rec, nil, http.StatusNotFound)
	updated, err := c.Update(t.Context(), mathRows())
	if err != nil {
		t.Fatalf("Update() error = %v, want nil", err)
	}
	if !updated {
		t.Error("Update() = false, want true (preset missing)")
	}
	if len(rec.postBodies) != 1 {
		t.Fatalf("POST count = %d, want 1", len(rec.postBodies))
	}
	body := rec.postBodies[0]
	if body["verbosity"] != "low" {
		t.Errorf("verbosity = %v, want low", body["verbosity"])
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v, want one system message", body["messages"])
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != systemPrompt {
		t.Errorf("system message = %v, want embedded prompt", m0)
	}
	got := fmtModels(body["models"])
	want := []string{"z-ai/glm-4.7", "deepseek/deepseek-v3.1-terminus", "minimax/minimax-m2"}
	if !slices.Equal(got, want) {
		t.Errorf("models = %v, want %v (unscored row skipped)", got, want)
	}
	if len(rec.postSlugs) != 1 || !strings.HasSuffix(rec.postSlugs[0], "/presets/admech-math/chat/completions") {
		t.Errorf("POST path = %v, want admech-math", rec.postSlugs)
	}
}

func TestUpdateSkipsWhenUnchanged(t *testing.T) {
	rec := &recorder{}
	cur := matchingVersion()
	c := serverFor(t, rec, &cur, http.StatusOK)
	updated, err := c.Update(t.Context(), mathRows())
	if err != nil {
		t.Fatalf("Update() error = %v, want nil", err)
	}
	if updated {
		t.Error("Update() = true, want false (nothing changed)")
	}
	if len(rec.postBodies) != 0 {
		t.Errorf("POST fired %d times on unchanged preset", len(rec.postBodies))
	}
}

func TestUpdatePostsWhenSystemPromptDiffers(t *testing.T) {
	rec := &recorder{}
	cur := matchingVersion()
	cur.Data.DesignatedVersion.SystemPrompt = "something else"
	c := serverFor(t, rec, &cur, http.StatusOK)
	updated, err := c.Update(t.Context(), mathRows())
	if err != nil || !updated {
		t.Fatalf("Update() = %v, %v, want updated", updated, err)
	}
}

func TestUpdatePostsWhenVerbosityMissing(t *testing.T) {
	rec := &recorder{}
	cur := matchingVersion()
	cur.Data.DesignatedVersion.Config.Verbosity = ""
	c := serverFor(t, rec, &cur, http.StatusOK)
	updated, err := c.Update(t.Context(), mathRows())
	if err != nil || !updated {
		t.Fatalf("Update() = %v, %v, want updated (verbosity missing)", updated, err)
	}
}

func TestUpdateNoScoredRows(t *testing.T) {
	rec := &recorder{}
	c := serverFor(t, rec, nil, http.StatusNotFound)
	sr := rank.SectorResult{Sector: rank.Finance, Rows: []rank.Row{{ModelID: "a/b"}}}
	updated, err := c.Update(t.Context(), sr)
	if err != nil || updated {
		t.Fatalf("Update() = %v, %v, want false, nil (no scored rows)", updated, err)
	}
	if len(rec.getPaths) != 0 {
		t.Error("GET fired despite no scored rows")
	}
}

func TestUpdateAPIErrorIsReturned(t *testing.T) {
	rec := &recorder{}
	c := serverFor(t, rec, nil, http.StatusBadRequest)
	_, err := c.Update(t.Context(), mathRows())
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("Update() error = %v, want 400 error", err)
	}
}

func TestSystemPromptEmbedded(t *testing.T) {
	if !strings.Contains(systemPrompt, "Shortest read time is the primary objective.") {
		t.Error("embedded system prompt missing its first rule")
	}
	if strings.ContainsAny(systemPrompt, "\r") {
		t.Error("embedded system prompt contains carriage returns")
	}
}

func fmtModels(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		out = append(out, e.(string))
	}
	return out
}
