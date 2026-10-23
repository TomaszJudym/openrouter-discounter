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
	return &Client{hc: srv.Client(), key: "test-or-key", base: srv.URL + "/api/v1"}
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
	r.Data.DesignatedVersion.Config.Reasoning.Effort = reasoningEffort
	return r
}

func TestSlugsCoverAllSectors(t *testing.T) {
	for _, sec := range rank.Sectors {
		switch sec {
		case rank.StrongestDiscount:
			if got := Slugs[sec]; got != "strongest-discount" {
				t.Errorf("Slugs[%s] = %q, want strongest-discount", sec, got)
			}
		default:
			if got := Slugs[sec]; !strings.HasPrefix(got, "admech-") && got != "expensive-long" {
				t.Errorf("Slugs[%s] = %q, want valid slug", sec, got)
			}
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
	reas, ok := body["reasoning"].(map[string]any)
	if !ok || reas["effort"] != "max" {
		t.Errorf("reasoning = %v, want {effort: max}", body["reasoning"])
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

func TestUpdatePostsWhenReasoningMissing(t *testing.T) {
	rec := &recorder{}
	cur := matchingVersion()
	cur.Data.DesignatedVersion.Config.Reasoning.Effort = ""
	c := serverFor(t, rec, &cur, http.StatusOK)
	updated, err := c.Update(t.Context(), mathRows())
	if err != nil || !updated {
		t.Fatalf("Update() = %v, %v, want updated (reasoning missing)", updated, err)
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

func strongestDiscountRows() rank.SectorResult {
	return rank.SectorResult{Sector: rank.StrongestDiscount, Rows: []rank.Row{
		{ModelID: "google/gemini-2.5-pro", Score: 89.2, Scored: true},
		{ModelID: "openai/o3", Score: 87.5, Scored: true},
		{ModelID: "anthropic/claude-opus-4", Score: 85.1, Scored: true},
	}}
}

func TestStrongestDiscountPostBody(t *testing.T) {
	rec := &recorder{}
	c := serverFor(t, rec, nil, http.StatusNotFound)
	updated, err := c.Update(t.Context(), strongestDiscountRows())
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
	// system prompt = halflight
	msgs := body["messages"].([]any)
	m0 := msgs[0].(map[string]any)
	if m0["content"] != halflightPrompt {
		t.Errorf("system prompt = %q, want halflight prompt", m0["content"])
	}
	// verbosity = low
	if body["verbosity"] != "low" {
		t.Errorf("verbosity = %v, want low", body["verbosity"])
	}
	// reasoning = {effort: max, enabled: true}
	reas := body["reasoning"].(map[string]any)
	if reas["effort"] != "max" {
		t.Errorf("reasoning.effort = %v, want max", reas["effort"])
	}
	if reas["enabled"] != true {
		t.Errorf("reasoning.enabled = %v, want true", reas["enabled"])
	}
	// sampling params
	if body["temperature"] != 1.15 {
		t.Errorf("temperature = %v, want 1.15", body["temperature"])
	}
	if body["top_p"] != 0.95 {
		t.Errorf("top_p = %v, want 0.95", body["top_p"])
	}
	if body["frequency_penalty"] != 0.4 {
		t.Errorf("frequency_penalty = %v, want 0.4", body["frequency_penalty"])
	}
	if body["presence_penalty"] != 0.4 {
		t.Errorf("presence_penalty = %v, want 0.4", body["presence_penalty"])
	}
}

func TestAdmechPostBodyUnchanged(t *testing.T) {
	rec := &recorder{}
	c := serverFor(t, rec, nil, http.StatusNotFound)
	updated, err := c.Update(t.Context(), mathRows())
	if err != nil {
		t.Fatalf("Update() error = %v, want nil", err)
	}
	if !updated {
		t.Error("Update() = false, want true")
	}
	if len(rec.postBodies) != 1 {
		t.Fatalf("POST count = %d, want 1", len(rec.postBodies))
	}
	body := rec.postBodies[0]
	// no sampling keys
	if _, ok := body["temperature"]; ok {
		t.Error("temperature key present, want absent")
	}
	if _, ok := body["top_p"]; ok {
		t.Error("top_p key present, want absent")
	}
	if _, ok := body["frequency_penalty"]; ok {
		t.Error("frequency_penalty key present, want absent")
	}
	if _, ok := body["presence_penalty"]; ok {
		t.Error("presence_penalty key present, want absent")
	}
	// reasoning = {effort: max} without enabled
	reas := body["reasoning"].(map[string]any)
	if reas["effort"] != "max" {
		t.Errorf("reasoning.effort = %v, want max", reas["effort"])
	}
	if _, ok := reas["enabled"]; ok {
		t.Error("reasoning.enabled key present, want absent")
	}
}

func matchingStrongestDiscountVersion() presetResponse {
	var r presetResponse
	r.Data.DesignatedVersion.SystemPrompt = halflightPrompt
	r.Data.DesignatedVersion.Config.Models = []string{"google/gemini-2.5-pro", "openai/o3", "anthropic/claude-opus-4"}
	r.Data.DesignatedVersion.Config.Verbosity = verbosity
	r.Data.DesignatedVersion.Config.Reasoning.Effort = reasoningEffort
	enabled := true
	r.Data.DesignatedVersion.Config.Reasoning.Enabled = &enabled
	temp := 1.15
	r.Data.DesignatedVersion.Config.Temperature = &temp
	topP := 0.95
	r.Data.DesignatedVersion.Config.TopP = &topP
	freq := 0.4
	r.Data.DesignatedVersion.Config.FrequencyPenalty = &freq
	pres := 0.4
	r.Data.DesignatedVersion.Config.PresencePenalty = &pres
	return r
}

func TestStrongestDiscountSkipsWhenUnchanged(t *testing.T) {
	rec := &recorder{}
	cur := matchingStrongestDiscountVersion()
	c := serverFor(t, rec, &cur, http.StatusOK)
	updated, err := c.Update(t.Context(), strongestDiscountRows())
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

func TestStrongestDiscountPostsWhenTemperatureDiffers(t *testing.T) {
	rec := &recorder{}
	cur := matchingStrongestDiscountVersion()
	temp := 1.0
	cur.Data.DesignatedVersion.Config.Temperature = &temp
	c := serverFor(t, rec, &cur, http.StatusOK)
	updated, err := c.Update(t.Context(), strongestDiscountRows())
	if err != nil || !updated {
		t.Fatalf("Update() = %v, %v, want updated (temperature differs)", updated, err)
	}
}

func TestStrongestDiscountPostsWhenEnabledMissing(t *testing.T) {
	rec := &recorder{}
	cur := matchingStrongestDiscountVersion()
	cur.Data.DesignatedVersion.Config.Reasoning.Enabled = nil
	c := serverFor(t, rec, &cur, http.StatusOK)
	updated, err := c.Update(t.Context(), strongestDiscountRows())
	if err != nil || !updated {
		t.Fatalf("Update() = %v, %v, want updated (enabled missing)", updated, err)
	}
}

func TestStrongestDiscountPostsWhenPromptDiffers(t *testing.T) {
	rec := &recorder{}
	cur := matchingStrongestDiscountVersion()
	cur.Data.DesignatedVersion.SystemPrompt = "wrong prompt"
	c := serverFor(t, rec, &cur, http.StatusOK)
	updated, err := c.Update(t.Context(), strongestDiscountRows())
	if err != nil || !updated {
		t.Fatalf("Update() = %v, %v, want updated (prompt differs)", updated, err)
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
