package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The stubs serve the exact JSON shapes internal/discounts and
// internal/darkbloom parse: an OpenRouter catalog plus per-model endpoint
// pricing, a Darkbloom /v1/models list, and OpenAI-compatible chat
// completions on both sides (dbproxy forwards chat to whichever upstream
// the route picks).
const (
	orModelID = "acme/orion" // OpenRouter catalog id
	dbModelID = "orion-7b"   // the Darkbloom build serving that model
	hfID      = "Acme/Orion-7B"

	presetWorker = "@preset/admech-code"
	presetOracle = "@preset/admech-code-oracle"

	// delegateMarker rides in the parent prompt; the stub answers it with
	// a subagent tool call, so pi delegates deterministically.
	delegateMarker = "E2E_DELEGATE"
	childTask      = "Reply with one word."
)

// price is USD per million tokens, the unit discounts works in.
type price struct{ prompt, completion float64 }

var (
	orExpensive = price{prompt: 0.60, completion: 1.60} // discounted: 0.30 / 0.80
	orCheap     = price{prompt: 0.04, completion: 0.12} // discounted: 0.02 / 0.06
	dbDirect    = price{prompt: 0.08, completion: 0.24}
)

// chatCall records one chat completion the stub answered.
type chatCall struct {
	Host   string
	Path   string
	Model  string
	Stream bool
}

// orStub is the OpenRouter stub: catalog, endpoint pricing, completions.
// Its price is mutable so a running dbproxy's refresh can see it change.
type orStub struct {
	mu       sync.Mutex
	p        price
	discount float64
	chats    []chatCall
	srv      *httptest.Server
}

func newORStub(t *testing.T, p price) *orStub {
	t.Helper()
	s := &orStub{p: p, discount: 0.5}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/models", s.models)
	mux.HandleFunc("GET /api/v1/models/"+orModelID+"/endpoints", s.endpoints)
	mux.HandleFunc("POST /api/v1/chat/completions", s.chat)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *orStub) url() string { return s.srv.URL }

func (s *orStub) setPrice(p price) {
	s.mu.Lock()
	s.p = p
	s.mu.Unlock()
}

func (s *orStub) chatsSnapshot() []chatCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]chatCall(nil), s.chats...)
}

func (s *orStub) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"data": []any{map[string]any{
		"id":                   orModelID,
		"canonical_slug":       orModelID,
		"hugging_face_id":      hfID,
		"pricing":              map[string]any{"prompt": "0.000001", "completion": "0.000002"},
		"benchmarks":           map[string]any{"artificial_analysis": map[string]any{"intelligence_index": 55.0, "coding_index": 50.0}},
		"reasoning":            map[string]any{"mandatory": false},
		"supported_parameters": []string{"tools", "reasoning"},
		"architecture":         map[string]any{"input_modalities": []string{"text"}, "output_modalities": []string{"text"}},
		"context_length":       1_000_000,
	}}})
}

func (s *orStub) endpoints(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	p, discount := s.p, s.discount
	s.mu.Unlock()
	writeJSON(w, map[string]any{"data": map[string]any{
		"id": orModelID,
		"endpoints": []any{map[string]any{
			"provider_name":             "acme-cloud",
			"tag":                       "acme-cloud/fp8",
			"quantization":              "fp8",
			"pricing":                   map[string]any{"prompt": perToken(p.prompt), "completion": perToken(p.completion), "discount": discount},
			"context_length":            1_000_000,
			"max_completion_tokens":     65_536,
			"supported_parameters":      []string{"tools"},
			"supports_implicit_caching": true,
			"supports_tool_choice":      map[string]any{"auto": true},
		}},
	}})
}

func (s *orStub) chat(w http.ResponseWriter, r *http.Request) {
	req := decodeChat(w, r)
	if req.Model == "" {
		return
	}
	s.mu.Lock()
	s.chats = append(s.chats, chatCall{Host: r.Host, Path: r.URL.Path, Model: req.Model, Stream: req.Stream})
	s.mu.Unlock()
	answerChat(w, req)
}

// dbStub is the Darkbloom stub: its model list and its OpenAI-compatible
// completions endpoint (dbproxy forwards the chat there for a db route).
type dbStub struct {
	mu    sync.Mutex
	p     price
	chats []chatCall
	srv   *httptest.Server
}

func newDBStub(t *testing.T, p price) *dbStub {
	t.Helper()
	s := &dbStub{p: p}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *dbStub) url() string { return s.srv.URL }

func (s *dbStub) chatsSnapshot() []chatCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]chatCall(nil), s.chats...)
}

func (s *dbStub) models(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	writeJSON(w, map[string]any{"data": []any{map[string]any{
		"id":                 dbModelID,
		"hugging_face_id":    hfID,
		"input_modalities":   []string{"text"},
		"context_length":     1_000_000,
		"max_output_length":  65_536,
		"pricing":            map[string]any{"prompt": perToken(p.prompt), "completion": perToken(p.completion)},
		"supported_features": []string{"tools"},
		"metadata":           map[string]any{"can_accept": true, "routable_providers": 2},
	}}})
}

func (s *dbStub) chat(w http.ResponseWriter, r *http.Request) {
	req := decodeChat(w, r)
	if req.Model == "" {
		return
	}
	s.mu.Lock()
	s.chats = append(s.chats, chatCall{Host: r.Host, Path: r.URL.Path, Model: req.Model, Stream: req.Stream})
	s.mu.Unlock()
	answerChat(w, req)
}

// chatRequest is the part of an OpenAI completion request the stubs need.
type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func decodeChat(w http.ResponseWriter, r *http.Request) chatRequest {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"error": "decode request: " + err.Error()})
		return chatRequest{}
	}
	return req
}

// answerChat scripts one reply. A parent turn (the marker in a user
// message, no tool result yet, subagent tool offered) delegates to oracle;
// every other turn, including the child's own, answers with text. That
// keeps the pi run deterministic without depending on model ids, which
// dbproxy rewrites for Darkbloom.
func answerChat(w http.ResponseWriter, req chatRequest) {
	if isParentTurn(req) {
		answerToolCall(w, req)
		return
	}
	answerText(w, req)
}

func isParentTurn(req chatRequest) bool {
	if !hasTool(req, "subagent") {
		return false
	}
	marked, toolResult := false, false
	for _, m := range req.Messages {
		switch m.Role {
		case "tool":
			toolResult = true
		case "user":
			marked = marked || strings.Contains(string(m.Content), delegateMarker)
		}
	}
	return marked && !toolResult
}

func hasTool(req chatRequest, name string) bool {
	for _, td := range req.Tools {
		if td.Function.Name == name {
			return true
		}
	}
	return false
}

func answerToolCall(w http.ResponseWriter, req chatRequest) {
	args, _ := json.Marshal(map[string]any{"agent": "oracle", "task": childTask, "async": false})
	msg := map[string]any{
		"role": "assistant", "content": "",
		"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_e2e", "type": "function",
			"function": map[string]any{"name": "subagent", "arguments": string(args)},
		}},
	}
	writeCompletion(w, req, msg, "tool_calls")
}

func answerText(w http.ResponseWriter, req chatRequest) {
	msg := map[string]any{"role": "assistant", "content": "hello from " + req.Model}
	writeCompletion(w, req, msg, "stop")
}

func writeCompletion(w http.ResponseWriter, req chatRequest, message map[string]any, finish string) {
	if req.Stream {
		writeSSE(w, req.Model, message, finish)
		return
	}
	writeJSON(w, map[string]any{
		"id": "chatcmpl-e2e", "object": "chat.completion", "created": 1, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
	})
}

func writeSSE(w http.ResponseWriter, model string, message map[string]any, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	chunk := map[string]any{
		"id": "chatcmpl-e2e", "object": "chat.completion.chunk", "created": 1, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}},
	}
	b, _ := json.Marshal(chunk)
	_, _ = w.Write([]byte("data: " + string(b) + "\n\ndata: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// perToken renders USD per Mtok as the per-token decimal string the APIs use.
func perToken(perMTok float64) string {
	return strconv.FormatFloat(perMTok/1e6, 'f', -1, 64)
}
