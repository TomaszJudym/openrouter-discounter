package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/pipeline"
	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

const (
	dbKey = "sk-db-proxy-secret"
	orKey = "sk-or-client-key"
)

// upstream is a scripted backend: per model, the status and body to answer;
// it records every request.
type upstream struct {
	mu     sync.Mutex
	srv    *httptest.Server
	script map[string]reply
	got    []recorded
}

type reply struct {
	status int
	ctype  string
	body   string
}

type recorded struct {
	auth string
	body map[string]any
}

func newUpstream(t *testing.T, script map[string]reply) *upstream {
	t.Helper()
	u := &upstream{script: script}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.got = append(u.got, recorded{auth: r.Header.Get("Authorization"), body: body})
		u.mu.Unlock()
		model, _ := body["model"].(string)
		rep, ok := u.script[model]
		if !ok {
			rep = reply{status: http.StatusNotFound, body: `{"error":{"code":"model_not_found"}}`}
		}
		w.Header().Set("Content-Type", cmpOr(rep.ctype, "application/json"))
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (u *upstream) models() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, len(u.got))
	for i, g := range u.got {
		out[i], _ = g.body["model"].(string)
	}
	return out
}

func newProxy(t *testing.T, or, db *upstream, rt routes) *Proxy {
	t.Helper()
	p := &Proxy{HC: http.DefaultClient, OpenRouter: or.srv.URL, Darkbloom: db.srv.URL, DarkbloomKey: dbKey}
	p.SetRoutes(rt)
	return p
}

func post(t *testing.T, p *Proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+orKey)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

const okJSON = `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_time","arguments":"{\"city\":\"Tokyo\"}"}}]}}]}`

const toolRequest = `{"model":"@preset/strongest-discount-volition","stream":false,` +
	`"messages":[{"role":"system","content":"You are Hermes."},{"role":"user","content":"time in Tokyo?"}],` +
	`"tools":[{"type":"function","function":{"name":"get_time","parameters":{"type":"object"}}}],` +
	`"tool_choice":"auto","temperature":0.3,"provider":{"only":["x"]},"models":["a","b"]}`

func TestDarkbloomPickGetsPersonaAndTools(t *testing.T) {
	or := newUpstream(t, nil)
	db := newUpstream(t, map[string]reply{"m1": {status: 200, body: okJSON}})
	p := newProxy(t, or, db, routes{"strongest-discount-volition": {direct: []string{"m1"}, dbFirst: true}})
	rec := post(t, p, toolRequest)
	if rec.Code != 200 || rec.Header().Get(routeHeader) != "darkbloom/m1" || rec.Body.String() != okJSON {
		t.Fatalf("got %d %q %s", rec.Code, rec.Header().Get(routeHeader), rec.Body)
	}
	if len(or.got) != 0 {
		t.Errorf("OpenRouter called: %v", or.models())
	}
	got := db.got[0]
	if got.auth != "Bearer "+dbKey {
		t.Errorf("Darkbloom auth = %q, want the proxy's key (never the client's)", got.auth)
	}
	msgs := got.body["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	if len(msgs) != 2 || !strings.Contains(sys, "You're Volition") || !strings.HasSuffix(sys, "\n\nYou are Hermes.") {
		t.Errorf("messages = %v, want the persona merged before the client's system message", msgs)
	}
	checkParams(t, got.body)
}

// checkParams: client params win, preset params fill the rest, OpenRouter
// routing and min_p are dropped, tools pass through.
func checkParams(t *testing.T, body map[string]any) {
	t.Helper()
	if body["temperature"] != 0.3 {
		t.Errorf("temperature = %v, want the client's 0.3", body["temperature"])
	}
	if body["top_p"] != 0.95 || body["max_tokens"] != float64(rank.PersonaMaxTokens) {
		t.Errorf("preset params missing: top_p %v, max_tokens %v", body["top_p"], body["max_tokens"])
	}
	for _, k := range []string{"min_p", "provider", "models"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s sent to Darkbloom", k)
		}
	}
	if _, ok := body["tools"].([]any); !ok || body["tool_choice"] != "auto" {
		t.Errorf("tools not passed through: %v / %v", body["tools"], body["tool_choice"])
	}
}

func TestFallbackWalksDarkbloomThenOpenRouter(t *testing.T) {
	or := newUpstream(t, map[string]reply{"@preset/hermes-delegate": {status: 200, body: okJSON}})
	db := newUpstream(t, map[string]reply{
		"m1": {status: http.StatusServiceUnavailable, body: `{"error":{"code":"model_unavailable"}}`},
		"m2": {status: http.StatusTooManyRequests, body: `{"error":{"code":"rate_limit_exceeded"}}`},
		// m3 unscripted: 404 model_not_found
	})
	p := newProxy(t, or, db, routes{"hermes-delegate": {direct: []string{"m1", "m2", "m3"}, dbFirst: true}})
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	rec := post(t, p, `{"model":"@preset/hermes-delegate","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || rec.Header().Get(routeHeader) != "openrouter" {
		t.Fatalf("got %d via %q, want 200 via openrouter", rec.Code, rec.Header().Get(routeHeader))
	}
	if got := strings.Join(db.models(), ","); got != "m1,m2,m3" {
		t.Errorf("Darkbloom tried %s, want m1,m2,m3", got)
	}
	if or.got[0].auth != "Bearer "+orKey || or.got[0].body["model"] != "@preset/hermes-delegate" {
		t.Errorf("OpenRouter got %+v, want the preset with the client's key", or.got[0])
	}
	if strings.Contains(logs.String(), dbKey) || strings.Contains(logs.String(), orKey) {
		t.Error("a key reached the log")
	}
}

func TestOpenRouterFirstThenDarkbloom(t *testing.T) {
	or := newUpstream(t, map[string]reply{"@preset/hermes-light": {status: http.StatusBadGateway, body: `{"error":{"message":"no endpoints"}}`}})
	db := newUpstream(t, map[string]reply{"m1": {status: 200, body: okJSON}})
	p := newProxy(t, or, db, routes{"hermes-light": {direct: []string{"m1"}}})
	rec := post(t, p, `{"model":"@preset/hermes-light","messages":[{"role":"user","content":"title?"}]}`)
	if rec.Header().Get(routeHeader) != "darkbloom/m1" || len(or.got) != 1 {
		t.Fatalf("route %q, OpenRouter calls %d; want OpenRouter then darkbloom/m1", rec.Header().Get(routeHeader), len(or.got))
	}
	// hermes-light carries no prompt: messages pass unchanged
	if msgs := db.got[0].body["messages"].([]any); len(msgs) != 1 {
		t.Errorf("messages = %v, want the client's only", msgs)
	}
}

func TestLastFailureIsRelayed(t *testing.T) {
	or := newUpstream(t, map[string]reply{"@preset/hermes-light": {status: http.StatusPaymentRequired, body: `{"error":{"message":"credits"}}`}})
	db := newUpstream(t, nil)
	p := newProxy(t, or, db, routes{"hermes-light": {direct: []string{"m1"}, dbFirst: true}})
	rec := post(t, p, `{"model":"@preset/hermes-light","messages":[]}`)
	if rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), "credits") {
		t.Errorf("got %d %s, want OpenRouter's 402 relayed", rec.Code, rec.Body)
	}
}

func TestStreamErrorEventFallsBackButContentCommits(t *testing.T) {
	sseErr := "data: {\"error\":{\"message\":\"provider died\"}}\n\n"
	sseOK := ": keepalive\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"get_time\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	or := newUpstream(t, nil)
	db := newUpstream(t, map[string]reply{
		"m1": {status: 200, ctype: "text/event-stream", body: sseErr},
		"m2": {status: 200, ctype: "text/event-stream", body: sseOK},
	})
	p := newProxy(t, or, db, routes{"hermes-delegate": {direct: []string{"m1", "m2"}, dbFirst: true}})
	rec := post(t, p, `{"model":"@preset/hermes-delegate","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Header().Get(routeHeader) != "darkbloom/m2" || rec.Body.String() != sseOK {
		t.Fatalf("route %q body %q, want m2's stream intact", rec.Header().Get(routeHeader), rec.Body)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("content type %q", rec.Header().Get("Content-Type"))
	}
	if len(or.got) != 0 {
		t.Error("OpenRouter called after a committed stream")
	}
}

func TestNonPresetPassesThrough(t *testing.T) {
	or := newUpstream(t, map[string]reply{"openai/gpt-5.2": {status: 200, body: okJSON}})
	db := newUpstream(t, nil)
	p := newProxy(t, or, db, routes{})
	raw := `{"model":"openai/gpt-5.2","messages":[{"role":"user","content":"hi"}],"provider":{"only":["x"]}}`
	rec := post(t, p, raw)
	if rec.Code != 200 || len(db.got) != 0 {
		t.Fatalf("got %d, Darkbloom calls %d", rec.Code, len(db.got))
	}
	if _, ok := or.got[0].body["provider"]; !ok {
		t.Error("OpenRouter body altered")
	}
	// a preset without a Darkbloom side goes to OpenRouter as is
	rec = post(t, p, `{"model":"@preset/admech-math","messages":[]}`)
	if or.models()[1] != "@preset/admech-math" || rec.Code != http.StatusNotFound {
		t.Errorf("unrouted preset: %v %d", or.models(), rec.Code)
	}
}

func TestSystemPromptAddedWithoutClientSystem(t *testing.T) {
	out, err := darkbloomBody(map[string]jsontext.Value{
		"model":    []byte(`"@preset/x"`),
		"messages": []byte(`[{"role":"user","content":[{"type":"text","text":"hi"}]}]`),
	}, "m1", "PERSONA", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "m1" || len(body.Messages) != 2 || body.Messages[0]["content"] != "PERSONA" {
		t.Errorf("body = %+v", body)
	}
	raw, err := prependText([]byte(`[{"type":"text","text":"client"}]`), "PERSONA")
	if err != nil {
		t.Fatal(err)
	}
	var parts []map[string]string
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) != 2 || parts[0]["text"] != "PERSONA" || parts[1]["text"] != "client" {
		t.Errorf("prependText parts = %s, %v", raw, err)
	}
}

func TestModelsListsPresets(t *testing.T) {
	p := &Proxy{}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
	if !strings.Contains(rec.Body.String(), `"@preset/hermes-delegate"`) {
		t.Errorf("models = %s", rec.Body)
	}
}

func TestBuildRoutesCheaperSideFirst(t *testing.T) {
	row := func(id, ep string, price float64) rank.Row {
		return rank.Row{ModelID: id, Endpoint: ep, Serves: []string{ep}, Price: price, Scored: true, Score: 50}
	}
	res := pipeline.Result{
		OpenRouter: []rank.SectorResult{
			{Sector: rank.HermesDelegate, Rows: []rank.Row{row("v/a", "darkbloom/fp4", 0.05)}},
			{Sector: rank.HermesReview, Rows: []rank.Row{row("v/b", "cheap/bf16", 0.01)}},
		},
		Direct: []rank.SectorResult{
			{Sector: rank.HermesDelegate, Rows: []rank.Row{
				row("v/a", discounts.DirectPrefix+"a-4bit", 0.05),
				row("v/b", discounts.DirectPrefix+"b", 0.06),
				row("v/c", discounts.DirectPrefix+"c", 0.07),
				row("v/d", discounts.DirectPrefix+"d", 0.08),
			}},
			{Sector: rank.HermesReview, Rows: []rank.Row{row("v/b", discounts.DirectPrefix+"b", 0.06)}},
			{Sector: rank.HermesLight, Rows: []rank.Row{row("v/b", discounts.DirectPrefix+"b", 0.06)}},
		},
	}
	rt := buildRoutes(res)
	if d := rt["hermes-delegate"]; !d.dbFirst || strings.Join(d.direct, ",") != "a-4bit,b,c" {
		t.Errorf("delegate = %+v, want darkbloom first (tie), top 3", d)
	}
	if r := rt["hermes-review"]; r.dbFirst {
		t.Errorf("review = %+v, want openrouter first (cheaper)", r)
	}
	if l := rt["hermes-light"]; !l.dbFirst {
		t.Errorf("light = %+v, want darkbloom first (no OpenRouter pick)", l)
	}
}

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	m.Run()
}
