// Command tracker fetches the OpenRouter catalog, ranks active discounts per
// sector (Epoch AI benchmarks, Artificial Analysis finance index, OpenRouter
// popularity), and delivers the report to Telegram. It runs stateless: one invocation, one report, no persisted
// state.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/aa"
	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/epoch"
	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/format"
	"github.com/tomaszjudym/openrouter-discounter/internal/popularity"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
	"github.com/tomaszjudym/openrouter-discounter/internal/telegram"
)

// Deadlines. Score sources run concurrently under scoreTimeout, so one
// hanging source cannot eat the run; each Telegram send has sendTimeout of
// its own.
const (
	runTimeout   = 5 * time.Minute
	scoreTimeout = 90 * time.Second
	sendTimeout  = 60 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("tracker: %v", err)
	}
}

func run() error {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("TELEGRAM_CHANNEL_ID")
	if token == "" || chatID == "" {
		return errors.New("TELEGRAM_BOT_TOKEN and TELEGRAM_CHANNEL_ID are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	hc := &http.Client{Timeout: 60 * time.Second}
	now := time.Now()

	models, err := discounts.Fetch(ctx, hc)
	if err != nil {
		return err
	}
	discs, free, err := discounts.FetchDiscounts(ctx, hc, models, now)
	if err != nil {
		// individual endpoint failures are tolerable; the affected models
		// are simply missing from the report
		log.Printf("endpoint fetch failures (continuing): %v", err)
	}
	// Selection sees only rows within the price ceilings; expensive-long
	// picks from the full discounted set, ignoring the ceilings.
	allDiscs := discs
	discs = discounts.Affordable(discs)
	// The Telegram send gets its own deadline: slow fetches before it must
	// not cost the day's report.
	send := func(msg string) error {
		sctx, scancel := context.WithTimeout(context.Background(), sendTimeout)
		defer scancel()
		return telegram.Send(sctx, hc, token, chatID, msg)
	}
	if len(discs) == 0 {
		return send(format.NoDiscounts(now))
	}
	res := rankSectors(ctx, hc, models, discs, rank.TextFree(free, models))
	// The halflight sector is shown in Telegram; all strongest-discount
	// presets are updated with the same top-3 strongest-thinking models.
	intel := intelligence(models)
	intelOK := len(intel) > 0
	if !intelOK {
		log.Printf("catalog carries no intelligence index (skipping strongest-discount)")
	}
	telegramSectors := res.Sectors
	sdSectors, freeSectors := buildIntelSectors(models, discs, free, intel, intelOK)
	if len(sdSectors) > 0 {
		telegramSectors = append(telegramSectors, sdSectors[0])
	}
	telegramRes := rank.Result{Sectors: telegramSectors}
	for _, msg := range format.Messages(telegramRes, now) {
		if err := send(msg); err != nil {
			return err
		}
	}
	sectors := append(res.Sectors, rank.SelectExpensiveLong(models, allDiscs))
	if intelOK && len(sdSectors) > 0 {
		sectors = append(sectors, sdSectors...)
	}
	if intelOK && len(freeSectors) > 0 {
		sectors = append(sectors, freeSectors...)
	}
	if intelOK {
		sectors = append(sectors, hermesSectors(models, discs, intel, res.Scores[rank.Code].ByModel)...)
	}
	// Presets update after the Telegram send, so a failure here never costs
	// the report, but it fails the run so CI shows it.
	return updatePresets(ctx, hc, sectors)
}

// rankSectors scores the sectors from their sources — Epoch AI (math,
// code), Artificial Analysis (finance), OpenRouter popularity (long
// context) — and logs every sector that fell back to price-only.
func rankSectors(ctx context.Context, hc *http.Client, models []discounts.Model, discs []discounts.Discount, free []rank.FreeModel) rank.Result {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	ctx, cancel := context.WithTimeout(ctx, scoreTimeout)
	defer cancel()
	g := fetch.Getter{HC: hc}
	prov := rank.Providers{
		epoch.New(g, ids),
		aa.New(g, os.Getenv("AA_API_KEY"), ids),
		popularity.New(g, models, rank.LCR),
	}
	res := rank.Rank(ctx, discs, free, prov)
	for _, sr := range res.Sectors {
		if sr.Err != "" {
			log.Printf("sector %s price-only: %s error: %s", sr.Sector, sr.Source, sr.Err)
		}
	}
	return res
}

// intelligence maps catalog ids to the AA Intelligence Index that the
// OpenRouter catalog carries (benchmarks.artificial_analysis). A ":free" id
// without its own value inherits the base model's; other variants
// (":thinking", ":exacto", …) do not, so they cannot duplicate the base
// model in the strongest-discount top 3.
func intelligence(models []discounts.Model) map[string]float64 {
	out := make(map[string]float64, len(models))
	for _, m := range models {
		if v := m.Benchmarks.ArtificialAnalysis.IntelligenceIndex; v > 0 {
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

// buildIntelSectors creates the strongest-discount and halflight-free preset result
// when intelligence scores are available. Returns the strongest-discount sectors first
// (for Telegram) and free-tier sectors second (for presets only).
func buildIntelSectors(models []discounts.Model, discs []discounts.Discount, freeIDs []string, intel map[string]float64, intelOK bool) (sdSectors, freeSectors []rank.SectorResult) {
	if !intelOK {
		return nil, nil
	}
	sd := rank.SelectStrongestDiscount(models, discs, intel)
	if len(sd.Rows) > 0 {
		sdSectors = []rank.SectorResult{
			sd,
			{Sector: rank.StrongestDiscountVolition, Rows: sd.Rows},
			{Sector: rank.StrongestDiscountInlandEmpire, Rows: sd.Rows},
			{Sector: rank.StrongestDiscountShivers, Rows: sd.Rows},
		}
	}
	freeSectors = buildFreeDiscoSectors(freeIDs, models, intel)
	return sdSectors, freeSectors
}

// buildFreeDiscoSectors creates the halflight-free preset result from the
// top 3 free models ranked by the catalog's AA Intelligence Index.
func buildFreeDiscoSectors(freeIDs []string, models []discounts.Model, intel map[string]float64) []rank.SectorResult {
	base := rank.SelectFreeDisco(freeIDs, models, intel)
	if len(base.Rows) == 0 {
		return nil
	}
	return []rank.SectorResult{base}
}

// hermesSectors selects the per-role Hermes presets and logs each role's
// #1 pick with its quality source.
func hermesSectors(models []discounts.Model, discs []discounts.Discount, intel, code map[string]float64) []rank.SectorResult {
	out := rank.SelectHermes(models, discs, intel, code)
	for _, sr := range out {
		log.Printf("%s: #1 %s (%s %.1f)", sr.Sector, sr.Rows[0].ModelID, sr.Source, sr.Rows[0].Score)
	}
	return out
}

// updatePresets points each category's preset at its top 3 models: the
// admech-* presets follow the discounted rankings; expensive-long follows
// the strongest discounted thinking models, deepest discount first;
// strongest-discount-* presets route to the top-3 strongest-thinking
// discounted chat models (same models, distinct personas). Every preset is
// attempted; the failures are joined into the returned error so the run
// exits nonzero. A missing key only skips the updates.
func updatePresets(ctx context.Context, hc *http.Client, sectors []rank.SectorResult) error {
	key := os.Getenv("OPENROUTER_MANAGEMENT_KEY")
	if key == "" {
		log.Printf("OPENROUTER_MANAGEMENT_KEY not set; presets not updated")
		return nil
	}
	pc := preset.New(hc, key)
	var errs []error
	for _, sr := range sectors {
		slug := preset.Slugs[sr.Sector]
		updated, err := pc.Update(ctx, sr)
		if err != nil {
			log.Printf("preset %s: %v", slug, err)
			errs = append(errs, fmt.Errorf("preset %s: %w", slug, err))
			continue
		}
		if updated {
			log.Printf("preset %s: new version with top-%d models", slug, preset.TopN)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d preset updates failed: %w", len(errs), len(sectors), errors.Join(errs...))
	}
	return nil
}
