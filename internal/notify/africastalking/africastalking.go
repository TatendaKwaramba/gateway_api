package africastalking

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/freeradius/payments-api/internal/notify"
)

const atAPIBase = "https://api.africastalking.com/version1"

// Provider implements the notify.Provider interface for Africa's Talking
type Provider struct {
	username string
	apiKey   string
	client   *http.Client
}

// NewATProvider creates a new Africa's Talking notification provider
func NewATProvider(username, apiKey string) (*Provider, error) {
	if username == "" || apiKey == "" {
		return nil, fmt.Errorf("africastalking: username and api_key are required")
	}
	return &Provider{
		username: username,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: notify.HTTPTimeout},
	}, nil
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "africastalking"
}

// Send sends a notification via Africa's Talking (defaults to SMS)
func (p *Provider) Send(ctx context.Context, msg notify.Message) error {
	return p.SendSMS(ctx, msg.To, msg.Body)
}

// SendSMS sends an SMS via Africa's Talking
func (p *Provider) SendSMS(ctx context.Context, to string, body string) error {
	apiURL := atAPIBase + "/messaging"

	data := url.Values{}
	data.Set("username", p.username)
	data.Set("to", to)
	data.Set("message", body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("africastalking: failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("apiKey", p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("africastalking: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("africastalking: API returned %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response to check for errors in payload
	var result struct {
		SMSMessageData struct {
			Messages []struct {
				StatusCode  int    `json:"statusCode"`
				Status      string `json:"status"`
				PhoneNumber string `json:"number"`
			} `json:"Messages"`
		} `json:"SMSMessageData"`
	}
	if err := json.Unmarshal(respBody, &result); err == nil {
		for _, msg := range result.SMSMessageData.Messages {
			if msg.StatusCode != 101 { // 101 = Queued/Sent
				return fmt.Errorf("africastalking: SMS to %s failed with status %d (%s)", msg.PhoneNumber, msg.StatusCode, msg.Status)
			}
		}
	}

	return nil
}
