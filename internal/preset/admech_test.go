package preset

import (
	"slices"
	"strings"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// admechSectors lists the seven admech-code role sectors in ManagedSlugs
// order after the admech-code worker preset.
var admechSectors = []rank.Sector{
	admechOracle, admechReviewer, admechResearcher,
	admechAuditor, admechDelegate, admechScout, admechVision,
}

func TestManagedSlugsExact(t *testing.T) {
	want := []string{
		"admech-code",
		"admech-code-oracle",
		"admech-code-reviewer",
		"admech-code-researcher",
		"admech-code-auditor",
		"admech-code-delegate",
		"admech-code-scout",
		"admech-code-vision",
	}
	if got := ManagedSlugs(); !slices.Equal(got, want) {
		t.Errorf("ManagedSlugs() = %v, want %v", got, want)
	}
}

func TestSlugsMapAdmechRoleSectors(t *testing.T) {
	for _, sec := range admechSectors {
		if got := Slugs[sec]; got != string(sec) {
			t.Errorf("Slugs[%s] = %q, want %q", sec, got, string(sec))
		}
	}
	if got := Slugs[rank.Code]; got != "admech-code" {
		t.Errorf("Slugs[rank.Code] = %q, want admech-code", got)
	}
}

func TestManagedSlugsAllInSlugs(t *testing.T) {
	for _, slug := range ManagedSlugs() {
		found := false
		for _, s := range Slugs {
			if s == slug {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("managed slug %q has no sector in Slugs", slug)
		}
	}
}

func TestUpdateManagedRejectsLegacySlugs(t *testing.T) {
	legacy := []rank.Sector{
		rank.Math,                       // admech-math
		rank.HermesDelegate,             // hermes-delegate
		rank.ExpensiveLong,              // expensive-long
		rank.StrongestDiscountHalflight, // strongest-discount-half-light
	}
	for _, sec := range legacy {
		rec := &recorder{}
		c := serverFor(t, rec, nil, 200)
		updated, err := c.UpdateManaged(t.Context(), rank.SectorResult{Sector: sec, Rows: mathRows().Rows})
		if err == nil {
			t.Errorf("UpdateManaged(%s) error = nil, want unmanaged refusal", sec)
		}
		if updated {
			t.Errorf("UpdateManaged(%s) = true, want false", sec)
		}
		slug := Slugs[sec]
		if !strings.Contains(err.Error(), slug) || !strings.Contains(err.Error(), "not managed") {
			t.Errorf("UpdateManaged(%s) error = %v, want it to name %q as not managed", sec, err, slug)
		}
		if len(rec.getPaths) != 0 || len(rec.postSlugs) != 0 {
			t.Errorf("UpdateManaged(%s) hit the API: GET %v POST %v", sec, rec.getPaths, rec.postSlugs)
		}
	}
}

func TestUpdateManagedUnknownSector(t *testing.T) {
	rec := &recorder{}
	c := serverFor(t, rec, nil, 200)
	if _, err := c.UpdateManaged(t.Context(), rank.SectorResult{Sector: "no-such-sector"}); err == nil {
		t.Error("UpdateManaged(unknown sector) error = nil, want error")
	}
	if len(rec.getPaths) != 0 || len(rec.postSlugs) != 0 {
		t.Error("UpdateManaged(unknown sector) hit the API")
	}
}

func TestUpdateManagedUpdatesOnlyManagedSlugs(t *testing.T) {
	rec := &recorder{}
	c := serverFor(t, rec, nil, 404)
	secs := append([]rank.Sector{rank.Code}, admechSectors...)
	for _, sec := range secs {
		rows := []rank.Row{
			{ModelID: "z-ai/glm-4.7", Score: 95, Scored: true},
			{ModelID: "deepseek/deepseek-v3.1-terminus", Score: 53.7, Scored: true},
			{ModelID: "minimax/minimax-m2", Score: 78.3, Scored: true},
		}
		updated, err := c.UpdateManaged(t.Context(), rank.SectorResult{Sector: sec, Rows: rows})
		if err != nil {
			t.Fatalf("UpdateManaged(%s) error = %v", sec, err)
		}
		if !updated {
			t.Errorf("UpdateManaged(%s) = false, want true", sec)
		}
	}
	if len(rec.postSlugs) != len(secs) {
		t.Fatalf("POST count = %d, want %d", len(rec.postSlugs), len(secs))
	}
	for _, path := range rec.postSlugs {
		slug := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/presets/"), "/chat/completions")
		if !managedSlug(slug) {
			t.Errorf("POSTed unmanaged slug %q", slug)
		}
	}
}

func TestRequestManagedSlugs(t *testing.T) {
	efforts := map[string]string{
		"admech-code":            "max",
		"admech-code-oracle":     "high",
		"admech-code-reviewer":   "high",
		"admech-code-researcher": "medium",
		"admech-code-auditor":    "medium",
		"admech-code-delegate":   "low",
		"admech-code-scout":      "low",
		"admech-code-vision":     "low",
	}
	for slug, wantEffort := range efforts {
		system, params, ok := Request(slug)
		if !ok {
			t.Fatalf("Request(%s) ok = false, want true", slug)
		}
		if system == "" {
			t.Errorf("Request(%s) system prompt empty", slug)
		}
		if params["verbosity"] != "low" {
			t.Errorf("Request(%s) verbosity = %v, want low", slug, params["verbosity"])
		}
		reas, ok := params["reasoning"].(map[string]any)
		if !ok || reas["effort"] != wantEffort {
			t.Errorf("Request(%s) reasoning = %v, want effort %s", slug, params["reasoning"], wantEffort)
		}
		for _, k := range []string{"temperature", "top_p", "max_tokens", "messages", "models", "provider"} {
			if _, present := params[k]; present {
				t.Errorf("Request(%s) has unexpected key %q", slug, k)
			}
		}
	}
}

func TestRequestUnmanagedSlugsStillResolve(t *testing.T) {
	// dbproxy compatibility: pre-existing slugs stay requestable even
	// though the update path refuses them.
	for _, sec := range []rank.Sector{rank.Math, rank.HermesDelegate, rank.ExpensiveLong} {
		system, params, ok := Request(Slugs[sec])
		if !ok {
			t.Fatalf("Request(%s) ok = false", Slugs[sec])
		}
		if len(params) == 0 {
			t.Errorf("Request(%s) params empty", Slugs[sec])
		}
		_ = system
	}
}

func TestAdmechCodeWorkerSpecUnchanged(t *testing.T) {
	if got := Slugs[rank.Code]; got != "admech-code" {
		t.Errorf("Slugs[rank.Code] = %q, want admech-code", got)
	}
	want := presetSpec{systemPrompt: systemPrompt}
	if got := specs[rank.Code]; got != want {
		t.Errorf("specs[rank.Code] = %+v, want %+v", got, want)
	}
	system, params, ok := Request("admech-code")
	if !ok || system != systemPrompt {
		t.Errorf("Request(admech-code) = %q, %v, %v; want embedded prompt", system, params, ok)
	}
	reas := params["reasoning"].(map[string]any)
	if reas["effort"] != "max" {
		t.Errorf("Request(admech-code) effort = %v, want max", reas["effort"])
	}
	if _, present := reas["enabled"]; present {
		t.Error("Request(admech-code) sets reasoning.enabled, want absent")
	}
}

func TestAdmechRoleSpecs(t *testing.T) {
	prompts := map[rank.Sector]string{
		admechOracle:     oraclePrompt,
		admechReviewer:   reviewerPrompt,
		admechResearcher: researcherPrompt,
		admechAuditor:    auditorPrompt,
		admechDelegate:   delegatePrompt,
		admechScout:      scoutPrompt,
		admechVision:     visionPrompt,
	}
	efforts := map[rank.Sector]string{
		admechOracle: "high", admechReviewer: "high",
		admechResearcher: "medium", admechAuditor: "medium",
		admechDelegate: "low", admechScout: "low",
		admechVision: "low",
	}
	seen := map[string]bool{}
	for sec, want := range prompts {
		spec := specs[sec]
		if spec.systemPrompt != want {
			t.Errorf("specs[%s] prompt mismatch", sec)
		}
		if spec.systemPrompt == "" || spec.systemPrompt != strings.TrimSpace(spec.systemPrompt) {
			t.Errorf("specs[%s] prompt empty or untrimmed", sec)
		}
		if spec.effort != efforts[sec] {
			t.Errorf("specs[%s] effort = %q, want %q", sec, spec.effort, efforts[sec])
		}
		if spec.sampling || spec.hermes || spec.reasoningEnabled {
			t.Errorf("specs[%s] = %+v, want admech style (no sampling, no hermes, effort only)", sec, spec)
		}
		if seen[spec.systemPrompt] {
			t.Errorf("specs[%s] prompt duplicates another role's", sec)
		}
		seen[spec.systemPrompt] = true
	}
	if len(seen) != len(admechSectors) {
		t.Errorf("role prompts not all distinct: %d unique", len(seen))
	}
}
