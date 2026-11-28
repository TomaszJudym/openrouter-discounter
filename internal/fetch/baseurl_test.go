package fetch

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// recordTransport records the URL it is asked for and answers 200.
type recordTransport struct{ urls []string }

func (r *recordTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL.String())
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func TestEnvClientUnsetIsUnchanged(t *testing.T) {
	t.Setenv("OPENROUTER_BASE_URL", "")
	t.Setenv("DARKBLOOM_BASE_URL", "")
	hc := &http.Client{}
	got, err := EnvClient(hc)
	if err != nil {
		t.Fatalf("EnvClient: %v", err)
	}
	if got != hc {
		t.Errorf("EnvClient returned a wrapper, want the client unchanged")
	}
}

func TestEnvClientRewritesProductionRoots(t *testing.T) {
	t.Setenv("OPENROUTER_BASE_URL", "http://127.0.0.1:9001/or/v1/")
	t.Setenv("DARKBLOOM_BASE_URL", "http://127.0.0.1:9001/db/v1")
	rt := &recordTransport{}
	hc, err := EnvClient(&http.Client{Transport: rt})
	if err != nil {
		t.Fatalf("EnvClient: %v", err)
	}
	for _, target := range []string{
		"https://openrouter.ai/api/v1/models/acme/orion/endpoints",
		"https://api.darkbloom.dev/v1/models",
		"https://epoch.ai/data/benchmark_data.zip",
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("do %s: %v", target, err)
		}
		_ = resp.Body.Close()
	}
	want := []string{
		"http://127.0.0.1:9001/or/v1/models/acme/orion/endpoints", // trailing slash of the override is dropped
		"http://127.0.0.1:9001/db/v1/models",
		"https://epoch.ai/data/benchmark_data.zip", // other hosts pass through
	}
	for i, w := range want {
		if rt.urls[i] != w {
			t.Errorf("request %d went to %s, want %s", i, rt.urls[i], w)
		}
	}
}

func TestEnvClientRejectsBadRoot(t *testing.T) {
	for _, tc := range []struct{ env, value string }{
		{"OPENROUTER_BASE_URL", "ftp://openrouter.ai/api/v1"},
		{"DARKBLOOM_BASE_URL", "/v1"},
		{"DARKBLOOM_BASE_URL", "https://api.darkbloom.dev/v1?x=1"},
	} {
		t.Setenv("OPENROUTER_BASE_URL", "")
		t.Setenv("DARKBLOOM_BASE_URL", "")
		t.Setenv(tc.env, tc.value)
		if _, err := EnvClient(&http.Client{}); err == nil {
			t.Errorf("EnvClient accepted %s=%q", tc.env, tc.value)
		}
	}
}
