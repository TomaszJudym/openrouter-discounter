package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/preset"
)

const (
	presetPrefix = "@preset/"
	maxBodyBytes = 64 << 20
	// routeHeader names the backend that served the response.
	routeHeader = "X-Dbproxy-Route"
)

// dropForDarkbloom are request fields Darkbloom does not take: OpenRouter
// routing and preset fields, and min_p (not in any model's
// supported_sampling_parameters). Darkbloom ignores unknown fields, so this
// only keeps the request honest.
var dropForDarkbloom = []string{"models", "provider", "preset", "route", "transforms", "plugins", "min_p"}

// Proxy serves the preset slugs as one OpenAI-compatible endpoint. A
// request for "@preset/<slug>" tries the slug's Darkbloom picks (persona
// and parameters injected here, since Darkbloom stores no presets) and the
// OpenRouter preset (applied by OpenRouter), cheaper side first, falling
// back on any error before the response starts. Anything else goes to
// OpenRouter unchanged.
type Proxy struct {
	HC           *http.Client
	OpenRouter   string // API root, e.g. https://openrouter.ai/api/v1
	Darkbloom    string // API root, e.g. https://api.darkbloom.dev/v1
	DarkbloomKey string
	routes       atomic.Pointer[routes]
}

// SetRoutes swaps in a new route table; requests in flight keep theirs.
func (p *Proxy) SetRoutes(r routes) { p.routes.Store(&r) }

func (p *Proxy) route(slug string) (route, bool) {
	rt := p.routes.Load()
	if rt == nil {
		return route{}, false
	}
	r, ok := (*rt)[slug]
	return r, ok
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
		p.chat(w, r)
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		p.models(w)
	default:
		writeError(w, http.StatusNotFound, "dbproxy serves POST /v1/chat/completions and GET /v1/models")
	}
}

// models lists the preset slugs as model ids.
func (p *Proxy) models(w http.ResponseWriter) {
	type entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]entry, 0, len(preset.Slugs))
	for _, slug := range preset.SortedSlugs() {
		data = append(data, entry{ID: presetPrefix + slug, Object: "model", OwnedBy: "dbproxy"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, map[string]any{"object": "list", "data": data}) // client gone: nothing to do
}

// attempt is one upstream request.
type attempt struct {
	name string // routeHeader value, "openrouter" or "darkbloom/<id>"
	url  string
	hdr  http.Header
	body []byte
}

func (p *Proxy) chat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request: "+err.Error())
		return
	}
	var req map[string]jsontext.Value
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "request is not a JSON object: "+err.Error())
		return
	}
	var model string
	_ = json.Unmarshal(req["model"], &model) // a missing or odd model goes to OpenRouter as is
	slug, _ := strings.CutPrefix(model, presetPrefix)
	or := attempt{name: "openrouter", url: p.OpenRouter + "/chat/completions", hdr: openRouterHeader(r.Header), body: body}
	rt, ok := p.route(slug)
	if !ok || !strings.HasPrefix(model, presetPrefix) {
		p.serve(r.Context(), w, model, []attempt{or})
		return
	}
	system, params, _ := preset.Request(slug)
	db := make([]attempt, 0, len(rt.direct))
	for _, id := range rt.direct {
		b, err := darkbloomBody(req, id, system, params)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		db = append(db, attempt{name: "darkbloom/" + id, url: p.Darkbloom + "/chat/completions", hdr: p.darkbloomHeader(), body: b})
	}
	attempts := slices.Concat([]attempt{or}, db)
	if rt.dbFirst {
		attempts = slices.Concat(db, []attempt{or})
	}
	p.serve(r.Context(), w, model, attempts)
}

// serve tries the attempts in order and relays the first that does not
// fail before its response starts; the last one is relayed whatever it is.
func (p *Proxy) serve(ctx context.Context, w http.ResponseWriter, model string, attempts []attempt) {
	for i, a := range attempts {
		if p.try(ctx, w, a, model, i == len(attempts)-1) {
			return
		}
	}
}

// try sends one attempt and relays it (see relay); a transport error is
// relayed as 502 only on the last attempt.
func (p *Proxy) try(ctx context.Context, w http.ResponseWriter, a attempt, model string, last bool) bool {
	resp, err := p.do(ctx, a)
	if err != nil {
		log.Printf("%s via %s: %s", model, a.name, p.clean(err.Error()))
		if last {
			writeError(w, http.StatusBadGateway, "every backend failed; last: "+p.clean(err.Error()))
		}
		return last
	}
	defer func() { _ = resp.Body.Close() }() // relay path: a close error is not actionable
	return p.relay(w, resp, a.name, model, last)
}

func (p *Proxy) do(ctx context.Context, a attempt) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(a.body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header = a.hdr.Clone()
	return p.HC.Do(req)
}

// relay writes resp to the client unless it failed before starting and a
// fallback remains (last false): any non-2xx status, an error object in a
// 2xx JSON body, or an error event before the first SSE data. Once bytes
// reach the client there is no retry. It reports whether it wrote.
func (p *Proxy) relay(w http.ResponseWriter, resp *http.Response, name, model string, last bool) bool {
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	var head []byte
	failed := resp.StatusCode < 200 || resp.StatusCode > 299
	switch {
	case failed:
		head, _ = io.ReadAll(io.LimitReader(br, 1<<20))
	case strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"):
		var err error
		head, failed, err = peekStream(br)
		if err != nil && !errors.Is(err, io.EOF) {
			failed = true
		}
	default:
		head, _ = io.ReadAll(io.LimitReader(br, maxBodyBytes))
		failed = hasError(head)
	}
	if failed && !last {
		log.Printf("%s via %s: status %d, falling back: %.200s", model, name, resp.StatusCode, p.clean(string(head)))
		return false
	}
	log.Printf("%s via %s: status %d", model, name, resp.StatusCode)
	copyHeader(w.Header(), resp.Header)
	w.Header().Set(routeHeader, name)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(head) // client gone: nothing to do
	stream(w, br)
	return true
}

// peekStream reads SSE lines up to and including the first data line, so a
// stream that opens with an error event can still fall back. failed reports
// that first data line carrying an error object.
func peekStream(br *bufio.Reader) (head []byte, failed bool, err error) {
	for {
		line, err := br.ReadBytes('\n')
		head = append(head, line...)
		if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			return head, hasError(bytes.TrimSpace(data)), nil
		}
		if err != nil {
			return head, false, err
		}
	}
}

// hasError reports whether a JSON object carries a non-null "error".
func hasError(b []byte) bool {
	var v struct {
		Error jsontext.Value `json:"error"`
	}
	if json.Unmarshal(b, &v) != nil {
		return false
	}
	return len(v.Error) > 0 && string(v.Error) != "null"
}

// stream copies the rest of the body, flushing each read so SSE events
// reach the client as they arrive.
func stream(w http.ResponseWriter, r io.Reader) {
	rc := http.NewResponseController(w)
	_ = rc.Flush() // unsupported or gone: the copy below still writes
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// darkbloomBody rewrites the client request for one Darkbloom model, the
// way OpenRouter applies a preset: the preset's system prompt goes first
// (merged into a leading client system message, since chat templates take
// one), its parameters fill only what the client did not send, tools and
// messages pass through untouched.
func darkbloomBody(req map[string]jsontext.Value, model, system string, params map[string]any) ([]byte, error) {
	out := make(map[string]jsontext.Value, len(req)+len(params))
	maps.Copy(out, req)
	for _, k := range dropForDarkbloom {
		delete(out, k)
	}
	for k, v := range params {
		if _, set := out[k]; set || k == "min_p" {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("preset param %s: %w", k, err)
		}
		out[k] = b
	}
	m, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	out["model"] = m
	if system != "" {
		msgs, err := withSystem(out["messages"], system)
		if err != nil {
			return nil, err
		}
		out["messages"] = msgs
	}
	return json.Marshal(out)
}

// withSystem puts the preset system prompt first: before a leading client
// system message's content, else as a new first message.
func withSystem(raw jsontext.Value, system string) (jsontext.Value, error) {
	var msgs []map[string]jsontext.Value
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("messages: %w", err)
		}
	}
	var role string
	if len(msgs) > 0 {
		_ = json.Unmarshal(msgs[0]["role"], &role) // no role: not a system message
	}
	if role != "system" {
		sys, err := json.Marshal(map[string]string{"role": "system", "content": system})
		if err != nil {
			return nil, err
		}
		var first map[string]jsontext.Value
		if err := json.Unmarshal(sys, &first); err != nil {
			return nil, err
		}
		return json.Marshal(append([]map[string]jsontext.Value{first}, msgs...))
	}
	content, err := prependText(msgs[0]["content"], system)
	if err != nil {
		return nil, err
	}
	msgs[0]["content"] = content
	return json.Marshal(msgs)
}

// prependText prefixes system to a message content: a string, or an array
// of content parts (a text part is inserted first).
func prependText(raw jsontext.Value, system string) (jsontext.Value, error) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return json.Marshal(system)
		}
		return json.Marshal(system + "\n\n" + s)
	}
	var parts []jsontext.Value
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("system content: %w", err)
	}
	text, err := json.Marshal(map[string]string{"type": "text", "text": system})
	if err != nil {
		return nil, err
	}
	return json.Marshal(append([]jsontext.Value{text}, parts...))
}

// openRouterHeader forwards the client's own OpenRouter key and attribution
// headers; nothing of the Darkbloom key reaches OpenRouter.
func openRouterHeader(in http.Header) http.Header {
	h := http.Header{"Content-Type": {"application/json"}}
	for _, k := range []string{"Authorization", "Http-Referer", "X-Title", "Accept"} {
		if v := in.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	return h
}

// darkbloomHeader authenticates with the proxy's Darkbloom key; the
// client's OpenRouter key never reaches Darkbloom.
func (p *Proxy) darkbloomHeader() http.Header {
	return http.Header{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer " + p.DarkbloomKey},
	}
}

// hopHeaders are not relayed: the proxy's own connection and framing.
var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Transfer-Encoding": true, "Content-Length": true,
	"Proxy-Connection": true, "Upgrade": true, "Trailer": true, "Te": true,
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// clean removes the Darkbloom key from log text and truncates it.
func (p *Proxy) clean(s string) string {
	return fetch.Sanitize(s, p.DarkbloomKey)
}

// writeError answers in the OpenAI error shape.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, map[string]any{"error": map[string]any{"message": msg, "type": "dbproxy_error", "code": status}}) // client gone: nothing to do
}
