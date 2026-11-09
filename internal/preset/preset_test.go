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
		case rank.StrongestDiscountHalflight:
			if got := Slugs[sec]; got != "strongest-discount-half-light" {
				t.Errorf("Slugs[%s] = %q, want strongest-discount-half-light", sec, got)
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
	return rank.SectorResult{Sector: rank.StrongestDiscountHalflight, Rows: []rank.Row{
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
	// reasoning = {effort: high, enabled: true}
	reas := body["reasoning"].(map[string]any)
	if reas["effort"] != "high" {
		t.Errorf("reasoning.effort = %v, want high", reas["effort"])
	}
	if reas["enabled"] != true {
		t.Errorf("reasoning.enabled = %v, want true", reas["enabled"])
	}
	// sampling params
	if body["temperature"] != 1.0 {
		t.Errorf("temperature = %v, want 1.0", body["temperature"])
	}
	if body["top_p"] != 0.95 {
		t.Errorf("top_p = %v, want 0.95", body["top_p"])
	}
	if body["frequency_penalty"] != 0.0 {
		t.Errorf("frequency_penalty = %v, want 0", body["frequency_penalty"])
	}
	if body["presence_penalty"] != 0.1 {
		t.Errorf("presence_penalty = %v, want 0.1", body["presence_penalty"])
	}
	if body["min_p"] != 0.05 {
		t.Errorf("min_p = %v, want 0.05", body["min_p"])
	}
	if body["max_tokens"] != 16384.0 {
		t.Errorf("max_tokens = %v, want 16384", body["max_tokens"])
	}
}

func strongestDiscountVolitionRows() rank.SectorResult {
	return rank.SectorResult{Sector: rank.StrongestDiscountVolition, Rows: []rank.Row{
		{ModelID: "google/gemini-2.5-pro", Score: 89.2, Scored: true},
		{ModelID: "openai/o3", Score: 87.5, Scored: true},
		{ModelID: "anthropic/claude-opus-4", Score: 85.1, Scored: true},
	}}
}

func TestStrongestDiscountVolitionPostBody(t *testing.T) {
	rec := &recorder{}
	c := serverFor(t, rec, nil, http.StatusNotFound)
	updated, err := c.Update(t.Context(), strongestDiscountVolitionRows())
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
	// system prompt = volition
	msgs := body["messages"].([]any)
	m0 := msgs[0].(map[string]any)
	if m0["content"] != volitionPrompt {
		t.Errorf("system prompt = %q, want volition prompt", m0["content"])
	}
	// reasoning = {effort: high, enabled: true}
	reas := body["reasoning"].(map[string]any)
	if reas["effort"] != "high" {
		t.Errorf("reasoning.effort = %v, want high", reas["effort"])
	}
	if reas["enabled"] != true {
		t.Errorf("reasoning.enabled = %v, want true", reas["enabled"])
	}
	// sampling params (same config as halflight)
	if body["temperature"] != 1.0 {
		t.Errorf("temperature = %v, want 1.0", body["temperature"])
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
	for _, k := range []string{"presence_penalty", "min_p", "max_tokens"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s key present, want absent", k)
		}
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
	r.Data.DesignatedVersion.Config.Reasoning.Effort = "high"
	enabled := true
	r.Data.DesignatedVersion.Config.Reasoning.Enabled = &enabled
	temp := 1.0
	r.Data.DesignatedVersion.Config.Temperature = &temp
	topP := 0.95
	r.Data.DesignatedVersion.Config.TopP = &topP
	freq := 0.0
	r.Data.DesignatedVersion.Config.FrequencyPenalty = &freq
	pres := 0.1
	r.Data.DesignatedVersion.Config.PresencePenalty = &pres
	minP := 0.05
	r.Data.DesignatedVersion.Config.MinP = &minP
	maxTok := 16384
	r.Data.DesignatedVersion.Config.MaxTokens = &maxTok
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
	temp := 1.15
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

func TestHalflightPromptEmbedded(t *testing.T) {
	if !strings.Contains(halflightPrompt, "fight-or-flight response") {
		t.Error("halflight prompt missing its persona description")
	}
}

func TestVolitionPromptEmbedded(t *testing.T) {
	if !strings.Contains(volitionPrompt, "willpower, resolve") {
		t.Error("volition prompt missing its persona description")
	}
}

func TestInlandEmpirePromptEmbedded(t *testing.T) {
	if !strings.Contains(inlandempirePrompt, "unfiltered wellspring") {
		t.Error("inlandempire prompt missing its persona description")
	}
}

func TestShiversPromptEmbedded(t *testing.T) {
	if !strings.Contains(shiversPrompt, "genius loci") {
		t.Error("shivers prompt missing its persona description")
	}
}

func strongestDiscountInlandEmpireRows() rank.SectorResult {
	return rank.SectorResult{Sector: rank.StrongestDiscountInlandEmpire, Rows: []rank.Row{
		{ModelID: "google/gemini-2.5-pro", Score: 89.2, Scored: true},
		{ModelID: "openai/o3", Score: 87.5, Scored: true},
		{ModelID: "anthropic/claude-opus-4", Score: 85.1, Scored: true},
	}}
}

func TestStrongestDiscountInlandEmpirePostBody(t *testing.T) {
	rec := &recorder{}
	c := serverFor(t, rec, nil, http.StatusNotFound)
	updated, err := c.Update(t.Context(), strongestDiscountInlandEmpireRows())
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
	msgs := body["messages"].([]any)
	m0 := msgs[0].(map[string]any)
	if m0["content"] != inlandempirePrompt {
		t.Errorf("system prompt = %q, want inlandempire prompt", m0["content"])
	}
	reas := body["reasoning"].(map[string]any)
	if reas["effort"] != "high" {
		t.Errorf("reasoning.effort = %v, want high", reas["effort"])
	}
	if reas["enabled"] != true {
		t.Errorf("reasoning.enabled = %v, want true", reas["enabled"])
	}
	if body["temperature"] != 1.0 {
		t.Errorf("temperature = %v, want 1.0", body["temperature"])
	}
}

func strongestDiscountShiversRows() rank.SectorResult {
	return rank.SectorResult{Sector: rank.StrongestDiscountShivers, Rows: []rank.Row{
		{ModelID: "google/gemini-2.5-pro", Score: 89.2, Scored: true},
		{ModelID: "openai/o3", Score: 87.5, Scored: true},
		{ModelID: "anthropic/claude-opus-4", Score: 85.1, Scored: true},
	}}
}

func TestStrongestDiscountShiversPostBody(t *testing.T) {
	rec := &recorder{}
	c := serverFor(t, rec, nil, http.StatusNotFound)
	updated, err := c.Update(t.Context(), strongestDiscountShiversRows())
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
	msgs := body["messages"].([]any)
	m0 := msgs[0].(map[string]any)
	if m0["content"] != shiversPrompt {
		t.Errorf("system prompt = %q, want shivers prompt", m0["content"])
	}
	reas := body["reasoning"].(map[string]any)
	if reas["effort"] != "high" {
		t.Errorf("reasoning.effort = %v, want high", reas["effort"])
	}
	if reas["enabled"] != true {
		t.Errorf("reasoning.enabled = %v, want true", reas["enabled"])
	}
	if body["temperature"] != 1.0 {
		t.Errorf("temperature = %v, want 1.0", body["temperature"])
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

func hermesRows() rank.SectorResult {
	return rank.SectorResult{Sector: rank.HermesDelegate, Rows: []rank.Row{
		{ModelID: "a/one", Score: 100, Scored: true},
		{ModelID: "b/two", Score: 60, Scored: true},
		{ModelID: "c/three", Score: 10, Scored: true},
	}}
}

func TestHermesSlugsAndSpecs(t *testing.T) {
	for _, sec := range []rank.Sector{rank.HermesDelegate, rank.HermesVision, rank.HermesCompress, rank.HermesReview, rank.HermesLight} {
		if got := Slugs[sec]; got != string(sec) {
			t.Errorf("Slugs[%s] = %q, want %q", sec, got, sec)
		}
		spec, ok := specs[sec]
		if !ok || !spec.hermes || spec.effort == "" || spec.sampling {
			t.Errorf("specs[%s] = %+v, want hermes with an effort, no sampling", sec, spec)
		}
		if spec.systemPrompt != "" {
			t.Errorf("specs[%s].systemPrompt = %.40q, want empty", sec, spec.systemPrompt)
		}
	}
}

func postHermes(t *testing.T, sr rank.SectorResult) map[string]any {
	t.Helper()
	rec := &recorder{}
	c := serverFor(t, rec, nil, http.StatusNotFound)
	if _, err := c.Update(t.Context(), sr); err != nil {
		t.Fatalf("Update() error = %v, want nil", err)
	}
	if len(rec.postBodies) != 1 {
		t.Fatalf("POST count = %d, want 1", len(rec.postBodies))
	}
	body := rec.postBodies[0]
	for _, k := range []string{"verbosity", "temperature"} {
		if _, ok := body[k]; ok {
			t.Errorf("body[%q] present, want absent", k)
		}
	}
	if got := fmtModels(body["models"]); !slices.Equal(got, []string{"a/one", "b/two", "c/three"}) {
		t.Errorf("models = %v, want top 3", got)
	}
	return body
}

func TestHermesTaskPresetsPostNoPrompt(t *testing.T) {
	sr := hermesRows()
	sr.Sector = rank.HermesCompress
	body := postHermes(t, sr)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("body[messages] = %v, want one transient user message, no system prompt", body["messages"])
	}
	if eff := body["reasoning"].(map[string]any)["effort"]; eff != "low" {
		t.Errorf("reasoning.effort = %v, want low", eff)
	}
}

func hermesVersion(prompt string) *presetResponse {
	var cur presetResponse
	cur.Data.DesignatedVersion.SystemPrompt = prompt
	cur.Data.DesignatedVersion.Config.Models = []string{"a/one", "b/two", "c/three"}
	cur.Data.DesignatedVersion.Config.Reasoning.Effort = "medium"
	return &cur
}

func TestHermesSkipsWhenUnchanged(t *testing.T) {
	tests := []struct {
		name    string
		prompt  string
		updated bool
	}{
		{"no prompt kept", "", false},
		{"stale prompt", shiversPrompt, true},
	}
	for _, tt := range tests {
		rec := &recorder{}
		c := serverFor(t, rec, hermesVersion(tt.prompt), http.StatusOK)
		updated, err := c.Update(t.Context(), hermesRows())
		if err != nil || updated != tt.updated {
			t.Errorf("%s: Update() = %v, %v; want %v, nil", tt.name, updated, err, tt.updated)
		}
	}
}

func TestStrongestDiscountSkipsWhenZeroPenaltyAbsent(t *testing.T) {
	rec := &recorder{}
	cur := matchingStrongestDiscountVersion()
	cur.Data.DesignatedVersion.Config.FrequencyPenalty = nil
	c := serverFor(t, rec, &cur, http.StatusOK)
	updated, err := c.Update(t.Context(), strongestDiscountRows())
	if err != nil || updated {
		t.Fatalf("Update() = %v, %v, want false (absent penalty means 0)", updated, err)
	}
}
