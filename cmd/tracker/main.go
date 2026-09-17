// Command tracker fetches the OpenRouter catalog, ranks active discounts per
// sector with Artificial Analysis scores, and delivers the report to
// Telegram. It runs stateless: one invocation, one report, no persisted
// state.
package main

import (
	"context"
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
		log.Fatal("TELEGRAM_BOT_TOKEN and TELEGRAM_CHANNEL_ID are required")
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
	if len(discs) == 0 {
		return deliver(ctx, hc, token, chatID, format.NoDiscounts(free, now))
	}

	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	prov := aa.New(hc, os.Getenv("AA_API_KEY"), ids)
	res := rank.Rank(ctx, discs, prov)

	for _, msg := range format.Messages(res, free, now) {
		if err := deliver(ctx, hc, token, chatID, msg); err != nil {
			return err
		}
	}
	updatePresets(ctx, hc, res)
	return nil
}

// updatePresets points the admech-* presets at each category's top 3 scored
// models. Failures are logged and non-fatal: the Telegram report is the
// primary output.
func updatePresets(ctx context.Context, hc *http.Client, res rank.Result) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		log.Printf("OPENROUTER_API_KEY not set; presets not updated")
		return
	}
	pc := preset.New(hc, key)
	for _, sr := range res.Sectors {
		updated, err := pc.Update(ctx, sr)
		if err != nil {
			log.Printf("preset %s: %v", preset.Slugs[sr.Sector], err)
			continue
		}
		if updated {
			log.Printf("preset %s: new version with top-%d models", preset.Slugs[sr.Sector], preset.TopN)
		}
	}
}

func deliver(ctx context.Context, hc *http.Client, token, chatID, msg string) error {
	if err := telegram.Send(ctx, hc, token, chatID, msg); err != nil {
		return err // token already redacted by the telegram package
	}
	return nil
}
