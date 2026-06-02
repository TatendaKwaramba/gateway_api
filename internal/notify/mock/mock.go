package mock

import (
	"context"
	"log/slog"

	"github.com/freeradius/payments-api/internal/notify"
)

// Provider is a mock notification provider that logs to stdout
type Provider struct{}

// NewMockProvider creates a new mock notification provider
func NewMockProvider() *Provider {
	return &Provider{}
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "mock"
}

// Send logs a notification message to stdout
func (p *Provider) Send(ctx context.Context, msg notify.Message) error {
	slog.Info("[MOCK NOTIFICATION]",
		slog.String("to", msg.To),
		slog.String("body", msg.Body),
		slog.String("subject", msg.Subject),
	)
	return nil
}

// SendSMS logs an SMS message to stdout
func (p *Provider) SendSMS(ctx context.Context, to string, body string) error {
	return p.Send(ctx, notify.Message{
		To:   to,
		Body: body,
	})
}
