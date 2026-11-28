// Command pipresets repoints the local pi agent (models.json and
// settings.json, see internal/piconfig) at the current best-deal preset
// models. It runs the same selection pipeline as the tracker — the
// OpenRouter side against the OpenRouter presets, the Darkbloom-direct side
// against the loopback dbproxy — and applies the surface rule per role:
// Darkbloom wins when its top row is no dearer than OpenRouter's. Nothing
// here fetches on its own; internal/pipeline.Run does the fetching.
//
// Flags:
//
//	--dir      pi agent dir (default ~/.pi/agent)
//	--dry-run  print what would change, write nothing
//
// Environment: DARKBLOOM_API_KEY optional (no key: every pick routes to
// openrouter), AA_API_KEY optional (finance scores), OPENROUTER_BASE_URL and
// DARKBLOOM_BASE_URL optional (default the production roots; override for a
// staging stack or a local end-to-end test, same names as cmd/dbproxy). No
// other key is read: the written API key is the "$OPENROUTER_API_KEY"
// interpolation pi resolves from its own environment.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/piconfig"
	"github.com/tomaszjudym/openrouter-discounter/internal/pipeline"
)

const runTimeout = 5 * time.Minute

func main() {
	if err := run(); err != nil {
		log.Fatalf("pipresets: %v", err)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	dir := flag.String("dir", filepath.Join(home, ".pi", "agent"), "pi agent dir")
	dryRun := flag.Bool("dry-run", false, "print what would change, write nothing")
	flag.Parse()

	hc, err := fetch.EnvClient(&http.Client{Timeout: 60 * time.Second})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	res, err := pipeline.Run(ctx, hc, pipeline.Keys{
		AA:        os.Getenv("AA_API_KEY"),
		Darkbloom: os.Getenv("DARKBLOOM_API_KEY"),
	}, time.Now())
	if err != nil {
		return err
	}

	var out piconfig.Result
	if *dryRun {
		out, err = piconfig.DryRun(*dir, res)
	} else {
		out, err = piconfig.Emit(*dir, res)
	}
	if err != nil {
		return err
	}

	overrides := piconfig.Overrides(res)
	for _, agent := range slices.Sorted(maps.Keys(overrides)) {
		fmt.Printf("%-17s %s\n", agent, overrides[agent])
	}
	if len(out.Changed) == 0 {
		fmt.Println("config already up to date")
	} else if *dryRun {
		fmt.Printf("would write: %v\n", out.Changed)
	} else {
		fmt.Printf("wrote: %v\n", out.Changed)
	}
	return nil
}
