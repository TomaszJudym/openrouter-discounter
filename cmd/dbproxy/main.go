// Command dbproxy serves the preset slugs locally for clients such as Hermes
// Agent: point the client's OpenAI base URL at it and keep the model names
// "@preset/<slug>". Each preset has two sides, the OpenRouter preset (kept
// by the tracker) and up to 3 Darkbloom-direct models; dbproxy tries the
// cheaper side first and falls back across both. Darkbloom stores no
// presets and has no model fallback, so for Darkbloom dbproxy injects the
// preset's prompt and parameters itself and walks the models in order.
//
// Picks are recomputed at start and every DBPROXY_REFRESH with the same
// code the tracker runs (internal/pipeline) and held in memory only. A
// refresh builds a whole new route table and swaps it in at the end
// (Proxy.SetRoutes, an atomic pointer), so requests served while a refresh
// is running — a minute or two — keep the previous table, and a failed run
// leaves it unchanged. Listens on loopback only.
//
// Environment: DARKBLOOM_API_KEY (required), AA_API_KEY (optional, finance
// scores), DBPROXY_ADDR (default 127.0.0.1:8787, loopback only),
// DBPROXY_REFRESH (default 2h), OPENROUTER_BASE_URL and DARKBLOOM_BASE_URL
// (default the production roots; override for staging or a local end-to-end
// test). The OpenRouter key is the client's own Authorization header,
// forwarded to OpenRouter only.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/pipeline"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
)

const (
	runTimeout = 5 * time.Minute
	// headerTimeout bounds the wait for an upstream's response headers;
	// past it the next backend is tried. Non-streamed replies send headers
	// only when done, so it must cover a long generation.
	headerTimeout = 10 * time.Minute
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("dbproxy: %v", err)
	}
}

func run() error {
	key := os.Getenv("DARKBLOOM_API_KEY")
	if key == "" {
		return errors.New("DARKBLOOM_API_KEY is required")
	}
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	hc, err := pipelineClient(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	upstream := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: headerTimeout,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}}
	p := &Proxy{
		HC:           upstream,
		OpenRouter:   cfg.openRouterBase,
		Darkbloom:    cfg.darkbloomBase,
		DarkbloomKey: key,
	}
	keys := pipeline.Keys{AA: os.Getenv("AA_API_KEY"), Darkbloom: key}
	go refresh(ctx, p, keys, hc, cfg.refresh)

	srv := &http.Server{Addr: cfg.addr, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx) // exiting anyway
	}()
	log.Printf("dbproxy on http://%s/v1 — model \"@preset/<slug>\", refresh every %s", cfg.addr, cfg.refresh)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// refresh recomputes the routes now and every `every`. Until the first run
// completes (a minute or two), every request goes to OpenRouter. Each run
// builds its route table in full and swaps it in at the end (SetRoutes), so
// a refresh in progress never clears the previous table: requests keep
// being served from it, and a failed run leaves it unchanged. Only a
// successful run whose Darkbloom side failed replaces the table with an
// OpenRouter-only one.
func refresh(ctx context.Context, p *Proxy, keys pipeline.Keys, hc *http.Client, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, runTimeout)
		res, err := pipeline.Run(rctx, hc, keys, time.Now())
		cancel()
		switch {
		case err != nil:
			log.Printf("refresh failed, keeping previous routes: %v", err)
		case res.DirectErr != "":
			log.Printf("refresh: Darkbloom unavailable (%s); OpenRouter only", res.DirectErr)
			p.SetRoutes(routes{})
		default:
			rt := buildRoutes(res)
			p.SetRoutes(rt)
			logRoutes(rt)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func logRoutes(rt routes) {
	for _, slug := range preset.SortedSlugs() {
		r, ok := rt[slug]
		if !ok {
			log.Printf("%s: openrouter only", slug)
			continue
		}
		first := "openrouter first"
		if r.dbFirst {
			first = "darkbloom first"
		}
		log.Printf("%s: %s, darkbloom %v", slug, first, r.direct)
	}
}
