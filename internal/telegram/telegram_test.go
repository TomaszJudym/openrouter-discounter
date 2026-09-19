package telegram

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "123456:ABC-def_ghi"

func TestSendSuccess(t *testing.T) {
	var got sendMessageRequest
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/bot"+testToken+"/sendMessage") {
			t.Errorf("request path = %q, want bot token path", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if err := json.UnmarshalRead(r.Body, &got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	err := Send(t.Context(), srv.Client(), testToken, "@mychannel", "<b>hello</b>")
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}
	if got.ChatID != "@mychannel" || got.Text != "<b>hello</b>" || got.ParseMode != "html" {
		t.Errorf("payload = %+v, want chat @mychannel, html parse mode", got)
	}
}

func TestSendErrorBodyCarriesAPIMessageNotToken(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	err := Send(t.Context(), srv.Client(), testToken, "@nope", "msg")
	if err == nil {
		t.Fatal("Send() error = nil, want API error")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error missing API description: %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestSendTransportErrorRedactsToken(t *testing.T) {
	// the transport error itself carries the token (as url.Error would);
	// Send must redact it before returning
	hc := &http.Client{Transport: failingTransport{}}
	err := Send(t.Context(), hc, testToken, "@nope", "msg")
	if err == nil {
		t.Fatal("Send() error = nil, want transport error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error leaks the token: %v", err)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("Post \"https://api.telegram.org/bot" + testToken + "/sendMessage\": connection refused")
}
