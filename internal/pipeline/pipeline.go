// Package pipeline runs one stateless selection: fetch the OpenRouter
// catalog and endpoint prices, the Darkbloom-direct prices, and the score
// sources, then rank every report sector and select every preset — once for
// OpenRouter and once for Darkbloom-direct over the same scores. The
// tracker (report, OpenRouter presets) and dbproxy (Darkbloom presets) both
// call Run, so they pick the same models.
package pipeline

import (
	"context"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/aa"
	"github.com/tomaszjudym/openrouter-discounter/internal/darkbloom"
	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/epoch"
	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/popularity"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// scoreTimeout bounds the concurrent score-source fetches, so one hanging
// source cannot eat the run.
const scoreTimeout = 90 * time.Second

// Keys are the optional API keys; an empty key degrades its source only.
type Keys struct {
	AA        string // Artificial Analysis: finance sector
	Darkbloom string // Darkbloom /v1/models: the direct side
}

// Result is one run's selections.
type Result struct {
	// NoDiscounts reports nothing to rank on either side and no Darkbloom
	// error: no affordable OpenRouter discount, no priced Darkbloom-direct
	// row and no Darkbloom failure. Only then is the report the short
	// message and do the OpenRouter presets stay as they are; a day with
	// priced direct rows — or a Darkbloom failure, reported as its own
	// section — shows a report instead.
	NoDiscounts bool
	// Report is the Telegram ranking: the OpenRouter sectors, the
	// strongest-discount pick and, with a Darkbloom key, the direct prices.
	Report rank.Result
	// OpenRouter and Direct are every preset sector per backend, in preset
	// order; Direct rows carry "db:" endpoints (see discounts.DirectPrefix).
	OpenRouter []rank.SectorResult
	Direct     []rank.SectorResult
	// DirectErr is the sanitized Darkbloom error; empty when it worked or
	// no key is set.
	DirectErr string
}

// Run fetches and selects. Only a catalog failure is an error; endpoint,
// score and Darkbloom failures degrade the result and are logged.
func Run(ctx context.Context, hc *http.Client, keys Keys, now time.Time) (Result, error) {
	models, err := discounts.Fetch(ctx, hc)
	if err != nil {
		return Result{}, err
	}
	discs, priced, free, err := discounts.FetchDiscounts(ctx, hc, models, now)
	if err != nil {
		// the affected models are simply missing from the report
		log.Printf("endpoint fetch failures (continuing): %v", err)
	}
	logWindows(discs)
	// Selection sees only rows within the price ceilings; expensive-long
	// picks from the full discounted set, ignoring the ceilings.
	allDiscs := discs
	discs = discounts.Affordable(discs)

	var res Result
	directDiscs, directPriced, err := direct(ctx, hc, keys.Darkbloom, models, priced, now)
	if err != nil {
		res.DirectErr = err.Error()
		log.Printf("%s direct side skipped: %s", darkbloom.SourceName, res.DirectErr)
	}
	// Nothing to report only when both sides come up empty and Darkbloom
	// did not fail: a day with priced Darkbloom-direct rows still shows the
	// direct table, and a failed Darkbloom side is reported as an error
	// section rather than a bare "no discounts today." that hides the
	// outage.
	res.NoDiscounts = len(discs) == 0 && len(directPriced) == 0 && res.DirectErr == ""
	if res.NoDiscounts {
		return res, nil
	}

	// Scores are only worth fetching when something is ranked: a Darkbloom
	// failure with no OpenRouter discounts reports the error alone.
	var scores map[rank.Sector]rank.SectorScores
	if len(discs) > 0 || len(directPriced) > 0 {
		scores = fetchScores(ctx, hc, models, keys.AA)
	}
	intel := intelligence(models)
	if len(intel) == 0 {
		log.Printf("catalog carries no intelligence index (skipping strongest-discount and hermes)")
	}
	if len(discs) > 0 {
		// nothing affordable on the OpenRouter side: the report carries only
		// the direct table and the OpenRouter presets stay as they are
		res.Report, res.OpenRouter = openrouterSide(models, discs, allDiscs, priced, rank.TextFree(free, models), scores, intel)
	}
	if len(directPriced) > 0 {
		res.Direct = directSide(models, directDiscs, directPriced, scores, intel)
		// the report always carries the direct table: NoDiscounts is false
		// whenever directPriced is non-empty
		res.Report.Sectors = append(res.Report.Sectors, rank.SelectDirect(directPriced, intel))
	}
	if res.DirectErr != "" {
		// the direct table is missing: say why under its own header, where
		// the table would have been. On a day with nothing else to show the
		// notice also carries that, so the message is not a bare error.
		extra := ""
		if len(discs) == 0 {
			extra = "no discounts found today"
		}
		res.Report.Sectors = append(res.Report.Sectors, directErrSector(res.DirectErr, extra))
	}
	return res, nil
}

// openrouterSide ranks the OpenRouter side into the report and its presets,
// logging every price-only sector's failure.
func openrouterSide(models []discounts.Model, discs, allDiscs, priced []discounts.Discount, free []rank.FreeModel, scores map[rank.Sector]rank.SectorScores, intel map[string]float64) (report rank.Result, presets []rank.SectorResult) {
	report, presets = selectSide(models, discs, allDiscs, priced, free, scores, intel)
	for _, sr := range report.Sectors {
		if sr.Err != "" {
			log.Printf("sector %s price-only: %s error: %s", sr.Sector, sr.Source, sr.Err)
		}
	}
	return report, presets
}

// directSide ranks the Darkbloom-direct side's presets; the report's direct
// table is appended separately. Only sectors with rows make presets.
func directSide(models []discounts.Model, directDiscs, directPriced []discounts.Discount, scores map[rank.Sector]rank.SectorScores, intel map[string]float64) []rank.SectorResult {
	_, presets := selectSide(models, discounts.Affordable(directDiscs), directDiscs, directPriced, nil, scores, intel)
	out := make([]rank.SectorResult, 0, len(presets))
	for _, sr := range presets {
		if len(sr.Rows) > 0 {
			out = append(out, sr)
		}
	}
	return out
}

// directErrSector is the DARKBLOOM section's fallback when the direct
// fetch failed: the section header stays and its notice carries the
// sanitized error, reusing the per-sector notice parameter (see
// rank.SectorResult.Err) so internal/format needs no change. Darkbloom is
// not a score source, but the notice slot is where a section-level failure
// is shown. extra, when non-empty, is prepended to the error text.
func directErrSector(errText, extra string) rank.SectorResult {
	if extra != "" {
		errText = extra + "; " + errText
	}
	return rank.SectorResult{Sector: rank.DarkbloomDirect, Source: darkbloom.SourceName, Err: errText}
}

// direct prices the Darkbloom-direct side; no key means no side.
func direct(ctx context.Context, hc *http.Client, key string, models []discounts.Model, priced []discounts.Discount, now time.Time) (discs, pricedOut []discounts.Discount, err error) {
	if key == "" {
		return nil, nil, nil
	}
	dm, err := darkbloom.Fetch(ctx, fetch.Getter{HC: hc}, darkbloom.BaseURL, key)
	if err != nil {
		return nil, nil, err
	}
	mapped, unmapped := darkbloom.Resolve(dm, models)
	if len(unmapped) > 0 {
		log.Printf("%s models without an OpenRouter base, skipped: %s", darkbloom.SourceName, strings.Join(unmapped, ", "))
	}
	discs, pricedOut = discounts.Direct(mapped, priced, now)
	return discs, pricedOut, nil
}

// fetchScores fetches every sector's scores — Epoch AI (math, code),
// Artificial Analysis (finance), OpenRouter popularity (long context) —
// concurrently under scoreTimeout.
func fetchScores(ctx context.Context, hc *http.Client, models []discounts.Model, aaKey string) map[rank.Sector]rank.SectorScores {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	ctx, cancel := context.WithTimeout(ctx, scoreTimeout)
	defer cancel()
	g := fetch.Getter{HC: hc}
	prov := rank.Providers{
		// models WebDev Arena does not rate fall back to the catalog's
		// coding index, calibrated onto the Epoch scale
		rank.Backfill{
			Provider: epoch.New(g, ids),
			Sectors:  []rank.Sector{rank.Code, rank.CodeFree},
			Fallback: codingIndex(models),
		},
		aa.New(g, aaKey, ids),
		popularity.New(g, models, rank.LCR),
	}
	return prov.Scores(ctx)
}

// selectSide ranks one backend's rows: discs are the affordable discounts,
// allDiscs every discount (expensive-long ignores the ceilings), priced
// every priced row (the byCost hermes and admech roles). report is the
// Telegram ranking — the report sectors plus the strongest-discount pick;
// presets every preset sector, including the seven admech-code-<role> picks
// that never appear in the report. Every managed slug with candidates gets
// its sector here, so preset updates never miss one.
func selectSide(models []discounts.Model, discs, allDiscs, priced []discounts.Discount, free []rank.FreeModel, scores map[rank.Sector]rank.SectorScores, intel map[string]float64) (report rank.Result, presets []rank.SectorResult) {
	report = rank.ByScores(discs, free, scores)
	presets = slices.Clone(report.Sectors)
	presets = append(presets, rank.SelectExpensiveLong(models, allDiscs))
	// The role sectors come from the same side's rows (darkbloom-direct
	// rows carry db: endpoints), so cmd/dbproxy can route them too.
	presets = append(presets, rank.SelectAdmech(models, discs, priced, scores)...)
	if len(intel) == 0 {
		return report, presets
	}
	if sd := intelSectors(models, discs, intel); len(sd) > 0 {
		// clone first: appending to report.Sectors must not share an array
		// with presets
		report.Sectors = append(slices.Clone(report.Sectors), sd[0])
		presets = append(presets, sd...)
	}
	return report, append(presets, hermesSectors(models, discs, priced, intel, scores[rank.Code].ByModel)...)
}

// logWindows logs every discount that holds only during a UTC time window.
func logWindows(discs []discounts.Discount) {
	for _, d := range discs {
		if d.Window != "" {
			log.Printf("window discount %s via %s: %.0f%% during %s", d.ModelID, provider(d.Provider), d.Pct, d.Window)
		}
	}
}

// provider names a discount's endpoint; catalog rows price every provider.
func provider(p string) string {
	if p == "" {
		return "any provider"
	}
	return p
}

// intelligence maps catalog ids to the AA Intelligence Index that the
// OpenRouter catalog carries (benchmarks.artificial_analysis).
func intelligence(models []discounts.Model) map[string]float64 {
	return catalogScores(models, func(m discounts.Model) float64 { return m.Benchmarks.ArtificialAnalysis.IntelligenceIndex })
}

// codingIndex maps catalog ids to the catalog's AA Coding Index, the
// fallback for models WebDev Arena does not rate.
func codingIndex(models []discounts.Model) map[string]float64 {
	return catalogScores(models, func(m discounts.Model) float64 { return m.Benchmarks.ArtificialAnalysis.CodingIndex })
}

// catalogScores maps catalog ids to a positive catalog benchmark value. A
// ":free" id without its own value inherits the base model's; other
// variants (":thinking", ":exacto", …) do not, so they cannot duplicate the
// base model in a top 3.
func catalogScores(models []discounts.Model, value func(discounts.Model) float64) map[string]float64 {
	out := make(map[string]float64, len(models))
	for _, m := range models {
		if v := value(m); v > 0 {
			out[m.ID] = v
		}
	}
	for _, m := range models {
		if _, ok := out[m.ID]; ok {
			continue
		}
		if base, ok := strings.CutSuffix(m.ID, ":free"); ok {
			if v, ok := out[base]; ok {
				out[m.ID] = v
			}
		}
	}
	return out
}

// intelSectors builds the strongest-discount sectors: all four persona
// presets route to the same top-3 strongest-thinking discounted models.
// The halflight sector comes first (it is the one shown in Telegram).
func intelSectors(models []discounts.Model, discs []discounts.Discount, intel map[string]float64) []rank.SectorResult {
	sd := rank.SelectStrongestDiscount(models, discs, intel)
	if len(sd.Rows) == 0 {
		return nil
	}
	return []rank.SectorResult{
		sd,
		{Sector: rank.StrongestDiscountVolition, Rows: sd.Rows},
		{Sector: rank.StrongestDiscountInlandEmpire, Rows: sd.Rows},
		{Sector: rank.StrongestDiscountShivers, Rows: sd.Rows},
	}
}

// hermesSectors selects the per-role Hermes presets and logs each role's
// #1 pick with its quality source.
func hermesSectors(models []discounts.Model, discs, priced []discounts.Discount, intel, code map[string]float64) []rank.SectorResult {
	out := rank.SelectHermes(models, discs, priced, intel, code)
	for _, sr := range out {
		log.Printf("%s: #1 %s via %s (%s %.1f)", sr.Sector, sr.Rows[0].ModelID, provider(sr.Rows[0].Provider), sr.Source, sr.Rows[0].Score)
	}
	return out
}
