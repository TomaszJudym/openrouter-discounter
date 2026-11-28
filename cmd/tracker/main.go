// Command tracker fetches the OpenRouter catalog and Darkbloom-direct
// prices, ranks active discounts per sector (Epoch AI benchmarks, Artificial
// Analysis finance index, OpenRouter popularity; see internal/pipeline),
// delivers the report to Telegram and repoints the OpenRouter presets. It
// runs stateless: one invocation, one report, no persisted state.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/format"
	"github.com/tomaszjudym/openrouter-discounter/internal/piconfig"
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

// piDir is the pi agent dir rewritten after a successful preset update;
// empty falls back to $TRACKER_PIPRESETS. Both empty keep the pi config
// surface off: the Actions run has no agent dir of its own and must not
// write into the runner home.
var piDir = flag.String("pi-dir", "", "pi agent dir to update after the report (default $TRACKER_PIPRESETS; both empty skip the pi config)")

func main() {
	flag.Parse()
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
	// the report, but it fails the run so CI shows it. The local pi config
	// is a separate surface: a preset failure must not skip it. It stays off
	// unless --pi-dir or $TRACKER_PIPRESETS asks for it.
	var errs []error
	if err := updatePresets(ctx, hc, res.OpenRouter); err != nil {
		errs = append(errs, err)
	}
	if dir := piConfigDir(); dir != "" {
		if err := emitPiConfig(dir, res); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// piConfigDir resolves the opt-in pi config surface: the --pi-dir flag, then
// $TRACKER_PIPRESETS. Empty means no emit at all. There is deliberately no
// home-dir default: ~/.pi/agent does not exist on a CI runner, and creating
// it there is a write outside the repo that can fail the run when $HOME is
// unset.
func piConfigDir() string {
	if *piDir != "" {
		return *piDir
	}
	return os.Getenv("TRACKER_PIPRESETS")
}

// emitPiConfig merge-writes the local pi agent's models.json and
// settings.json (see internal/piconfig) into dir. run calls it only when
// piConfigDir named a dir; the pi agent dir is outside the repo.
func emitPiConfig(dir string, res pipeline.Result) error {
	out, err := piconfig.Emit(dir, res)
	if err != nil {
		return fmt.Errorf("pi config: %w", err)
	}
	if len(out.Changed) > 0 {
		log.Printf("pi config %s: wrote %v", dir, out.Changed)
	}
	return nil
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

// sectorBySlug is preset.Slugs inverted: the rank sector each managed
// slug's rows live under. admech-code ranks in the code sector; the seven
// admech-code-<role> sectors carry their slug as the sector name.
var sectorBySlug = func() map[string]rank.Sector {
	m := make(map[string]rank.Sector, len(preset.Slugs))
	for sec, slug := range preset.Slugs {
		m[slug] = sec
	}
	return m
}()

// updatePresets points exactly the managed presets (preset.ManagedSlugs)
// at their top 3 rows via UpdateManaged. Every managed slug is attempted,
// even one without candidates today (UpdateManaged is then a no-op); a
// legacy preset such as admech-math or hermes-* is never touched. The
// failures are joined into the returned error so the run exits nonzero. A
// missing key only skips the updates.
func updatePresets(ctx context.Context, hc *http.Client, sectors []rank.SectorResult) error {
	key := os.Getenv("OPENROUTER_MANAGEMENT_KEY")
	if key == "" {
		log.Printf("OPENROUTER_MANAGEMENT_KEY not set; presets not updated")
		return nil
	}
	bySlug := make(map[string]rank.SectorResult, len(sectors))
	for _, sr := range sectors {
		if slug, ok := preset.Slugs[sr.Sector]; ok {
			bySlug[slug] = sr
		}
	}
	pc := preset.New(hc, key)
	managed := preset.ManagedSlugs()
	var errs []error
	for _, slug := range managed {
		sr, ok := bySlug[slug]
		if !ok {
			sec, found := sectorBySlug[slug]
			if !found {
				continue
			}
			sr = rank.SectorResult{Sector: sec}
		}
		updated, err := pc.UpdateManaged(ctx, sr)
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
		return fmt.Errorf("%d of %d preset updates failed: %w", len(errs), len(managed), errors.Join(errs...))
	}
	return nil
}
