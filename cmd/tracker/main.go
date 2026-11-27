// Command tracker fetches the OpenRouter catalog and Darkbloom-direct
// prices, ranks active discounts per sector (Epoch AI benchmarks, Artificial
// Analysis finance index, OpenRouter popularity; see internal/pipeline),
// delivers the report to Telegram and repoints the OpenRouter presets. It
// runs stateless: one invocation, one report, no persisted state.
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

	"github.com/tomaszjudym/openrouter-discounter/internal/format"
	"github.com/tomaszjudym/openrouter-discounter/internal/pipeline"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
	"github.com/tomaszjudym/openrouter-discounter/internal/telegram"
)

// Deadlines. Each Telegram send has sendTimeout of its own.
const (
	runTimeout  = 5 * time.Minute
	sendTimeout = 60 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("tracker: %v", err)
	}
}

// telegramSender builds the report send. TRACKER_REPORT=false is a
// presets-only run: the intra-day reruns that repoint presets after a
// discount ends send nothing and need no Telegram secrets. Each send gets
// its own deadline: slow fetches before it must not cost the day's report.
func telegramSender(hc *http.Client) (func(msg string) error, error) {
	if os.Getenv("TRACKER_REPORT") == "false" {
		return func(string) error { return nil }, nil
	}
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("TELEGRAM_CHANNEL_ID")
	if token == "" || chatID == "" {
		return nil, errors.New("TELEGRAM_BOT_TOKEN and TELEGRAM_CHANNEL_ID are required")
	}
	return func(msg string) error {
		sctx, scancel := context.WithTimeout(context.Background(), sendTimeout)
		defer scancel()
		return telegram.Send(sctx, hc, token, chatID, msg)
	}, nil
}

func run() error {
	hc := &http.Client{Timeout: 60 * time.Second}
	send, err := telegramSender(hc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	now := time.Now()

	res, err := pipeline.Run(ctx, hc, pipeline.Keys{
		AA:        os.Getenv("AA_API_KEY"),
		Darkbloom: os.Getenv("DARKBLOOM_API_KEY"),
	}, now)
	if err != nil {
		return err
	}
	logDirect(res.Direct)
	logPicks(res.OpenRouter)
	if res.NoDiscounts {
		return send(format.NoDiscounts(now))
	}
	for _, msg := range format.Messages(res.Report, now) {
		if err := send(msg); err != nil {
			return err
		}
	}
	// Presets update after the Telegram send, so a failure here never costs
	// the report, but it fails the run so CI shows it.
	return updatePresets(ctx, hc, res.OpenRouter)
}

// logDirect logs the Darkbloom-direct rows preset.Picks routes per preset:
// the same rankings cmd/dbproxy computes for itself — dbproxy re-picks at
// its own now, nothing here hands rows to it or writes them anywhere.
func logDirect(sectors []rank.SectorResult) {
	logSide("darkbloom", sectors)
}

// logPicks logs the OpenRouter-side rows preset.Picks routes per preset:
// the same rankings updatePresets pushes into the OpenRouter presets.
// Unscored and unpinnable rows are dropped by preset.Picks, so on the
// price-only fallback path the log can list fewer than preset.TopN.
func logPicks(sectors []rank.SectorResult) {
	logSide("openrouter", sectors)
}

// logSide logs each preset sector's routed models: preset.Picks' rows, the
// exact selection the presets route by.
func logSide(side string, sectors []rank.SectorResult) {
	for _, sr := range sectors {
		picks := preset.Picks(sr.Rows)
		ids := make([]string, 0, len(picks))
		for _, r := range picks {
			ids = append(ids, r.ModelID)
		}
		log.Printf("%s %s: %s", side, preset.Slugs[sr.Sector], strings.Join(ids, ", "))
	}
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
