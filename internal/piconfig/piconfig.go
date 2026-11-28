// Package piconfig keeps the local pi agent pointed at the best-deal
// preset models. It merge-writes two files in the agent dir:
//
//   - models.json: a "@preset/<slug>" entry per managed slug under the
//     openrouter provider (with cost derived from the winning row) and the
//     same ids under the loopback dbproxy provider, so pi-subagents can
//     route either surface;
//   - settings.json: subagents.agentOverrides per builtin pi-subagents
//     agent, choosing "dbproxy/<id>" or "openrouter/<id>" by whichever
//     surface's top row is cheaper.
//
// Merge-write never clobbers unmanaged keys: existing provider blocks,
// model entries and settings keys are kept, and only entries this package
// owns (marked managedBy) are replaced. Files are written atomically and
// skipped entirely when the rendered content is unchanged. No secret value
// is ever written: API keys are "$NAME" environment interpolations.
package piconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tomaszjudym/openrouter-discounter/internal/pipeline"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// managedBy marks every generated block, so a later run can tell its own
// entries from the user's and replace only its own.
const managedBy = "openrouter-discounter"

// orSector maps a managed slug to the rank sector string its rows are
// listed under: preset.Slugs inverted. admech-code ranks in the existing
// code sector; the seven admech-code-* sectors carry the slug as their own
// sector name. Both sides (OpenRouter and Darkbloom-direct) use the same
// sector layout.
var orSector = func() map[string]string {
	m := make(map[string]string, len(preset.Slugs))
	for sec, slug := range preset.Slugs {
		m[slug] = string(sec)
	}
	return m
}()

// agentFor maps the pi-subagents builtin agent to its preset slug, exactly
// the settings.json subagents.agentOverrides wiring.
var agentFor = map[string]string{
	"worker":           "admech-code",
	"oracle":           "admech-code-oracle",
	"reviewer":         "admech-code-reviewer",
	"researcher":       "admech-code-researcher",
	"evidence-auditor": "admech-code-auditor",
	"delegate":         "admech-code-delegate",
	"scout":            "admech-code-scout",
}

// reasoningSlugs are the models pi must treat as thinking-capable.
var reasoningSlugs = map[string]bool{
	"admech-code-oracle":     true,
	"admech-code-reviewer":   true,
	"admech-code-researcher": true,
}

const (
	presetPrefix         = "@preset/"
	defaultContextWindow = 262144
	oracleContextWindow  = 1050000

	dbproxyProvider = "dbproxy"
	dbproxyBaseURL  = "http://127.0.0.1:8787/v1"
	openrouterBase  = "https://openrouter.ai/api/v1"
	completionsAPI  = "openai-completions"
	// apiKeyEnv is an environment interpolation, never a secret value: pi
	// resolves it from the client's own environment at request time.
	apiKeyEnv = "$OPENROUTER_API_KEY" //nolint:gosec // G101: env-var reference, not a credential

	modelsFile   = "models.json"
	settingsFile = "settings.json"
	filePerm     = 0600
	roundTo      = 4 // decimals of USD per Mtok in the emitted cost
)

// Result reports which files a run wrote (or, for DryRun, would write).
type Result struct {
	Changed []string
}

// Pick is one managed slug's surface decision.
type Pick struct {
	Slug string
	// Model is the override value: "dbproxy/@preset/<slug>" or
	// "openrouter/@preset/<slug>".
	Model string
	// Cost is the emitted cost object (USD per Mtok per rate).
	Cost map[string]any
}

// Emit renders and writes both files under dir, the pi agent dir
// (~/.pi/agent). Files whose content is unchanged are left alone and omitted
// from Result.Changed.
func Emit(dir string, res pipeline.Result) (Result, error) {
	return emit(dir, res, true)
}

// DryRun renders both files without writing anything, reporting which files
// would change.
func DryRun(dir string, res pipeline.Result) (Result, error) {
	return emit(dir, res, false)
}

// Overrides maps each pi-subagents agent to the model id the run selected,
// so callers can show the decision without parsing settings.json.
func Overrides(res pipeline.Result) map[string]string {
	ps := picks(res)
	out := make(map[string]string, len(agentFor))
	for agent, slug := range agentFor {
		if p, ok := ps[slug]; ok {
			out[agent] = p.Model
		}
	}
	return out
}

// emit is Emit and DryRun. Picks with no priced row on either side are
// skipped: no entry is generated and any existing entry and override are
// left untouched (stale but working, never a broken reference). A dry run
// (apply false) is write-free down to the agent dir, which it does not
// create.
func emit(dir string, res pipeline.Result, apply bool) (Result, error) {
	ps := picks(res)

	if apply {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Result{}, fmt.Errorf("agent dir %s: %w", dir, err)
		}
	}
	var out Result
	if err := emitModels(dir, ps, apply, &out); err != nil {
		return Result{}, err
	}
	if err := emitSettings(dir, ps, apply, &out); err != nil {
		return Result{}, err
	}
	return out, nil
}

// picks applies the surface rule per managed slug: the Darkbloom-direct
// row wins (dbproxy surface) when it is no dearer than the OpenRouter
// winner; otherwise the OpenRouter row wins. A slug with rows on neither
// side is dropped.
func picks(res pipeline.Result) map[string]Pick {
	slugs := preset.ManagedSlugs()
	ps := make(map[string]Pick, len(slugs))
	for _, slug := range slugs {
		sector := orSector[slug]
		or, hasOR := topRow(res.OpenRouter, sector)
		db, hasDB := topRow(res.Direct, sector)
		var viaDB bool
		var row rank.Row
		switch {
		case hasDB && (!hasOR || db.Price <= or.Price):
			viaDB, row = true, db
		case hasOR:
			row = or
		default:
			continue
		}
		surface := "openrouter"
		if viaDB {
			surface = dbproxyProvider
		}
		ps[slug] = Pick{
			Slug:  slug,
			Model: surface + "/" + presetPrefix + slug,
			Cost:  costFromRow(row),
		}
	}
	return ps
}

// topRow is the sector's winner row: rows are ranked, so the first one.
func topRow(sectors []rank.SectorResult, sector string) (rank.Row, bool) {
	for _, sr := range sectors {
		if string(sr.Sector) == sector && len(sr.Rows) > 0 {
			return sr.Rows[0], true
		}
	}
	return rank.Row{}, false
}

// costFromRow derives the pi cost object from the winning row. The picked
// offer's per-rate prices (prompt/completion/cacheRead/cacheWrite) are not
// reachable from pipeline.Result yet, so every rate is the row's blended
// USD per Mtok: pi's cost display then matches the reported blended price.
// Integration swaps this body for the real per-Mtok rates; call sites stay
// behind it.
func costFromRow(row rank.Row) map[string]any {
	p := math.Round(row.Price*math.Pow10(roundTo)) / math.Pow10(roundTo)
	return map[string]any{"input": p, "output": p, "cacheRead": p, "cacheWrite": p}
}

// modelEntry is one generated models.json model entry for the slug.
func modelEntry(p Pick) map[string]any {
	cw := defaultContextWindow
	if p.Slug == "admech-code-oracle" {
		cw = oracleContextWindow
	}
	entry := map[string]any{
		"id":            presetPrefix + p.Slug,
		"managedBy":     managedBy,
		"contextWindow": cw,
		"cost":          p.Cost,
	}
	if reasoningSlugs[p.Slug] {
		entry["reasoning"] = true
	}
	return entry
}

// emitModels merge-writes models.json: the openrouter provider gains one
// "@preset/<slug>" entry per pick and the loopback dbproxy provider the
// same ids, replacing only previously managed entries and keeping every
// other provider, entry and key.
func emitModels(dir string, ps map[string]Pick, apply bool, out *Result) error {
	path := filepath.Join(dir, modelsFile)
	cur, err := readJSON(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	root := cur.obj
	if root == nil {
		root = map[string]any{}
	}
	providers, err := objectAt(root, "providers", "providers")
	if err != nil {
		return err
	}
	// entriesBySlug keeps only the managed entries replaced this run; a
	// slug with no pick today keeps its stale entry instead of losing it.
	stale := managedEntries(providers)
	for _, slug := range preset.ManagedSlugs() {
		p, ok := ps[slug]
		if !ok {
			continue
		}
		delete(stale, slug) // being regenerated below
		setEntry(providers, "openrouter", modelEntry(p), false)
		setEntry(providers, dbproxyProvider, modelEntry(p), true)
	}
	for _, entry := range stale {
		setEntry(providers, "openrouter", entry, false)
		setEntry(providers, dbproxyProvider, entry, true)
	}
	data, err := render(root)
	if err != nil {
		return fmt.Errorf("render %s: %w", modelsFile, err)
	}
	return write(path, cur.raw, data, apply, out)
}

// setEntry adds or replaces the "@preset/<id>" entry in provider's models.
// A missing provider block is created with the fields pi needs; an existing
// block is never modified beyond its models list.
func setEntry(providers map[string]any, provider string, entry map[string]any, loopback bool) {
	prov, ok := providers[provider].(map[string]any)
	if !ok {
		prov = map[string]any{"baseUrl": openrouterBase, "api": completionsAPI, "apiKey": apiKeyEnv}
		if loopback {
			prov["baseUrl"], prov["managedBy"] = dbproxyBaseURL, managedBy
		}
		providers[provider] = prov
	}
	models, _ := prov["models"].([]any)
	kept := models[:0:0]
	for _, m := range models {
		if em, ok := m.(map[string]any); ok && em["id"] == entry["id"] {
			continue
		}
		kept = append(kept, m)
	}
	prov["models"] = append(kept, entry)
}

// managedEntries collects the managed "@preset/<slug>" entries currently in
// both providers, keyed by slug, so slugs without a pick today survive.
func managedEntries(providers map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, provider := range []string{"openrouter", dbproxyProvider} {
		prov, ok := providers[provider].(map[string]any)
		if !ok {
			continue
		}
		models, _ := prov["models"].([]any)
		for _, m := range models {
			em, ok := m.(map[string]any)
			if !ok || em["managedBy"] != managedBy {
				continue
			}
			id, _ := em["id"].(string)
			if slug, ok := toSlug(id); ok {
				out[slug] = em
			}
		}
	}
	return out
}

// toSlug is the slug of a managed "@preset/<slug>" id.
func toSlug(id string) (string, bool) {
	s, ok := strings.CutPrefix(id, presetPrefix)
	if !ok || !slices.Contains(preset.ManagedSlugs(), s) {
		return "", false
	}
	return s, true
}

// emitSettings merge-writes settings.json's subagents.agentOverrides: each
// builtin agent's entry gains (or replaces) its "model" field, keeping
// every other key of the entry — thinking levels and the like — and every
// other settings key. pi-subagents rejects unknown fields in override
// entries, so no marker is added here.
func emitSettings(dir string, ps map[string]Pick, apply bool, out *Result) error {
	path := filepath.Join(dir, settingsFile)
	cur, err := readJSON(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	root := cur.obj
	if root == nil {
		root = map[string]any{}
	}
	subagents, err := objectAt(root, "subagents", "subagents")
	if err != nil {
		return err
	}
	overrides, err := objectAt(subagents, "agentOverrides", "subagents.agentOverrides")
	if err != nil {
		return err
	}
	for agent, slug := range agentFor {
		p, ok := ps[slug]
		if !ok {
			continue
		}
		if entry, ok := overrides[agent].(map[string]any); ok {
			entry["model"] = p.Model
			continue
		}
		overrides[agent] = map[string]any{"model": p.Model}
	}
	data, err := render(root)
	if err != nil {
		return fmt.Errorf("render %s: %w", settingsFile, err)
	}
	return write(path, cur.raw, data, apply, out)
}

// jsonFile is a parsed JSON object plus its raw bytes, for the unchanged
// check and to fail closed on an unparseable file rather than clobber it.
type jsonFile struct {
	obj map[string]any
	raw []byte
}

// readJSON reads and parses a JSON object file; a missing file yields a
// nil obj. Any other read or parse error is returned. The path is the
// caller-controlled agent-dir file this package owns.
func readJSON(path string) (jsonFile, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the agent dir's models.json/settings.json
	if errors.Is(err, os.ErrNotExist) {
		return jsonFile{}, nil
	}
	if err != nil {
		return jsonFile{}, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return jsonFile{}, fmt.Errorf("parse: %w", err)
	}
	return jsonFile{obj: obj, raw: raw}, nil
}

// objectAt returns root[key] as an object, creating it when absent and
// rejecting a non-object value under name.
func objectAt(root map[string]any, key, name string) (map[string]any, error) {
	switch v := root[key].(type) {
	case nil:
		obj := map[string]any{}
		root[key] = obj
		return obj, nil
	case map[string]any:
		return v, nil
	default:
		return nil, fmt.Errorf("%s is not a JSON object", name)
	}
}

// render is the on-disk form: two-space indented JSON with a trailing
// newline (map keys serialize sorted, so output is deterministic).
func render(root map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// write applies the rendered data atomically when it differs from the file
// on disk: temp file in the target directory, then rename, so a reader
// never sees a partial file and a failed write never destroys the old one.
func write(path string, cur, data []byte, apply bool, out *Result) error {
	name := filepath.Base(path)
	if cur != nil && bytes.Equal(cur, data) {
		return nil
	}
	out.Changed = append(out.Changed, name)
	if !apply {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pipresets-*")
	if err != nil {
		return fmt.Errorf("temp file for %s: %w", name, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := writeFile(tmp, data); err != nil {
		cleanup()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s: %w", name, err)
	}
	return nil
}

// writeFile writes and closes the temp file at 0600: models.json may carry
// a literal apiKey the user stored, so both files stay owner-only.
func writeFile(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(filePerm); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
