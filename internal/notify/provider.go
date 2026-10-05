// Package notify defines the notification provider interface and implementations
package notify

import (
	"context"
	"errors"
)

// Message represents a notification message to be sent
type Message struct {
	To      string // Phone number or email address
	Body    string // Message body (plain text; always populated)
	HTML    string // Optional branded HTML body (email channel only)
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
	ChannelPush  Channel = "push"
)

// PushSender is the interface for mobile push notification adapters (e.g. Expo).
// The recipient "to" is a provider-specific device token (Expo push token).
type PushSender interface {
	// Name returns the sender name
	Name() string

	// SendPush delivers a push notification to a single device token.
	// data carries optional deep-link payload key/value pairs.
	SendPush(ctx context.Context, to string, title string, body string, data map[string]string) error
}

// ErrDeviceNotRegistered should be returned (wrapped) by PushSender
// implementations when the device token is no longer valid, so callers
// can disable the device row.
var ErrDeviceNotRegistered = errors.New("device not registered")

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
