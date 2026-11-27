// Command dbproxy serves the preset slugs locally for clients such as Hermes
// Agent: point the client's OpenAI base URL at it and keep the model names
// "@preset/<slug>". Each preset has two sides, the OpenRouter preset (kept
// by the tracker) and up to 3 Darkbloom-direct models; dbproxy tries the
// cheaper side first and falls back across both. Darkbloom stores no
// presets and has no model fallback, so for Darkbloom dbproxy injects the
// preset's prompt and parameters itself and walks the models in order.
//
// Picks are recomputed at start and every RefreshEvery with the same code
// the tracker runs (internal/pipeline) and held in memory only. Listens on
// loopback only.
//
// Environment: DARKBLOOM_API_KEY (required), AA_API_KEY (optional, finance
// scores), DBPROXY_ADDR (default 127.0.0.1:8787). The OpenRouter key is the
// client's own Authorization header, forwarded to OpenRouter only.
package main

import (
	"cmp"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tomaszjudym/openrouter-discounter/internal/darkbloom"
	"github.com/tomaszjudym/openrouter-discounter/internal/pipeline"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
)

const (
	defaultAddr = "127.0.0.1:8787"
	// RefreshEvery matches the tracker's intra-day preset reruns.
	RefreshEvery = 2 * time.Hour
	runTimeout   = 5 * time.Minute
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
	addr := cmp.Or(os.Getenv("DBPROXY_ADDR"), defaultAddr)
	if host, _, err := net.SplitHostPort(addr); err != nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("DBPROXY_ADDR must be a loopback address: the proxy spends your keys")
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
		OpenRouter:   "https://openrouter.ai/api/v1",
		Darkbloom:    darkbloom.BaseURL,
		DarkbloomKey: key,
	}
	keys := pipeline.Keys{AA: os.Getenv("AA_API_KEY"), Darkbloom: key}
	go refresh(ctx, p, keys)

	srv := &http.Server{Addr: addr, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx) // exiting anyway
	}()
	log.Printf("dbproxy on http://%s/v1 — model \"@preset/<slug>\"", addr) //nolint:gosec // G706: addr was checked above to be a loopback host:port
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// refresh recomputes the routes now and every RefreshEvery. Until the
// first run completes (a minute or two), every request goes to OpenRouter.
// A failed run keeps the previous routes.
func refresh(ctx context.Context, p *Proxy, keys pipeline.Keys) {
	hc := &http.Client{Timeout: 60 * time.Second}
	tick := time.NewTicker(RefreshEvery)
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
