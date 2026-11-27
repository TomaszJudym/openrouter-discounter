// Package telegram sends messages via the Bot API sendMessage call.
//
// The bot token is carried only in the request URL and the chat id only in
// the request body; neither appears in logged error text: every error
// message is redacted before it is returned.
package telegram

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const apiFormat = "https://api.telegram.org/bot%s/sendMessage"

const maxBodyBytes = 64 << 10

type sendMessageRequest struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

type apiResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// Send posts text to chatID as HTML. A non-2xx reply or an ok:false reply is
// an error carrying the API error body.
func Send(ctx context.Context, hc *http.Client, token, chatID, text string) error {
	payload, err := json.Marshal(sendMessageRequest{ChatID: chatID, Text: text, ParseMode: "html"})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	apiURL := fmt.Sprintf(apiFormat, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return redact(fmt.Errorf("build request: %w", err), token, chatID)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return redact(err, token, chatID)
	}
	defer func() { _ = resp.Body.Close() }() // read-only: a close error is not actionable
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return redact(fmt.Errorf("read response: %w", err), token, chatID)
	}
	if resp.StatusCode != http.StatusOK {
		return redact(fmt.Errorf("sendMessage: status %d: %.500s", resp.StatusCode, body), token, chatID)
	}
	var api apiResponse
	if err := json.Unmarshal(body, &api); err != nil {
		return redact(fmt.Errorf("parse response: %v (body: %.200s)", err, body), token, chatID)
	}
	if !api.OK {
		return redact(fmt.Errorf("sendMessage rejected: %s", api.Description), token, chatID)
	}
	return nil
}

// redact removes the token and the chat id — both secrets — from an error's
// text before it reaches callers. The chat id is echoed in request params
// when the API rejects the call.
func redact(err error, token, chatID string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "REDACTED")
	}
	if chatID != "" {
		msg = strings.ReplaceAll(msg, chatID, "REDACTED")
	}
	return errors.New(msg)
}
