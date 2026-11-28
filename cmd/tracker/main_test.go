package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/pipeline"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// telegramSender and updatePresets build their requests against the real
// API roots (api.telegram.org, openrouter.ai), so the tests intercept at the
// transport: stubAPI serves every request from one httptest server after the
// transport rewrites its URL to it, dispatching by method and path. The
// original URL, headers and body are recorded for assertions.

type stubReply struct {
	status int
	body   string
}

type stubReq struct {
	method string
	url    string
	hdr    http.Header
	body   []byte
}

// stubAPI is the scripted API surface: it records requests and answers from
// script (keys "METHOD path"), defaulting to a 404.
type stubAPI struct {
	mu     sync.Mutex
	script map[string]stubReply
	got    []stubReq
	srv    *httptest.Server
	base   http.RoundTripper
}

func newStubAPI(t *testing.T, script map[string]stubReply) (*stubAPI, *http.Client) {
	t.Helper()
	s := &stubAPI{script: script, base: http.DefaultTransport}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		_ = r.Body.Close()
		s.mu.Lock()
		s.got = append(s.got, stubReq{method: r.Method, url: r.URL.String(), hdr: r.Header.Clone(), body: raw})
		rep, ok := s.script[r.Method+" "+r.URL.Path]
		s.mu.Unlock()
		if !ok {
			rep = stubReply{status: http.StatusNotFound, body: `{"error":"not scripted"}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(s.srv.Close)
	return s, &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		rr := r.Clone(r.Context())
		rr.Body = r.Body // Clone does not carry the body
		rr.URL.Scheme = "http"
		rr.URL.Host = s.srv.Listener.Addr().String()
		return s.base.RoundTrip(rr)
	})}
}

// called reports how many requests the stub saw.
func (s *stubAPI) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// captureLog redirects the standard logger into a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// telegramToken is built at runtime so no credential-shaped literal exists
// in the source; its value only has to be a stable, leak-detectable string.
var telegramToken = strings.Repeat("tok", 3) + "-secret"

func TestTelegramSenderNoReportModeSkipsHTTP(t *testing.T) {
	t.Setenv("TRACKER_REPORT", "false")
	s, hc := newStubAPI(t, nil)
	send, err := telegramSender(hc)
	if err != nil {
		t.Fatalf("telegramSender: %v", err)
	}
	if err := send("must not be sent"); err != nil {
		t.Errorf("send: %v", err)
	}
	if s.requests() != 0 {
		t.Errorf("presets-only run issued %d Telegram requests, want none", s.requests())
	}
}

func TestTelegramSenderRequiresSecrets(t *testing.T) {
	tests := []struct {
		name  string
		token bool
		chat  bool
	}{
		{"no secrets", false, false},
		{"token only", true, false},
		{"chat only", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TRACKER_REPORT", "true")
			if tt.token {
				t.Setenv("TELEGRAM_BOT_TOKEN", telegramToken)
			}
			if tt.chat {
				t.Setenv("TELEGRAM_CHANNEL_ID", "@admech-report")
			}
			_, hc := newStubAPI(t, nil)
			_, err := telegramSender(hc)
			if err == nil {
				t.Fatal("telegramSender: nil error, want the missing-secrets error")
			}
			if !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN and TELEGRAM_CHANNEL_ID are required") {
				t.Errorf("error = %q, want the missing-secrets message", err)
			}
		})
	}
}

func TestTelegramSenderPostsHTMLReport(t *testing.T) {
	t.Setenv("TRACKER_REPORT", "true")
	t.Setenv("TELEGRAM_BOT_TOKEN", telegramToken)
	t.Setenv("TELEGRAM_CHANNEL_ID", "@admech-report")
	s, hc := newStubAPI(t, map[string]stubReply{
		"POST /bot" + telegramToken + "/sendMessage": {status: http.StatusOK, body: `{"ok":true}`},
	})
	send, err := telegramSender(hc)
	if err != nil {
		t.Fatalf("telegramSender: %v", err)
	}
	if err := send("hello report"); err != nil {
		t.Fatalf("send: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) != 1 {
		t.Fatalf("requests = %d, want 1", len(s.got))
	}
	got := s.got[0]
	if got.method != http.MethodPost || !strings.HasSuffix(got.url, "/bot"+telegramToken+"/sendMessage") {
		t.Errorf("request %s %s, want POST .../bot<token>/sendMessage", got.method, got.url)
	}
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("request body %q: %v", got.body, err)
	}
	if body["chat_id"] != "@admech-report" || body["text"] != "hello report" || body["parse_mode"] != "html" {
		t.Errorf("body = %v, want chat_id, html text", body)
	}
}

// TestTelegramSenderRedactsSecretsInErrors: every failure carries the API's
// message to the caller (main logs it and exits nonzero) but never a secret
// — the bot token or the channel id, both of which the API body can echo.
func TestTelegramSenderRedactsSecretsInErrors(t *testing.T) {
	const chatID = "@admech-report"
	tests := []struct {
		name   string
		reply  stubReply
		status string
	}{
		{"api rejects", stubReply{status: http.StatusOK, body: `{"ok":false,"description":"Bad Request: chat not found ` + chatID + ` bot ` + telegramToken + `"}`}, "chat not found"},
		{"server error", stubReply{status: http.StatusInternalServerError, body: `{"error":"upstream exploded for ` + chatID + ` ` + telegramToken + `"}`}, "status 500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TRACKER_REPORT", "true")
			t.Setenv("TELEGRAM_BOT_TOKEN", telegramToken)
			t.Setenv("TELEGRAM_CHANNEL_ID", "@admech-report")
			_, hc := newStubAPI(t, map[string]stubReply{
				"POST /bot" + telegramToken + "/sendMessage": tt.reply,
			})
			send, err := telegramSender(hc)
			if err != nil {
				t.Fatalf("telegramSender: %v", err)
			}
			err = send("the day's report")
			if err == nil {
				t.Fatal("send: nil error, want the API failure returned so the run exits nonzero")
			}
			msg := err.Error()
			if strings.Contains(msg, telegramToken) || strings.Contains(msg, chatID) {
				t.Errorf("error = %q, want no secret (token or chat id) echoed", msg)
			}
			if got := strings.Count(msg, "REDACTED"); got != 2 {
				t.Errorf("error = %q, want both secrets redacted, got %d", msg, got)
			}
			if !strings.Contains(msg, tt.status) {
				t.Errorf("error = %q, want the API error body kept (%s)", msg, tt.status)
			}
		})
	}
}

// TestPiConfigDir: --pi-dir wins over $TRACKER_PIPRESETS; both empty leave
// the pi config surface off. There is no ~/.pi/agent default: run emits only
// what piConfigDir names.
func TestPiConfigDir(t *testing.T) {
	prev := *piDir
	t.Cleanup(func() { *piDir = prev })
	base := t.TempDir()

	*piDir = ""
	t.Setenv("TRACKER_PIPRESETS", "")
	if got := piConfigDir(); got != "" {
		t.Errorf("no flag, no env: dir = %q, want empty (pi config off)", got)
	}

	envDir := filepath.Join(base, "pi-from-env")
	t.Setenv("TRACKER_PIPRESETS", envDir)
	if got := piConfigDir(); got != envDir {
		t.Errorf("env dir = %q, want %q", got, envDir)
	}

	flagDir := filepath.Join(base, "pi-from-flag")
	*piDir = flagDir
	if got := piConfigDir(); got != flagDir {
		t.Errorf("flag dir = %q, want %q", got, flagDir)
	}
}

// TestPiConfigOptIn: without --pi-dir or $TRACKER_PIPRESETS run's gate skips
// the emit, so nothing is created and no $HOME is needed; with --pi-dir the
// emit writes both files into the flagged dir.
func TestPiConfigOptIn(t *testing.T) {
	prev := *piDir
	t.Cleanup(func() { *piDir = prev })
	base := t.TempDir()
	res := pipeline.Result{OpenRouter: []rank.SectorResult{{
		Sector: rank.Code,
		Rows:   []rank.Row{{ModelID: "vendor/discounted", Endpoint: "prov/tag", Price: 0.4, Was: 0.8, Pct: -50}},
	}}}

	*piDir = ""
	t.Setenv("TRACKER_PIPRESETS", "")
	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	if dir := piConfigDir(); dir != "" {
		t.Fatalf("unrequested dir = %q, want empty", dir)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("default run created a dir under $HOME: %v", err)
	}
	if entries, err := os.ReadDir(base); err != nil || len(entries) != 0 {
		t.Fatalf("default run touched %s: %v, %v", base, entries, err)
	}

	flagDir := filepath.Join(base, "pi-from-flag")
	*piDir = flagDir
	if err := emitPiConfig(piConfigDir(), res); err != nil {
		t.Fatalf("emitPiConfig: %v", err)
	}
	for _, name := range []string{"models.json", "settings.json"} {
		if _, err := os.Stat(filepath.Join(flagDir, name)); err != nil {
			t.Errorf("--pi-dir emit did not write %s: %v", name, err)
		}
	}
}

func TestLogDirect(t *testing.T) {
	rows := func(ids ...string) []rank.Row {
		out := make([]rank.Row, len(ids))
		for i, id := range ids {
			out[i] = rank.Row{ModelID: id, Scored: true}
		}
		return out
	}
	buf := captureLog(t)
	logDirect([]rank.SectorResult{
		{Sector: rank.Math, Rows: rows("m1", "m2", "m3", "m4", "m5")},
		{Sector: rank.HermesLight, Rows: rows("l1", "l2")},
		// unscored rows route nothing: preset.Picks drops them, so the log
		// must show an empty pick list, not the raw rows
		{Sector: rank.Finance, Rows: []rank.Row{{ModelID: "u1", Scored: false}}},
	})
	out := buf.String()
	want := []string{
		"darkbloom admech-math: m1, m2, m3",
		"darkbloom hermes-light: l1, l2",
		"darkbloom admech-finance: \n",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("log = %q, want the line %q", out, w)
		}
	}
	if strings.Contains(out, "m4") {
		t.Errorf("log = %q, want only the top-%d models logged", out, 3)
	}
}

func TestLogPicks(t *testing.T) {
	rows := func(ids ...string) []rank.Row {
		out := make([]rank.Row, len(ids))
		for i, id := range ids {
			out[i] = rank.Row{ModelID: id, Scored: true}
		}
		return out
	}
	buf := captureLog(t)
	logPicks([]rank.SectorResult{
		{Sector: rank.Math, Rows: rows("m1", "m2", "m3", "m4", "m5")},
		{Sector: rank.HermesLight, Rows: rows("l1", "l2")},
		// unscored rows route nothing: preset.Picks drops them, so the log
		// must show an empty pick list, not the raw rows
		{Sector: rank.Finance, Rows: []rank.Row{{ModelID: "u1", Scored: false}}},
	})
	out := buf.String()
	want := []string{
		"openrouter admech-math: m1, m2, m3",
		"openrouter hermes-light: l1, l2",
		"openrouter admech-finance: \n",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("log = %q, want the line %q", out, w)
		}
	}
	if strings.Contains(out, "m4") || strings.Contains(out, "u1") {
		t.Errorf("log = %q, want only the routed picks logged", out)
	}
}

func TestUpdatePresetsWithoutKeySkips(t *testing.T) {
	t.Setenv("OPENROUTER_MANAGEMENT_KEY", "")
	s, hc := newStubAPI(t, nil)
	buf := captureLog(t)
	if err := updatePresets(t.Context(), hc, []rank.SectorResult{{Sector: rank.Math}}); err != nil {
		t.Fatalf("updatePresets: %v", err)
	}
	if s.requests() != 0 {
		t.Errorf("without a key issued %d requests, want none", s.requests())
	}
	if !strings.Contains(buf.String(), "OPENROUTER_MANAGEMENT_KEY not set") {
		t.Errorf("log = %q, want the no-key notice", buf.String())
	}
}

// TestUpdatePresetsSkipsUnscored: a sector with no scored models keeps its
// preset, so nothing is fetched or posted.
func TestUpdatePresetsSkipsUnscored(t *testing.T) {
	t.Setenv("OPENROUTER_MANAGEMENT_KEY", "or-mgmt-key")
	s, hc := newStubAPI(t, nil)
	if err := updatePresets(t.Context(), hc, []rank.SectorResult{
		{Sector: rank.Math, Rows: []rank.Row{{ModelID: "v/a"}}}, // Scored: false
	}); err != nil {
		t.Fatalf("updatePresets: %v", err)
	}
	if s.requests() != 0 {
		t.Errorf("unscored sector issued %d requests, want none", s.requests())
	}
}

func TestUpdatePresetsPointsAtTopModels(t *testing.T) {
	t.Setenv("OPENROUTER_MANAGEMENT_KEY", "or-mgmt-key")
	s, hc := newStubAPI(t, map[string]stubReply{
		"GET /api/v1/presets/admech-code":                   {status: http.StatusNotFound, body: "{}"},
		"POST /api/v1/presets/admech-code/chat/completions": {status: http.StatusOK, body: "{}"},
	})
	sector := rank.SectorResult{Sector: rank.Code, Rows: []rank.Row{
		{ModelID: "v/a", Scored: true}, {ModelID: "v/b"},
	}}
	buf := captureLog(t)
	if err := updatePresets(t.Context(), hc, []rank.SectorResult{sector}); err != nil {
		t.Fatalf("updatePresets: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) != 2 {
		t.Fatalf("requests = %d, want the GET and the POST", len(s.got))
	}
	getReq := s.got[0]
	if getReq.hdr.Get("Authorization") != "Bearer or-mgmt-key" {
		t.Errorf("GET auth = %q, want the management key", getReq.hdr.Get("Authorization"))
	}
	post := s.got[1]
	var body map[string]any
	if err := json.Unmarshal(post.body, &body); err != nil {
		t.Fatalf("POST body %q: %v", post.body, err)
	}
	models, _ := body["models"].([]any)
	if len(models) != 1 || models[0] != "v/a" {
		t.Errorf("models = %v, want only the scored model", models)
	}
	if !strings.Contains(buf.String(), "preset admech-code: new version") {
		t.Errorf("log = %q, want the new-version notice", buf.String())
	}
}

// TestUpdatePresetsReportsFailures: every preset is attempted and the
// failures are joined, so the run exits nonzero naming the slug.
func TestUpdatePresetsReportsFailures(t *testing.T) {
	t.Setenv("OPENROUTER_MANAGEMENT_KEY", "or-mgmt-key")
	s, hc := newStubAPI(t, map[string]stubReply{
		"GET /api/v1/presets/admech-code":                          {status: http.StatusNotFound, body: "{}"},
		"GET /api/v1/presets/admech-code-oracle":                   {status: http.StatusNotFound, body: "{}"},
		"POST /api/v1/presets/admech-code/chat/completions":        {status: http.StatusOK, body: "{}"},
		"POST /api/v1/presets/admech-code-oracle/chat/completions": {status: http.StatusInternalServerError, body: `{"error":"boom"}`},
	})
	sectors := []rank.SectorResult{
		{Sector: rank.Code, Rows: []rank.Row{{ModelID: "v/a", Scored: true}}},
		{Sector: rank.AdmechOracle, Rows: []rank.Row{{ModelID: "v/b", Scored: true}}},
	}
	err := updatePresets(t.Context(), hc, sectors)
	if err == nil {
		t.Fatal("updatePresets: nil error, want the joined failure so the run exits nonzero")
	}
	msg := err.Error()
	if !strings.Contains(msg, "1 of 8 preset updates failed") || !strings.Contains(msg, "preset admech-code-oracle") {
		t.Errorf("error = %q, want the failed count and slug", msg)
	}
	if s.requests() != 4 {
		t.Errorf("requests = %d, want every preset attempted (2 GET, 2 POST)", s.requests())
	}
	if !strings.Contains(msg, "boom") {
		t.Errorf("error = %q, want the failing preset's error body kept", msg)
	}
}

// TestUpdatePresetsOnlyManagedSlugs: a run carrying every report and legacy
// preset sector still writes only the seven preset.ManagedSlugs entries.
func TestUpdatePresetsOnlyManagedSlugs(t *testing.T) {
	t.Setenv("OPENROUTER_MANAGEMENT_KEY", "or-mgmt-key")
	script := map[string]stubReply{}
	for _, slug := range preset.ManagedSlugs() {
		script["GET /api/v1/presets/"+slug] = stubReply{status: http.StatusNotFound, body: "{}"}
		script["POST /api/v1/presets/"+slug+"/chat/completions"] = stubReply{status: http.StatusOK, body: "{}"}
	}
	s, hc := newStubAPI(t, script)
	managed := map[rank.Sector]bool{
		rank.Code: true, rank.AdmechOracle: true, rank.AdmechReviewer: true,
		rank.AdmechResearcher: true, rank.AdmechAuditor: true,
		rank.AdmechDelegate: true, rank.AdmechScout: true, rank.AdmechVision: true,
	}
	sectors := []rank.SectorResult{
		{Sector: rank.Math, Rows: []rank.Row{{ModelID: "v/math", Scored: true}}},
		{Sector: rank.Finance, Rows: []rank.Row{{ModelID: "v/fin", Scored: true}}},
		{Sector: rank.ExpensiveLong, Rows: []rank.Row{{ModelID: "v/long", Scored: true}}},
		{Sector: rank.HermesLight, Rows: []rank.Row{{ModelID: "v/light", Scored: true}}},
		{Sector: rank.Code, Rows: []rank.Row{{ModelID: "v/code", Scored: true}}},
		{Sector: rank.AdmechOracle, Rows: []rank.Row{{ModelID: "v/oracle", Scored: true}}},
		{Sector: rank.AdmechReviewer, Rows: []rank.Row{{ModelID: "v/reviewer", Scored: true}}},
		{Sector: rank.AdmechResearcher, Rows: []rank.Row{{ModelID: "v/researcher", Scored: true}}},
		{Sector: rank.AdmechAuditor, Rows: []rank.Row{{ModelID: "v/auditor", Scored: true}}},
		{Sector: rank.AdmechDelegate, Rows: []rank.Row{{ModelID: "v/delegate", Scored: true}}},
		{Sector: rank.AdmechScout, Rows: []rank.Row{{ModelID: "v/scout", Scored: true}}},
		{Sector: rank.AdmechVision, Rows: []rank.Row{{ModelID: "v/vision", Scored: true}}},
	}
	if err := updatePresets(t.Context(), hc, sectors); err != nil {
		t.Fatalf("updatePresets: %v", err)
	}
	allow := make(map[string]bool, len(managed))
	for sec := range managed {
		allow[preset.Slugs[sec]] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	posted := map[string]bool{}
	for _, req := range s.got {
		if req.method != http.MethodPost {
			continue
		}
		slug, ok := strings.CutPrefix(req.url, "/api/v1/presets/")
		slug, ok2 := strings.CutSuffix(slug, "/chat/completions")
		if !ok || !ok2 || !allow[slug] {
			t.Errorf("POST %s is outside preset.ManagedSlugs()", req.url)
			continue
		}
		posted[slug] = true
	}
	if len(posted) != len(managed) {
		t.Errorf("posted slugs = %v, want exactly the %d managed slugs", posted, len(managed))
	}
}
