package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// clearDBProxyEnv unsets every overridable variable, so a test starts from
// the production defaults.
func clearDBProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"DBPROXY_ADDR", "OPENROUTER_BASE_URL", "DARKBLOOM_BASE_URL", "DBPROXY_REFRESH"} {
		t.Setenv(k, "")
	}
}

func TestConfigDefaults(t *testing.T) {
	clearDBProxyEnv(t)
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		addr:           "127.0.0.1:8787",
		openRouterBase: "https://openrouter.ai/api/v1",
		darkbloomBase:  "https://api.darkbloom.dev/v1",
		refresh:        2 * time.Hour,
	}
	if cfg != want {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
}

// TestConfigFromEnv: every override is read, the API roots are normalized
// (trailing slash trimmed) and DBPROXY_REFRESH is a Go duration.
func TestConfigFromEnv(t *testing.T) {
	clearDBProxyEnv(t)
	t.Setenv("DBPROXY_ADDR", "127.0.0.1:9123")
	t.Setenv("OPENROUTER_BASE_URL", "http://127.0.0.1:9001/or/v1/")
	t.Setenv("DARKBLOOM_BASE_URL", "http://127.0.0.1:9001/db/v1")
	t.Setenv("DBPROXY_REFRESH", "30m")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		addr:           "127.0.0.1:9123",
		openRouterBase: "http://127.0.0.1:9001/or/v1",
		darkbloomBase:  "http://127.0.0.1:9001/db/v1",
		refresh:        30 * time.Minute,
	}
	if cfg != want {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	for _, tt := range []struct{ name, key, value string }{
		{"non-loopback addr", "DBPROXY_ADDR", "0.0.0.0:8787"},
		{"hostname addr", "DBPROXY_ADDR", "proxy.local:8787"},
		{"unparseable refresh", "DBPROXY_REFRESH", "every-two-hours"},
		{"zero refresh", "DBPROXY_REFRESH", "0s"},
		{"non-http root", "OPENROUTER_BASE_URL", "ftp://openrouter.ai/api/v1"},
		{"relative root", "DARKBLOOM_BASE_URL", "/v1"},
		{"root with query", "DARKBLOOM_BASE_URL", "https://api.darkbloom.dev/v1?x=1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearDBProxyEnv(t)
			t.Setenv(tt.key, tt.value)
			if _, err := configFromEnv(); err == nil {
				t.Fatalf("%s=%q accepted", tt.key, tt.value)
			}
		})
	}
}

// TestPipelineClientRewritesBaseURLs: the pipeline builds its URLs from
// fixed production roots, so OPENROUTER_BASE_URL and DARKBLOOM_BASE_URL
// have to reach the stub through the client's transport, path intact.
func TestPipelineClientRewritesBaseURLs(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := config{
		addr:           defaultAddr,
		openRouterBase: srv.URL + "/or/v1",
		darkbloomBase:  srv.URL + "/db/v1",
		refresh:        defaultRefresh,
	}
	hc, err := pipelineClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		defaultOpenRouterBase + "/models",
		defaultOpenRouterBase + "/models/mistralai/mistral-small/endpoints",
		defaultDarkbloomBase + "/models",
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", target, resp.StatusCode)
		}
	}
	want := []string{
		"GET /or/v1/models",
		"GET /or/v1/models/mistralai/mistral-small/endpoints",
		"GET /db/v1/models",
	}
	if !slices.Equal(got, want) {
		t.Errorf("stub saw %v, want %v", got, want)
	}
}

// TestPipelineClientLeavesOtherHostsAlone: the score sources and
// OpenRouter's non-API frontend paths are not under either API root, so
// they keep their own URL (the production defaults must not rewrite them).
func TestPipelineClientLeavesOtherHostsAlone(t *testing.T) {
	cfg := config{
		addr:           defaultAddr,
		openRouterBase: "http://127.0.0.1:9001/or/v1",
		darkbloomBase:  "http://127.0.0.1:9001/db/v1",
		refresh:        defaultRefresh,
	}
	hc, err := pipelineClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	rw := hc.Transport.(baseRewriter)
	rw.base = rtFunc(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.URL.String())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    r,
		}, nil
	})
	hc.Transport = rw
	targets := []string{
		"https://openrouter.ai/api/frontend/v1/rankings/models",
		"https://api.artificialanalysis.ai/api/v2/data/llms/models",
		"https://epoch.ai/api/benchmarks",
	}
	for _, target := range targets {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		_ = resp.Body.Close()
	}
	if !slices.Equal(got, targets) {
		t.Errorf("base saw %v, want the original URLs %v", got, targets)
	}
}
