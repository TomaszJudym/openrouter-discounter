package darkbloom

import (
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
)

// The fixtures are trimmed live responses (2026-11-27): models.json is
// Darkbloom's /v1/models plus one model no Mac can take; catalog.json the
// OpenRouter catalog entries near them, variants and look-alikes included.

const testKey = "sk-db-test-secret"

func serve(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/models.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer "+testKey {
			// The 401 body echoes the key first, so the sanitized error must
			// show REDACTED — a plain truncation would still pass the
			// key-absent assertion
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"` + testKey + ` rejected"}`))
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func catalog(t *testing.T) []discounts.Model {
	t.Helper()
	body, err := os.ReadFile("testdata/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Data []discounts.Model `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r.Data
}

func TestFetchKeepsRoutable(t *testing.T) {
	srv := serve(t)
	models, err := Fetch(t.Context(), fetch.Getter{HC: srv.Client(), Attempts: 1}, srv.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 13 {
		t.Fatalf("got %d models, want the 13 routable", len(models))
	}
	if slices.ContainsFunc(models, func(m Model) bool { return m.ID == "qwen3-vl-30b-a3b-instruct" }) {
		t.Error("unroutable model kept")
	}
	var oss Model
	for _, m := range models {
		if m.ID == "gpt-oss-20b" {
			oss = m
		}
	}
	if oss.SupportedFeatures != nil || oss.Pricing.Prompt != "0.0000000153" || oss.ContextLength != 131072 {
		t.Errorf("gpt-oss-20b parsed as %+v", oss)
	}
}

func TestFetchErrorRedactsKey(t *testing.T) {
	srv := serve(t)
	_, err := Fetch(t.Context(), fetch.Getter{HC: srv.Client(), Attempts: 1}, srv.URL+"/x", testKey)
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("err = %v, want an error without the key", err)
	}
	if _, err := Fetch(t.Context(), fetch.Getter{HC: srv.Client()}, srv.URL, ""); err == nil {
		t.Error("no key: want an error")
	}
}

// TestResolveLive maps every live Darkbloom model onto the OpenRouter model
// whose darkbloom/* endpoint OpenRouter lists for it.
func TestResolveLive(t *testing.T) {
	srv := serve(t)
	models, err := Fetch(t.Context(), fetch.Getter{HC: srv.Client(), Attempts: 1}, srv.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	direct, unmapped := Resolve(models, catalog(t))
	if len(unmapped) > 0 {
		t.Errorf("unmapped: %v", unmapped)
	}
	got := make(map[string]string, len(direct))
	for _, d := range direct {
		got[d.ID] = d.BaseID
	}
	want := map[string]string{
		"gemma-4-26b":                            "google/gemma-4-26b-a4b-it",
		"nvidia-nemotron-3.5-super":              "nvidia/nemotron-3.5-super-vl",
		"glm-5.3-flash":                          "z-ai/glm-5.3-flash",
		"Qwen3.5-9B":                             "qwen/qwen3.5-9b",
		"qwen3.6-35b-a3b-vl-mtp-mxfp8":           "qwen/qwen3.6-35b-a3b",
		"EigenLabs/Qwen3.8-27B-4bit-mtp":         "qwen/qwen3.8-27b",
		"thinkingmachines/Inkling-2-Mini":        "thinkingmachines/inkling-2-mini",
		"ternary-bonsai-2-27b":                   "prism-ml/ternary-bonsai-2-27b",
		"gpt-oss-20b":                            "openai/gpt-oss-20b",
		"nvidia-nemotron-3.5-lightning":          "nvidia/nemotron-3.5-lightning",
		"qwen3.5-35b-a3b":                        "qwen/qwen3.5-35b-a3b",
		"EigenLabs/Ternary-Bonsai-2-9B-MLX-2bit": "prism-ml/ternary-bonsai-2-9b",
		"mimo-v2.6-flash-mopd":                   "xiaomi/mimo-v2.6-flash",
	}
	if !maps.Equal(got, want) {
		for id, w := range want {
			if got[id] != w {
				t.Errorf("%s → %q, want %q", id, got[id], w)
			}
		}
	}
	for _, d := range direct {
		if !d.Tools {
			t.Errorf("%s: tools false; every live model calls tools", d.ID)
		}
	}
}

func TestToolsWithoutFeaturesFollowsBase(t *testing.T) {
	m := Model{ID: "x"}
	if m.Tools(discounts.Model{SupportedParameters: []string{"tools"}}) != true {
		t.Error("nil features, base calls tools: want true")
	}
	if m.Tools(discounts.Model{}) {
		t.Error("nil features, base without tools: want false")
	}
	m.SupportedFeatures = []string{"reasoning"}
	if m.Tools(discounts.Model{SupportedParameters: []string{"tools"}}) {
		t.Error("features listed without tools: want false")
	}
}

func TestKeyDropsBuildTokens(t *testing.T) {
	for in, want := range map[string]string{
		"EigenLabs/Qwen3.8-27B-4bit-mtp":                    "qwen3827b",
		"prism-ml/Ternary-Bonsai-2-9B-mlx-2bit":             "ternarybonsai29b",
		"gemma-4-26b-qat-4bit":                              "gemma426b",
		"qwen3.6-35b-a3b-vl-mtp-mxfp8":                      "qwen3635ba3bvl",
		"nvidia/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-BF16": "nvidianemotron35lightning30ba3b",
	} {
		if got := key(in); got != want {
			t.Errorf("key(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindRejectsAmbiguousPrefix(t *testing.T) {
	idx := newIndex([]discounts.Model{{ID: "v/model-a-1"}, {ID: "v/model-a-2"}, {ID: "v/model-a-2:free"}})
	if id, ok := idx.find("model-a"); ok {
		t.Errorf("ambiguous prefix matched %s", id)
	}
	if id, ok := idx.find("model-a-2-4bit"); !ok || id != "v/model-a-2" {
		t.Errorf("find = %q %v, want v/model-a-2", id, ok)
	}
}
