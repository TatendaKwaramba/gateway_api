// Package mailgun implements an email notification provider backed by the
// Mailgun HTTP API (https://api.mailgun.net/v3/<domain>/messages). It
// satisfies the notify.Provider interface for the email channel and
// outranks SMTP when MAILGUN_API_KEY is configured (M-Login email OTP and
// notification fallback).
package mailgun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/freeradius/payments-api/internal/notify"
)

// Provider sends email through the Mailgun messages API
type Provider struct {
	httpClient *http.Client
	apiKey     string
	domain     string
	from       string
	baseURL    string
}

// NewProvider creates a Mailgun email provider. baseURL overrides
// https://api.mailgun.net (may be a test httptest server); passing "" uses
// the production endpoint.
func NewProvider(apiKey, domain, from, baseURL string) *Provider {
	if baseURL == "" {
		baseURL = "https://api.mailgun.net"
	}
	return &Provider{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		apiKey:     apiKey,
		domain:     domain,
		from:       from,
		baseURL:    strings.TrimSuffix(baseURL, "/"),
	}
}

// Name returns the provider name
func (p *Provider) Name() string { return "mailgun" }

// From holds the sending identity; overridable per production setup
func (p *Provider) SetFrom(from string) { p.from = from }

// Send delivers an email message via the Mailgun messages API
func (p *Provider) Send(ctx context.Context, msg notify.Message) error {
	if msg.To == "" {
		return errors.New("mailgun: recipient is required")
	}
	if msg.Subject == "" {
		msg.Subject = "AmaniConnect notification"
	}

	form := url.Values{}
	form.Set("from", p.from)
	form.Set("to", msg.To)
	form.Set("subject", msg.Subject)
	form.Set("text", msg.Body)
	if msg.HTML != "" {
		form.Set("html", msg.HTML)
	}

	endpoint := fmt.Sprintf("%s/v3/%s/messages", p.baseURL, p.domain)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("mailgun: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("api", p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mailgun: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		safe := string(raw)
		if len(safe) > 300 {
			safe = safe[:300]
		}
		return fmt.Errorf("mailgun: API returned %d: %s", resp.StatusCode, safe)
	}

	slog.Info("mailgun: email sent",
		slog.String("to", msg.To),
		slog.String("domain", p.domain),
		"message_id", resp.Header.Get("X-Mailgun-Message-Id"),
	)
	return nil
}

// SendSMS — Mailgun is an email-only provider; keep the interface satisfied
// and fail explicitly so channel fallback stays predictable.
func (p *Provider) SendSMS(ctx context.Context, to, body string) error {
	return errors.New("mailgun: SMS not supported (email channel only)")
}
