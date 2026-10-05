// Package expo implements a mobile push notification sender via the
// Expo Push API (https://docs.expo.dev/push-notifications/sending-notifications/).
package expo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/freeradius/payments-api/internal/notify"
)

const (
	// pushAPIEndpoint is the Expo Push API send endpoint
	pushAPIEndpoint = "https://exp.host/--/api/v2/push/send"

	// requestTimeout bounds each push API call
	requestTimeout = 10 * time.Second
)

// Provider sends push notifications through the Expo Push API.
// It implements notify.PushSender; the notify.Provider methods are
// implemented only to satisfy the interface used by the mock SMS
// provider switch and return an error (push is not SMS).
type Provider struct {
	httpClient *http.Client
}

// NewProvider creates a new Expo push provider
func NewProvider() *Provider {
	return &Provider{
		httpClient: &http.Client{Timeout: requestTimeout},
	}
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "expo"
}

type pushRequest struct {
	To        string            `json:"to"`
	Title     string            `json:"title,omitempty"`
	Body      string            `json:"body"`
	Sound     string            `json:"sound,omitempty"`
	ChannelID string            `json:"channelId,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
}

type pushResponse struct {
	Data []struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Details struct {
			Error string `json:"error"`
		} `json:"details"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// SendPush delivers a push notification to a single Expo push token.
// Returns an error wrapping notify.ErrDeviceNotRegistered when Expo reports
// the token as invalid, so callers can disable the device row.
func (p *Provider) SendPush(ctx context.Context, to string, title string, body string, data map[string]string) error {
	if !strings.HasPrefix(to, "ExponentPushToken[") {
		return fmt.Errorf("expo: invalid push token format")
	}

	payload := pushRequest{
		To:        to,
		Title:     title,
		Body:      body,
		Sound:     "default",
		ChannelID: "default",
		Data:      data,
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("expo: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pushAPIEndpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("expo: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("expo: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("expo: read response: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("expo: rate limited (429)")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("expo: unexpected status %d: %s", resp.StatusCode, truncate(respBody, 200))
	}

	var parsed pushResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return fmt.Errorf("expo: decode response: %w", err)
	}

	if len(parsed.Errors) > 0 {
		return fmt.Errorf("expo: api error: %s", parsed.Errors[0].Message)
	}
	if len(parsed.Data) == 0 {
		return fmt.Errorf("expo: empty response")
	}

	result := parsed.Data[0]
	if result.Status == "ok" {
		return nil
	}

	if result.Details.Error == "DeviceNotRegistered" {
		return fmt.Errorf("expo: %s: %w", result.Message, notify.ErrDeviceNotRegistered)
	}
	return fmt.Errorf("expo: push failed: %s", result.Message)
}

// Send satisfies notify.Provider but push is not an SMS channel
func (p *Provider) Send(ctx context.Context, msg notify.Message) error {
	return errors.New("expo: provider does not support generic send (use SendPush)")
}

// SendSMS satisfies notify.Provider but push is not an SMS channel
func (p *Provider) SendSMS(ctx context.Context, to string, body string) error {
	return errors.New("expo: provider does not support SMS (use SendPush)")
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

// ensure interface compliance
var _ notify.PushSender = (*Provider)(nil)
