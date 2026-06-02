package twilio

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/freeradius/payments-api/internal/notify"
)

const twilioAPIBase = "https://api.twilio.com/2010-04-01"

// Provider implements the notify.Provider interface for Twilio
type Provider struct {
	accountSID   string
	authToken    string
	phoneNumber  string
	client      *http.Client
}

// NewTwilioProvider creates a new Twilio notification provider
func NewTwilioProvider(accountSID, authToken, phoneNumber string) (*Provider, error) {
	if accountSID == "" || authToken == "" || phoneNumber == "" {
		return nil, fmt.Errorf("twilio: account_sid, auth_token, and phone_number are required")
	}
	return &Provider{
		accountSID:  accountSID,
		authToken:   authToken,
		phoneNumber: phoneNumber,
		client:      &http.Client{Timeout: notify.HTTPTimeout},
	}, nil
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "twilio"
}

// Send sends a notification via Twilio (defaults to SMS)
func (p *Provider) Send(ctx context.Context, msg notify.Message) error {
	return p.SendSMS(ctx, msg.To, msg.Body)
}

// SendSMS sends an SMS via Twilio
func (p *Provider) SendSMS(ctx context.Context, to string, body string) error {
	apiURL := fmt.Sprintf("%s/Accounts/%s/Messages.json", twilioAPIBase, p.accountSID)

	data := url.Values{}
	data.Set("To", to)
	data.Set("From", p.phoneNumber)
	data.Set("Body", body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("twilio: failed to create request: %w", err)
	}

	req.SetBasicAuth(p.accountSID, p.authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("twilio: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("twilio: API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
