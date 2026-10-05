// Package smtp implements an email notification provider over SMTP
// (STARTTLS on the submission port, using net/smtp's automatic TLS).
// It satisfies the notify.Provider interface for the email channel and is
// used for email OTP delivery and the email notification fallback.
package smtp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
	"time"

	"github.com/freeradius/payments-api/internal/notify"
)

// Provider sends email through an SMTP server
type Provider struct {
	host     string
	port     int
	username string
	password string
	from     string
}

// NewProvider creates a new SMTP email provider
func NewProvider(host string, port int, username, password, from string) *Provider {
	return &Provider{
		host:     host,
		port:     port,
		username: username,
		password: password,
		from:     from,
	}
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "smtp"
}

// Send delivers an email message
func (p *Provider) Send(ctx context.Context, msg notify.Message) error {
	if msg.To == "" {
		return errors.New("smtp: recipient is required")
	}
	if msg.Subject == "" {
		msg.Subject = "Amani Ecosystems notification"
	}

	// Multipart message: always a text part (renderable everywhere) plus a
	// branded HTML part when the caller supplied one (M-Login OTP template).
	raw := buildMime(p.from, msg.To, msg.Subject, msg.Body, msg.HTML)

	addr := fmt.Sprintf("%s:%d", p.host, p.port)
	auth := smtp.PlainAuth("", p.username, p.password, p.host)

	// net/smtp upgrades to STARTTLS automatically when the server advertises it.
	// Transient Google "454 4.7.0 Too many login attempts" windows self-heal with
	// a short spaced retry instead of failing the identity request outright.

	var backoffs = []time.Duration{0, 5 * time.Second, 15 * time.Second}
	var lastErr error
	for _, backoff := range backoffs {
		if backoff > 0 {
			time.Sleep(backoff)
		}
		err := smtp.SendMail(addr, auth, p.from, []string{msg.To}, raw)
		if err == nil {
			slog.Info("smtp: email sent",
				slog.String("to", msg.To),
				slog.String("subject", msg.Subject),
			)
			return nil
		}
		lastErr = err
		if !isTransient454(err) {
			break
		}
	}
	return fmt.Errorf("smtp: send failed: %w", lastErr)
}

// SendSMS satisfies notify.Provider but email is not an SMS channel
func (p *Provider) SendSMS(ctx context.Context, to string, body string) error {
	return errors.New("smtp: provider does not support SMS (use Send)")
}

// ensure interface compliance
var _ notify.Provider = (*Provider)(nil)

// buildMime assembles the RFC 5322 message. Plain-text-only messages go out
// asBefore; HTML availability promotes the body to multipart/alternative so
// branded OTP emails render in both modes.
func buildMime(from, to, subject, textBody, htmlBody string) []byte {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = "Amani Ecosystems notification"
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "From: %s\r\n", from)
	fmt.Fprintf(&builder, "To: %s\r\n", to)
	fmt.Fprintf(&builder, "Subject: %s\r\n", subject)
	builder.WriteString("MIME-Version: 1.0\r\n")

	if htmlBody == "" {
		builder.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
		builder.WriteString("\r\n")
		builder.WriteString(textBody)
		return []byte(builder.String())
	}

	boundary := "AmaniOTP-Boundary-7483"
	fmt.Fprintf(&builder, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", boundary)
	builder.WriteString("--" + boundary + "\r\n")
	builder.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n\r\n")
	builder.WriteString(textBody)
	builder.WriteString("\r\n")
	builder.WriteString("--" + boundary + "\r\n")
	builder.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n\r\n")
	builder.WriteString(htmlBody)
	builder.WriteString("\r\n")
	builder.WriteString("--" + boundary + "--\r\n")
	return []byte(builder.String())
}

// isTransient454 matches Google's temporary "454 4.7.0 Too many login
// attempts" — retried with backoff before giving up.
func isTransient454(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "454 4.7.0 Too many login attempts")
}
