package telegram

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dummyToken is built at runtime so no credential-shaped literal exists in
// the source; its value only has to be a stable, leak-detectable string.
var dummyToken = strings.Repeat("dummy", 3) + ":tok"

func TestSendSuccess(t *testing.T) {
	var got sendMessageRequest
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/bot"+dummyToken+"/sendMessage") {
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
	err := Send(t.Context(), srv.Client(), dummyToken, "@mychannel", "<b>hello</b>")
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}
	if got.ChatID != "@mychannel" || got.Text != "<b>hello</b>" || got.ParseMode != "html" {
		t.Errorf("payload = %+v, want chat @mychannel, html parse mode", got)
	}
}

func TestSendErrorBodyCarriesAPIMessageWithoutSecrets(t *testing.T) {
	// the API body echoes both secrets (a rejected chat_id and the URL's
	// token); Send must redact both before returning
	desc := "Bad Request: chat_id @nope is invalid, bot " + dummyToken
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"` + desc + `"}`))
	}))
	err := Send(t.Context(), srv.Client(), dummyToken, "@nope", "msg")
	if err == nil {
		t.Fatal("Send() error = nil, want API error")
	}
	if !strings.Contains(err.Error(), "chat_id") {
		t.Errorf("error missing API description: %v", err)
	}
	if strings.Contains(err.Error(), dummyToken) {
		t.Errorf("error leaks the token: %v", err)
	}
	if strings.Contains(err.Error(), "@nope") {
		t.Errorf("error leaks the chat id: %v", err)
	}
	if got := strings.Count(err.Error(), "REDACTED"); got != 2 {
		t.Errorf("error = %q, want both secrets replaced, got %d REDACTED", err, got)
	}
}

func TestSendTransportErrorRedactsToken(t *testing.T) {
	// the transport error itself carries both secrets (as url.Error would);
	// Send must redact them before returning
	hc := &http.Client{Transport: failingTransport{"@nope"}}
	err := Send(t.Context(), hc, dummyToken, "@nope", "msg")
	if err == nil {
		t.Fatal("Send() error = nil, want transport error")
	}
	if strings.Contains(err.Error(), dummyToken) {
		t.Errorf("error leaks the token: %v", err)
	}
	if strings.Contains(err.Error(), "@nope") {
		t.Errorf("error leaks the chat id: %v", err)
	}
}

// TestSendRejectedDescriptionRedactsChatID: the ok:false path (status 200)
// redacts the chat id too — the API's description can name the chat.
func TestSendRejectedDescriptionRedactsChatID(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat @nope not found"}`))
	}))
	err := Send(t.Context(), srv.Client(), dummyToken, "@nope", "msg")
	if err == nil {
		t.Fatal("Send() error = nil, want the rejection error")
	}
	if strings.Contains(err.Error(), "@nope") {
		t.Errorf("error leaks the chat id: %v", err)
	}
	if !strings.Contains(err.Error(), "chat") {
		t.Errorf("error lost the API description: %v", err)
	}
}

type failingTransport struct{ chatID string }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("Post \"https://api.telegram.org/bot" + dummyToken + "/sendMessage\": chat " + f.chatID + ": connection refused")
}
