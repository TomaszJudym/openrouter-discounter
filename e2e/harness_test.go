package e2e

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// binDir holds the repo binaries built once for the suite.
var binDir string

// TestMain builds cmd/pipresets and cmd/dbproxy from the repo, so the
// suite drives the real entry points rather than reimplementing them.
func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "discounter-e2e-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for _, name := range []string{"pipresets", "dbproxy"} {
		build := exec.CommandContext(context.Background(), "go", "build", "-o", filepath.Join(dir, name), "./cmd/"+name) //nolint:gosec // G204: fixed repo package paths
		build.Dir = ".."
		build.Env = append(os.Environ(), "GOTOOLCHAIN=local")
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", name, err, out)
			return 1
		}
	}
	binDir = dir
	return m.Run()
}

// scrubPrefixes are dropped from the inherited environment: a suite started
// from inside a pi session would otherwise hand its PI_* session markers to
// pi-subagents (PI_SUBAGENT_CHILD disables delegation) and its proxies to
// every child.
var scrubPrefixes = []string{
	"PI_", "AI_AGENT=", "OPENROUTER_", "DARKBLOOM_", "AA_API_KEY=",
	"HTTP_PROXY=", "HTTPS_PROXY=", "http_proxy=", "https_proxy=",
	"NO_PROXY=", "no_proxy=", "SSL_CERT_FILE=",
}

// cleanEnv returns the inherited environment minus the scrubbed keys, plus
// extra. Loopback is always excluded from proxying: the stubs must be
// reached directly.
func cleanEnv(extra map[string]string) []string {
	out := make([]string, 0, len(os.Environ())+len(extra)+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ALL_PROXY=") || strings.HasPrefix(kv, "all_proxy=") {
			continue
		}
		skip := false
		for _, p := range scrubPrefixes {
			if strings.HasPrefix(kv, p) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// guard is the suite's only route to a non-loopback host: a local CONNECT
// proxy that terminates TLS with its own CA. It serves the two score
// sources the pipeline needs (the Epoch AI benchmark zip and the
// OpenRouter rankings feed) and answers 404 for every other host, so the
// suite needs no network and the preset rows come out scored — cmd/dbproxy
// only routes scored rows (preset.Picks).
type guard struct {
	srv   *httptest.Server
	caPEM string
	mu    sync.Mutex
	got   map[string]int
	certs map[string]*tls.Certificate
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	zip   []byte
}

func newGuard(t *testing.T) *guard {
	t.Helper()
	g := &guard{got: map[string]int{}, certs: map[string]*tls.Certificate{}, zip: epochZip(t)}
	g.ca, g.caKey = newCA(t)
	g.caPEM = filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.ca.Raw})
	if err := os.WriteFile(g.caPEM, pemBytes, 0o600); err != nil {
		t.Fatalf("write test CA: %v", err)
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *guard) url() string { return g.srv.URL }

func (g *guard) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		g.tunnel(w, r)
		return
	}
	g.record(r.Host, r.URL.Path)
	status, ctype, body := g.answer(r.Host, r.URL.Path)
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// tunnel answers one CONNECT, then speaks TLS on the hijacked connection
// with a certificate for the requested host.
func (g *guard) tunnel(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = conn.Close()
		return
	}
	if err := bufrw.Flush(); err != nil {
		_ = conn.Close()
		return
	}
	cert, err := g.certFor(host)
	if err != nil {
		_ = conn.Close()
		return
	}
	tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if err := tlsConn.HandshakeContext(r.Context()); err != nil {
		_ = conn.Close()
		return
	}
	defer func() { _ = tlsConn.Close() }()
	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		g.record(host, req.URL.Path)
		status, ctype, body := g.answer(host, req.URL.Path)
		header := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: keep-alive\r\n\r\n",
			status, http.StatusText(status), ctype, len(body))
		if _, err := tlsConn.Write(append([]byte(header), body...)); err != nil {
			return
		}
		if req.Close {
			return
		}
	}
}

// answer is the whole upstream surface the pipeline reaches off loopback.
func (g *guard) answer(host, path string) (status int, ctype string, body []byte) {
	switch {
	case host == "epoch.ai" && path == "/data/benchmark_data.zip":
		return http.StatusOK, "application/zip", g.zip
	case host == "openrouter.ai" && path == "/api/frontend/v1/rankings/models":
		return http.StatusOK, "application/json", []byte(`{"data":[{"model_permaslug":"` + orModelID + `","variant":"standard","rankingMetricValue":1000}]}`)
	default:
		return http.StatusNotFound, "application/json", []byte(`{"error":"the e2e suite serves no such host"}`)
	}
}

func (g *guard) record(host, path string) {
	g.mu.Lock()
	g.got[host+path]++
	g.mu.Unlock()
}

// certFor signs (and caches) a leaf certificate for one proxied host.
func (g *guard) certFor(host string) (tls.Certificate, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.certs[host]; ok {
		return *c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, g.ca, &key.PublicKey, g.caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	g.certs[host] = &cert
	return cert, nil
}

func newCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "openrouter-discounter e2e guard"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return cert, key
}

// epochZip is a minimal Epoch AI bulk download: one model rated in both
// benchmark files, so Math and Code/CodeFree come out scored.
func epochZip(t *testing.T) []byte {
	t.Helper()
	files := map[string]string{
		"otis_mock_aime_2024_2025.csv": "Model version,mean_score\n" + orModelID + ",0.8\n",
		"webdev_arena_external.csv":    "Model version,Arena Score\n" + orModelID + ",1200\n",
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// scenario is one stub pair plus the environment the repo binaries run in.
type scenario struct {
	t        *testing.T
	or       *orStub
	db       *dbStub
	guard    *guard
	env      []string
	agentDir string
	workDir  string
}

func newScenario(t *testing.T, orp, dbp price) *scenario {
	t.Helper()
	or := newORStub(t, orp)
	db := newDBStub(t, dbp)
	g := newGuard(t)
	root := t.TempDir()
	s := &scenario{
		t: t, or: or, db: db, guard: g,
		agentDir: filepath.Join(root, "agent"),
		workDir:  filepath.Join(root, "work"),
	}
	for _, dir := range []string{s.agentDir, s.workDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	s.env = cleanEnv(map[string]string{
		"OPENROUTER_API_KEY":  "test-openrouter-key",
		"DARKBLOOM_API_KEY":   "test-darkbloom-key",
		"OPENROUTER_BASE_URL": or.url() + "/api/v1",
		"DARKBLOOM_BASE_URL":  db.url() + "/v1",
		"HTTP_PROXY":          g.url(),
		"HTTPS_PROXY":         g.url(),
		"NO_PROXY":            "127.0.0.1,localhost",
		"SSL_CERT_FILE":       g.caPEM,
	})
	return s
}

func (s *scenario) runPipresets() string {
	s.t.Helper()
	cmd := exec.CommandContext(context.Background(), filepath.Join(binDir, "pipresets"), "--dir", s.agentDir) //nolint:gosec // G204: paths built by the test
	cmd.Env = s.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("pipresets: %v\n%s", err, out)
	}
	return string(out)
}

func (s *scenario) readJSON(name string) map[string]any {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.agentDir, name)) //nolint:gosec // G304: path under t.TempDir
	if err != nil {
		s.t.Fatalf("read %s: %v", name, err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		s.t.Fatalf("parse %s: %v\n%s", name, err, b)
	}
	return v
}

// agentOverride is settings.subagents.agentOverrides[agent].model.
func (s *scenario) agentOverride(agent string) string {
	s.t.Helper()
	settings := s.readJSON("settings.json")
	subagents, _ := settings["subagents"].(map[string]any)
	overrides, _ := subagents["agentOverrides"].(map[string]any)
	entry, _ := overrides[agent].(map[string]any)
	model, _ := entry["model"].(string)
	return model
}

// provider returns providers[name] of models.json.
func (s *scenario) provider(name string) map[string]any {
	s.t.Helper()
	models := s.readJSON("models.json")
	providers, _ := models["providers"].(map[string]any)
	prov, _ := providers[name].(map[string]any)
	if prov == nil {
		s.t.Fatalf("models.json has no %q provider", name)
	}
	return prov
}

func (s *scenario) providerModelIDs(name string) []string {
	s.t.Helper()
	prov := s.provider(name)
	models, _ := prov["models"].([]any)
	ids := make([]string, 0, len(models))
	for _, m := range models {
		entry, _ := m.(map[string]any)
		id, _ := entry["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// proxyProc is a running cmd/dbproxy.
type proxyProc struct {
	url  string
	logs *syncBuffer
	stop func()
}

// startProxy runs cmd/dbproxy against the stubs on its own loopback port.
func (s *scenario) startProxy(refresh string) *proxyProc {
	s.t.Helper()
	addr := reserveAddr(s.t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "dbproxy")) //nolint:gosec // G204: path built by the test
	cmd.Env = append(append([]string{}, s.env...), "DBPROXY_ADDR="+addr, "DBPROXY_REFRESH="+refresh)
	logs := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start dbproxy: %v", err)
	}
	s.t.Cleanup(func() {
		cancel()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	p := &proxyProc{url: "http://" + addr, logs: logs, stop: cancel}
	s.waitUp(p)
	return p
}

func (s *scenario) waitUp(p *proxyProc) {
	s.t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, p.url+"/v1/models", nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatalf("dbproxy did not come up on %s\n%s", p.url, p.logs.String())
}

// chatRoute posts one completion for model through the proxy and reports the
// backend that served it (dbproxy sets X-Dbproxy-Route).
func (s *scenario) chatRoute(p *proxyProc, model string) (int, string, error) {
	body := `{"model":` + strconvQuote(model) + `,"messages":[{"role":"user","content":"ping"}]}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, p.url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Dbproxy-Route"), nil
}

// waitRoute polls until the proxy serves model via the backend whose route
// name starts with want ("darkbloom/" or "openrouter").
func (s *scenario) waitRoute(p *proxyProc, model, want string, timeout time.Duration) string {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		status, route, err := s.chatRoute(p, model)
		if err == nil && status == http.StatusOK {
			last = route
			if strings.HasPrefix(route, want) {
				return route
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatalf("route %q for %s not reached within %s (last %q)\n%s", want, model, timeout, last, p.logs.String())
	return ""
}

// patchForPi makes the emitted agent dir runnable against the stubs:
// models.json's production base URLs (https://openrouter.ai/api/v1 and the
// documented loopback proxy) point at the test servers, and settings.json
// gains the parent session's default model, which pipresets does not write.
// The surface there is the one under test (the worker preset's winner), so
// the parent's own traffic never masks a routing bug in the oracle override.
func (s *scenario) patchForPi(surface, proxyURL string) {
	s.t.Helper()
	path := filepath.Join(s.agentDir, "models.json")
	b, err := os.ReadFile(path) //nolint:gosec // G304: path under t.TempDir
	if err != nil {
		s.t.Fatalf("read models.json: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		s.t.Fatalf("parse models.json: %v", err)
	}
	providers, _ := root["providers"].(map[string]any)
	if prov, ok := providers["dbproxy"].(map[string]any); ok {
		prov["baseUrl"] = proxyURL + "/v1"
	}
	if prov, ok := providers["openrouter"].(map[string]any); ok {
		prov["baseUrl"] = s.or.url() + "/api/v1"
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		s.t.Fatalf("render models.json: %v", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		s.t.Fatalf("write models.json: %v", err)
	}
	s.patchSettings(surface)
}

// patchSettings adds the parent session model to the emitted settings.json.
func (s *scenario) patchSettings(surface string) {
	s.t.Helper()
	path := filepath.Join(s.agentDir, "settings.json")
	b, err := os.ReadFile(path) //nolint:gosec // G304: path under t.TempDir
	if err != nil {
		s.t.Fatalf("read settings.json: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		s.t.Fatalf("parse settings.json: %v", err)
	}
	root["defaultModel"] = presetWorker
	root["defaultProvider"] = surface
	// The emitted settings.json only carries subagents.agentOverrides; the
	// test agent dir still has to declare the extension that owns them.
	if _, ok := root["packages"]; !ok {
		root["packages"] = []any{"npm:pi-subagents"}
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		s.t.Fatalf("render settings.json: %v", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		s.t.Fatalf("write settings.json: %v", err)
	}
}

// piEvent is one JSONL record of pi --mode json.
type piEvent struct {
	Type    string `json:"type"`
	Message struct {
		Role     string          `json:"role"`
		Provider string          `json:"provider"`
		Model    string          `json:"model"`
		Content  json.RawMessage `json:"content"`
	} `json:"message"`
	ToolName string `json:"toolName"`
	IsError  bool   `json:"isError"`
	// Result is the subagent tool's answer: the child's own turns are not
	// part of pi's root JSON stream, so this text is where the child's
	// model surface shows up.
	Result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

// piPath is the pi binary, or "" when the suite must skip the opt-in pi
// subtest.
func piPath() string {
	p, err := exec.LookPath("pi")
	if err != nil {
		return ""
	}
	return p
}

// npmDir is the pi package directory carrying pi-subagents.
func npmDir() string {
	if v := os.Getenv("PI_NPM_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "npm")
}

// runPi runs one headless pi turn against the scenario's agent dir and
// returns its JSONL events. It links the real npm package directory into the
// temp agent dir (pi-subagents is not copied, only read).
func (s *scenario) runPi(prompt string) []piEvent {
	s.t.Helper()
	bin := piPath()
	if bin == "" {
		s.t.Skip("pi binary not found")
	}
	npm := npmDir()
	if _, err := os.Stat(filepath.Join(npm, "node_modules", "pi-subagents")); err != nil {
		s.t.Skipf("pi-subagents not installed under %s: %v", npm, err)
	}
	if err := os.Symlink(npm, filepath.Join(s.agentDir, "npm")); err != nil {
		s.t.Fatalf("link npm dir: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--mode", "json", "--print", prompt) //nolint:gosec // G204: pi from PATH, fixed flags
	cmd.Dir = s.workDir
	cmd.Env = append(append([]string{}, s.env...),
		"PI_CODING_AGENT_DIR="+s.agentDir,
		"PI_OFFLINE=1",
		"PI_SKIP_VERSION_CHECK=1",
	)
	var stdout, stderr syncBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		s.t.Fatalf("pi: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
	}
	events := make([]piEvent, 0, 64)
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev piEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			s.t.Fatalf("parse pi event: %v\n%s", err, line)
		}
		events = append(events, ev)
	}
	return events
}

// reserveAddr returns a free loopback host:port.
func reserveAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

// syncBuffer is a concurrency-safe bytes.Buffer for child output.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
