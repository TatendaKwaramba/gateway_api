// Package notify defines the notification provider interface and implementations
package notify

import (
	"context"
)

// Message represents a notification message to be sent
type Message struct {
	To      string // Phone number or email address
	Body    string // Message body
	Subject string // Optional subject (for email)
}

// Provider is the interface for notification adapters
type Provider interface {
	// Name returns the provider name
	Name() string

	// Send sends a notification message
	Send(ctx context.Context, msg Message) error

	// SendSMS sends an SMS message
	SendSMS(ctx context.Context, to string, body string) error
}

// Channel represents a notification channel
type Channel string

const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
)

// Result represents the result of a notification attempt
type Result struct {
	Success   bool
	Error     error
	Provider  string
	Channel   Channel
	AttemptID int64
}



// Config holds configuration for all notification providers
type Config struct {
	TwilioAccountSID  string
	TwilioAuthToken   string
	TwilioPhoneNumber string
	ATUsername        string
	ATAPIKey          string
}
