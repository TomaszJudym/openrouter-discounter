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
	return nil
}

func deliver(ctx context.Context, hc *http.Client, token, chatID, msg string) error {
	if err := telegram.Send(ctx, hc, token, chatID, msg); err != nil {
		return err // token already redacted by the telegram package
	}
	return nil
}
