package piconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/pipeline"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// sector builds one ranked sector with a single winner row.
func sector(name string, price float64) rank.SectorResult {
	return rank.SectorResult{
		Sector: rank.Sector(name),
		Rows:   []rank.Row{{ModelID: "vendor/model-" + name, Endpoint: "prov/tag", Price: price, Was: price * 2, Pct: -50}},
	}
}

// fixture is a pipeline.Result with an OpenRouter code row at orPrice and,
// when dbPrice is set, a Darkbloom-direct code row at dbPrice. The code
// sector is the admech-code preset on both sides (preset.Slugs).
func fixture(orPrice, dbPrice float64) pipeline.Result {
	res := pipeline.Result{OpenRouter: []rank.SectorResult{sector("code", orPrice)}}
	if dbPrice >= 0 {
		res.Direct = []rank.SectorResult{sector("code", dbPrice)}
	}
	return res
}

// writeJSON writes path with content, 0600.
func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: t.TempDir paths built by the test itself
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// modelIDs lists a provider's models.json entry ids.
func modelIDs(t *testing.T, obj map[string]any, provider string) []string {
	t.Helper()
	prov, ok := obj["providers"].(map[string]any)[provider].(map[string]any)
	if !ok {
		return nil
	}
	models, _ := prov["models"].([]any)
	out := make([]string, 0, len(models))
	for _, m := range models {
		if em, ok := m.(map[string]any); ok {
			if id, _ := em["id"].(string); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// TestMergePreservesUserKeys checks that one Emit leaves every pre-existing
// user key — unrelated models.json entries, settings keys and the thinking
// levels inside override entries — exactly as it found them.
func TestMergePreservesUserKeys(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, modelsFile), `{
	  "providers": {
	    "openrouter": {
	      "baseUrl": "https://openrouter.ai/api/v1",
	      "apiKey": "$OPENROUTER_API_KEY",
	      "api": "openai-completions",
	      "models": [
	        {"id": "user/favourite", "contextWindow": 400000},
	        {"id": "@preset/admech-code", "contextWindow": 262144, "managedBy": "openrouter-discounter"}
	      ]
	    }
	  }
	}`)
	writeJSON(t, filepath.Join(dir, settingsFile), `{
	  "theme": "dark",
	  "packages": ["npm:pi-subagents"],
	  "subagents": {
	    "defaultModel": "openrouter/x",
	    "agentOverrides": {
	      "worker": {"model": "openrouter/old", "thinking": "high"},
	      "scout": {"model": "openrouter/y", "thinking": "low"}
	    }
	  }
	}`)

	if _, err := Emit(dir, fixture(0.4, -1)); err != nil {
		t.Fatal(err)
	}

	models := readConfig(t, filepath.Join(dir, modelsFile))
	if ids := modelIDs(t, models, "openrouter"); len(ids) != 2 || ids[0] != "user/favourite" {
		t.Fatalf("user model entry lost or reordered wrongly: %v", ids)
	}
	prov := models["providers"].(map[string]any)["openrouter"].(map[string]any)
	if got, _ := prov["apiKey"].(string); got != "$OPENROUTER_API_KEY" {
		t.Fatalf("openrouter apiKey clobbered: %q", got)
	}

	settings := readConfig(t, filepath.Join(dir, settingsFile))
	if got, _ := settings["theme"].(string); got != "dark" {
		t.Fatalf("theme key lost: %v", settings)
	}
	if pkgs := settings["packages"].([]any); len(pkgs) != 1 || pkgs[0] != "npm:pi-subagents" {
		t.Fatalf("packages key lost: %v", settings["packages"])
	}
	sub := settings["subagents"].(map[string]any)
	if got, _ := sub["defaultModel"].(string); got != "openrouter/x" {
		t.Fatalf("subagents.defaultModel clobbered: %q", got)
	}
	worker := sub["agentOverrides"].(map[string]any)["worker"].(map[string]any)
	if got, _ := worker["model"].(string); got != "openrouter/@preset/admech-code" {
		t.Fatalf("worker model not updated: %q", got)
	}
	if got, _ := worker["thinking"].(string); got != "high" {
		t.Fatalf("worker thinking clobbered: %q", got)
	}
	scout := sub["agentOverrides"].(map[string]any)["scout"].(map[string]any)
	if got, _ := scout["model"].(string); got != "openrouter/y" {
		t.Fatalf("unmanaged agent override clobbered: %q", got)
	}
}

// TestSurfaceRuleDarkbloomCheaper checks db wins when its row is no dearer
// (ties included): the override uses dbproxy/ and the cost comes from the
// direct row.
func TestSurfaceRuleDarkbloomCheaper(t *testing.T) {
	for _, tc := range []struct {
		name             string
		orPrice, dbPrice float64
	}{
		{"db cheaper", 0.5, 0.3},
		{"tie", 0.5, 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := Emit(dir, fixture(tc.orPrice, tc.dbPrice)); err != nil {
				t.Fatal(err)
			}
			settings := readConfig(t, filepath.Join(dir, settingsFile))
			got := settings["subagents"].(map[string]any)["agentOverrides"].(map[string]any)["worker"].(map[string]any)["model"]
			if want := "dbproxy/@preset/admech-code"; got != want {
				t.Fatalf("worker override = %q, want %q", got, want)
			}
			models := readConfig(t, filepath.Join(dir, modelsFile))
			entry := findEntry(t, models, "openrouter", "@preset/admech-code")
			cost, _ := entry["cost"].(map[string]any)
			if got, want := cost["input"].(float64), tc.dbPrice; got != want {
				t.Fatalf("cost.input = %v, want the direct row's %v", got, want)
			}
			if ids := modelIDs(t, models, "dbproxy"); !contains(ids, "@preset/admech-code") {
				t.Fatalf("dbproxy provider missing the preset id: %v", ids)
			}
		})
	}
}

// TestSurfaceRuleOpenRouterCheaper checks openrouter wins when its row is
// strictly cheaper: the override uses openrouter/ and the cost comes from
// the OpenRouter row.
func TestSurfaceRuleOpenRouterCheaper(t *testing.T) {
	dir := t.TempDir()
	if _, err := Emit(dir, fixture(0.3, 0.5)); err != nil {
		t.Fatal(err)
	}
	settings := readConfig(t, filepath.Join(dir, settingsFile))
	got := settings["subagents"].(map[string]any)["agentOverrides"].(map[string]any)["worker"].(map[string]any)["model"]
	if want := "openrouter/@preset/admech-code"; got != want {
		t.Fatalf("worker override = %q, want %q", got, want)
	}
	models := readConfig(t, filepath.Join(dir, modelsFile))
	entry := findEntry(t, models, "openrouter", "@preset/admech-code")
	cost, _ := entry["cost"].(map[string]any)
	if got, want := cost["input"].(float64), 0.3; got != want {
		t.Fatalf("cost.input = %v, want the OpenRouter row's %v", got, want)
	}
}

// TestSurfaceRuleNoDarkbloomRow checks openrouter wins when the direct side
// has no row for the sector (no key, failed fetch, sector absent).
func TestSurfaceRuleNoDarkbloomRow(t *testing.T) {
	dir := t.TempDir()
	if _, err := Emit(dir, fixture(0.4, -1)); err != nil {
		t.Fatal(err)
	}
	settings := readConfig(t, filepath.Join(dir, settingsFile))
	got := settings["subagents"].(map[string]any)["agentOverrides"].(map[string]any)["worker"].(map[string]any)["model"]
	if want := "openrouter/@preset/admech-code"; got != want {
		t.Fatalf("worker override = %q, want %q", got, want)
	}
}

// TestNoPricedRowSkipsSlug checks a slug with rows on neither side gets no
// entry and no override: no broken references, no invented costs.
func TestNoPricedRowSkipsSlug(t *testing.T) {
	dir := t.TempDir()
	// only the code sector has rows; the six admech-code-* slugs have none
	res := pipeline.Result{OpenRouter: []rank.SectorResult{sector("code", 0.4)}}
	if _, err := Emit(dir, res); err != nil {
		t.Fatal(err)
	}
	models := readConfig(t, filepath.Join(dir, modelsFile))
	if ids := modelIDs(t, models, "openrouter"); contains(ids, "@preset/admech-code-oracle") {
		t.Fatalf("rowless slug emitted anyway: %v", ids)
	}
	if ids := modelIDs(t, models, "openrouter"); !contains(ids, "@preset/admech-code") {
		t.Fatalf("priced slug missing: %v", ids)
	}
	settings := readConfig(t, filepath.Join(dir, settingsFile))
	over := settings["subagents"].(map[string]any)["agentOverrides"].(map[string]any)
	if _, ok := over["oracle"]; ok {
		t.Fatalf("override for a rowless slug emitted: %v", over["oracle"])
	}
	if got := over["worker"].(map[string]any)["model"]; got != "openrouter/@preset/admech-code" {
		t.Fatalf("worker override = %v", got)
	}
}

// TestGeneratedEntryShape checks the managed marker, the oracle's larger
// context window, and reasoning on the thinking roles.
func TestGeneratedEntryShape(t *testing.T) {
	dir := t.TempDir()
	res := pipeline.Result{
		OpenRouter: []rank.SectorResult{
			sector("code", 0.4),
			sector("admech-code-oracle", 0.9),
			sector("admech-code-reviewer", 0.8),
			sector("admech-code-researcher", 0.7),
			sector("admech-code-auditor", 0.6),
			sector("admech-code-delegate", 0.5),
			sector("admech-code-scout", 0.2),
		},
	}
	if _, err := Emit(dir, res); err != nil {
		t.Fatal(err)
	}
	models := readConfig(t, filepath.Join(dir, modelsFile))
	checkShape(t, models)
	checkDBProxyProvider(t, models)
	checkNoSecrets(t, dir)
}

// checkShape asserts the openrouter entries' shape.
func checkShape(t *testing.T, models map[string]any) {
	t.Helper()
	oracle := findEntry(t, models, "openrouter", "@preset/admech-code-oracle")
	if got, _ := oracle["contextWindow"].(float64); got != 1050000 {
		t.Fatalf("oracle contextWindow = %v", got)
	}
	if got, _ := oracle["reasoning"].(bool); !got {
		t.Fatal("oracle entry missing reasoning: true")
	}
	worker := findEntry(t, models, "openrouter", "@preset/admech-code")
	if got, _ := worker["contextWindow"].(float64); got != 262144 {
		t.Fatalf("worker contextWindow = %v", got)
	}
	if _, ok := worker["reasoning"]; ok {
		t.Fatal("worker entry must not claim reasoning")
	}
	for _, slug := range preset.ManagedSlugs() {
		entry := findEntry(t, models, "openrouter", "@preset/"+slug)
		if got, _ := entry["managedBy"].(string); got != managedBy {
			t.Fatalf("%s entry missing managedBy marker: %v", slug, entry)
		}
	}
}

// checkDBProxyProvider asserts the loopback provider block and its ids.
func checkDBProxyProvider(t *testing.T, models map[string]any) {
	t.Helper()
	for _, slug := range preset.ManagedSlugs() {
		if ids := modelIDs(t, models, "dbproxy"); !contains(ids, "@preset/"+slug) {
			t.Fatalf("dbproxy provider missing @preset/%s: %v", slug, ids)
		}
	}
	dbp := models["providers"].(map[string]any)[dbproxyProvider].(map[string]any)
	if got, _ := dbp["baseUrl"].(string); got != "http://127.0.0.1:8787/v1" {
		t.Fatalf("dbproxy baseUrl = %q", got)
	}
	if got, _ := dbp["apiKey"].(string); got != "$OPENROUTER_API_KEY" {
		t.Fatalf("dbproxy apiKey must be the $NAME interpolation, got %q", got)
	}
}

// checkNoSecrets asserts neither written file carries a literal key value.
func checkNoSecrets(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{modelsFile, settingsFile} {
		raw, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: t.TempDir paths built by the test itself
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "sk-or-") || strings.Contains(string(raw), "db-") {
			t.Fatalf("%s carries a literal secret: %s", name, raw)
		}
	}
}

// TestUnchangedContentNoWrite checks a second run over an already-written
// dir rewrites nothing: Changed is empty and mtimes are untouched.
func TestUnchangedContentNoWrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := Emit(dir, fixture(0.4, 0.3)); err != nil {
		t.Fatal(err)
	}
	before := mtimes(t, dir)
	out, err := Emit(dir, fixture(0.4, 0.3))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Changed) != 0 {
		t.Fatalf("unchanged content reported changed files: %v", out.Changed)
	}
	if got := mtimes(t, dir); !equalTimes(before, got) {
		t.Fatal("files were rewritten despite unchanged content")
	}
}

// TestAtomicWriteLeavesNoTempFiles checks Emit never leaves a temp file
// behind and the written files are owner-only (0600).
func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := Emit(dir, fixture(0.4, 0.3)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pipresets-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	for _, name := range []string{modelsFile, settingsFile} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", name, fi.Mode().Perm())
		}
	}
}

// TestDryRunWritesNothing checks --dry-run reports the files it would write
// without touching the directory.
func TestDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	out, err := DryRun(dir, fixture(0.4, 0.3))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out.Changed, modelsFile) || !contains(out.Changed, settingsFile) {
		t.Fatalf("dry-run should report both files as would-change: %v", out.Changed)
	}
	if _, err := os.Stat(filepath.Join(dir, modelsFile)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote models.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, settingsFile)); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote settings.json: %v", err)
	}
}

// TestDryRunThenEmitMatches checks the dry-run projection is exact: the
// subsequent real emit changes exactly the files the dry-run named.
func TestDryRunThenEmitMatches(t *testing.T) {
	dir := t.TempDir()
	projected, err := DryRun(dir, fixture(0.4, 0.3))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Emit(dir, fixture(0.4, 0.3))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(projected.Changed, ",") != strings.Join(out.Changed, ",") {
		t.Fatalf("dry-run projected %v, emit changed %v", projected.Changed, out.Changed)
	}
}

// TestOverridesMatchesSettings checks Overrides() agrees with the
// settings.json the emit wrote.
func TestOverridesMatchesSettings(t *testing.T) {
	dir := t.TempDir()
	res := fixture(0.5, 0.3)
	if _, err := Emit(dir, res); err != nil {
		t.Fatal(err)
	}
	want := Overrides(res)
	settings := readConfig(t, filepath.Join(dir, settingsFile))
	over := settings["subagents"].(map[string]any)["agentOverrides"].(map[string]any)
	for agent, model := range want {
		got := over[agent].(map[string]any)["model"]
		if got != model {
			t.Fatalf("%s: settings.json has %q, Overrides says %q", agent, got, model)
		}
	}
	if len(want) != 1 {
		t.Fatalf("fixture should select one agent, got %v", want)
	}
}

// findEntry is the "@preset/<id>" entry under provider's models.
func findEntry(t *testing.T, obj map[string]any, provider, id string) map[string]any {
	t.Helper()
	for _, got := range modelIDs(t, obj, provider) {
		if got == id {
			prov := obj["providers"].(map[string]any)[provider].(map[string]any)
			for _, m := range prov["models"].([]any) {
				if em := m.(map[string]any); em["id"] == id {
					return em
				}
			}
		}
	}
	t.Fatalf("%s has no entry %s", provider, id)
	return nil
}

func contains(xs []string, x string) bool {
	return slices.Contains(xs, x)
}

func mtimes(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = fi.ModTime().UnixNano()
	}
	return out
}

func equalTimes(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
