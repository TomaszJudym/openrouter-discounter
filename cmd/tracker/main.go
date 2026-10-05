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
	res := rank.Rank(ctx, discs, free, aa.New(hc, os.Getenv("AA_API_KEY"), ids))
	if len(discs) == 0 {
		return telegram.Send(ctx, hc, token, chatID, format.NoDiscounts(free, now))
	}
	for _, msg := range format.Messages(res, free, now) {
		if err := telegram.Send(ctx, hc, token, chatID, msg); err != nil {
			return err
		}
	}
	updatePresets(ctx, hc, append(res.Sectors, rank.SelectExpensiveLong(models, discs)))
	return nil
}

// updatePresets points each category's preset at its top 3 models: the
// admech-* presets follow the discounted rankings; expensive-long follows
// the strongest thinking frontier with discount-resorted order. Failures
// are logged and non-fatal: the Telegram report is the primary output.
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
