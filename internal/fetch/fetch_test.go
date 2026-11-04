package fetch

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestGetRetriesTransient(t *testing.T) {
	var calls int
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	g := Getter{HC: srv.Client(), Attempts: 3, Backoff: time.Millisecond}
	body, err := g.Get(t.Context(), srv.URL, nil, "")
	if err != nil || string(body) != "ok" || calls != 3 {
		t.Errorf("Get = %q, %v after %d calls; want ok after 3", body, err, calls)
	}
}

func TestGetNoRetryOnClientError(t *testing.T) {
	var calls int
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	g := Getter{HC: srv.Client(), Attempts: 3, Backoff: time.Millisecond}
	if _, err := g.Get(t.Context(), srv.URL, nil, ""); err == nil || calls != 1 {
		t.Errorf("err = %v after %d calls, want error after 1", err, calls)
	}
}

func TestGetSendsHeaderAndRedacts(t *testing.T) {
	const secret = "s3cret"
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != secret {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("echo " + secret))
	}))
	g := Getter{HC: srv.Client(), Attempts: 2, Backoff: time.Millisecond}
	_, err := g.Get(t.Context(), srv.URL, http.Header{"X-Api-Key": {secret}}, secret)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "echo REDACTED") {
		t.Errorf("err = %v, want retries exhausted with secret redacted", err)
	}
}

func TestGetRejectsOversizeBody(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 11)))
	}))
	g := Getter{HC: srv.Client(), Attempts: 1, MaxBytes: 10}
	if _, err := g.Get(t.Context(), srv.URL, nil, ""); err == nil || !strings.Contains(err.Error(), "exceeds 10 bytes") {
		t.Errorf("err = %v, want size error", err)
	}
	g.MaxBytes = 11
	if body, err := g.Get(t.Context(), srv.URL, nil, ""); err != nil || len(body) != 11 {
		t.Errorf("Get at limit = %d bytes, %v; want 11, nil", len(body), err)
	}
}

func TestSanitizeTruncates(t *testing.T) {
	got := Sanitize(strings.Repeat("é", 200), "")
	if n := utf8.RuneCountInString(got); n != MaxErrRunes || !strings.HasSuffix(got, "…") {
		t.Errorf("Sanitize length = %d, want %d ending in …", n, MaxErrRunes)
	}
}
