// Command tracker fetches the OpenRouter catalog, ranks active discounts per
// sector with Artificial Analysis scores, and delivers the report to
// Telegram. It runs stateless: one invocation, one report, no persisted
// state.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/aa"
	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/format"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
	"github.com/tomaszjudym/openrouter-discounter/internal/telegram"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	prov := aa.New(hc, os.Getenv("AA_API_KEY"), ids)
	res := rank.Rank(ctx, discs, free, prov)
	if len(discs) == 0 {
		return telegram.Send(ctx, hc, token, chatID, format.NoDiscounts(now))
	}
	// Fetch intelligence once, ahead of the Telegram send, so the
	// strongest-discount section can appear in the report and feed the
	// preset update with the same computation. The halflight sector is
	// shown in Telegram; both halflight and volition presets are updated
	// with the same top-3 strongest-thinking discounted models.
	intel, err := prov.Intelligence(ctx)
	if err != nil {
		log.Printf("Intelligence fetch failed (skipping strongest-discount): %v", err)
	}
	intelOK := err == nil
	telegramSectors := res.Sectors
	sdSectors, freeSectors := buildIntelSectors(models, discs, free, intel, intelOK)
	if len(sdSectors) > 0 {
		telegramSectors = append(telegramSectors, sdSectors[0])
	}
	telegramRes := rank.Result{Sectors: telegramSectors, GlobalErr: res.GlobalErr}
	for _, msg := range format.Messages(telegramRes, now) {
		if err := telegram.Send(ctx, hc, token, chatID, msg); err != nil {
			return err
		}
	}
	sectors := append(res.Sectors, rank.SelectExpensiveLong(models, discs))
	if intelOK && len(sdSectors) > 0 {
		sectors = append(sectors, sdSectors...)
	}
	if intelOK && len(freeSectors) > 0 {
		sectors = append(sectors, freeSectors...)
	}
	updatePresets(ctx, hc, sectors)
	return nil
}

// buildIntelSectors creates the strongest-discount and free-tier disco preset results
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

// buildFreeDiscoSectors creates the four free-tier disco preset results from the
// top 3 free models ranked by AA Intelligence Index. All four personas share the
// same model list.
func buildFreeDiscoSectors(freeIDs []string, models []discounts.Model, intel map[string]float64) []rank.SectorResult {
	base := rank.SelectFreeDisco(freeIDs, models, intel)
	if len(base.Rows) == 0 {
		return nil
	}
	return []rank.SectorResult{
		base,
		{Sector: rank.FreeVolition, Rows: base.Rows},
		{Sector: rank.FreeInlandEmpire, Rows: base.Rows},
		{Sector: rank.FreeShivers, Rows: base.Rows},
	}
}

// updatePresets points each category's preset at its top 3 models: the
// admech-* presets follow the discounted rankings; expensive-long follows
// the strongest thinking frontier with discount-resorted order;
// strongest-discount-* presets route to the top-3 strongest-thinking
// discounted chat models (same models, distinct personas). Failures are
// logged and non-fatal: the Telegram report is the primary output.
func updatePresets(ctx context.Context, hc *http.Client, sectors []rank.SectorResult) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		log.Printf("OPENROUTER_API_KEY not set; presets not updated")
		return
	}
	pc := preset.New(hc, key)
	for _, sr := range sectors {
		slug := preset.Slugs[sr.Sector]
		updated, err := pc.Update(ctx, sr)
		if err != nil {
			log.Printf("preset %s: %v", slug, err)
			continue
		}
		if updated {
			log.Printf("preset %s: new version with top-%d models", slug, preset.TopN)
		}
	}
}
