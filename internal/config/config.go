package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/kelseyhightower/envconfig"
)

// Config holds all configuration for the application
type Config struct {
	// Application
	AppEnv   string `envconfig:"APP_ENV" default:"development"`
	Port     int    `envconfig:"PORT" default:"8080"`
	LogLevel string `envconfig:"LOG_LEVEL" default:"info"`

	// Database
	DBDSN string `envconfig:"DB_DSN" required:"true"`

	// Admin authentication (HMAC shared secret with Beta App)
	AdminHMACSecret string `envconfig:"ADMIN_HMAC_SECRET" required:"true"`

	// Default currency for tariff plans (used when plan has no currency stored)
	DefaultCurrency string `envconfig:"DEFAULT_CURRENCY" default:"ZAR"`

	// Gateway configurations
	MockGatewayEnabled       bool   `envconfig:"MOCK_GATEWAY_ENABLED" default:"false"`
	MockGatewayWebhookSecret string `envconfig:"MOCK_GATEWAY_WEBHOOK_SECRET"`
	MockReturnURL            string `envconfig:"MOCK_RETURN_URL"`

	PaynowIntegrationID  string `envconfig:"PAYNOW_INTEGRATION_ID"`
	PaynowIntegrationKey string `envconfig:"PAYNOW_INTEGRATION_KEY"`
	PaynowResultURL      string `envconfig:"PAYNOW_RESULT_URL"`
	PaynowReturnURL      string `envconfig:"PAYNOW_RETURN_URL"`

	// EcoCash direct gateway (developers.ecocash.co.zw)
	EcoCashAPIKey         string `envconfig:"ECOCASH_API_KEY"`
	EcoCashMerchantCode   string `envconfig:"ECOCASH_MERCHANT_CODE"`
	EcoCashMerchantPin    string `envconfig:"ECOCASH_MERCHANT_PIN"`
	EcoCashMerchantNumber string `envconfig:"ECOCASH_MERCHANT_NUMBER"`
	EcoCashTerminalID     string `envconfig:"ECOCASH_TERMINAL_ID"`
	EcoCashBaseURL        string `envconfig:"ECOCASH_BASE_URL" default:"https://developers.ecocash.co.zw/sandbox"`
	EcoCashNotifyURL      string `envconfig:"ECOCASH_NOTIFY_URL"`
	EcoCashReturnURL      string `envconfig:"ECOCASH_RETURN_URL"`

	// Django internal API (for CoA disconnect, etc.)
	DjangoBaseURL        string `envconfig:"DJANGO_BASE_URL" default:"http://flash-api:8000"`
	DjangoInternalAPIKey string `envconfig:"DJANGO_INTERNAL_API_KEY" required:"true"`

	// Notifications
	NotifyProvider    string `envconfig:"NOTIFY_PROVIDER" default:"mock"`
	TwilioAccountSID  string `envconfig:"TWILIO_ACCOUNT_SID"`
	TwilioAuthToken   string `envconfig:"TWILIO_AUTH_TOKEN"`
	TwilioPhoneNumber string `envconfig:"TWILIO_PHONE_NUMBER"`
	ATAPIKey          string `envconfig:"AT_API_KEY"`
	ATUsername        string `envconfig:"AT_USERNAME"`

	// Mobile app: identity (OTP login) + push notifications
	IdentityHMACSecret string `envconfig:"IDENTITY_HMAC_SECRET"`
	// Rotation: tokens minted with the previous secret keep validating until
	// all clients refresh (M-Login), then can be dropped.
	IdentityHMACSecretPrevious string `envconfig:"IDENTITY_HMAC_SECRET_PREVIOUS"`
	// Google SSO (M-Login): OAuth audience allowlist for id_tokens, comma-separated
	GoogleClientIDs string `envconfig:"GOOGLE_CLIENT_IDS"`
	ExpoPushEnabled bool   `envconfig:"EXPO_PUSH_ENABLED" default:"false"`

	// Branding — user-visible strings (SMS/email/push texts)
	BrandName string `envconfig:"BRAND_NAME" default:"Amani Ecosystems"`
	// Organization attributed to app sign-up customers (0 = auto: the only
	// organization when the deployment has exactly one, else unattributed)
	DefaultOrgID int64 `envconfig:"DEFAULT_ORG_ID" default:"0"`

	// Email (SMTP) — used for email OTP and the email notification fallback
	SMTPHost     string `envconfig:"SMTP_HOST"`
	SMTPPort     int    `envconfig:"SMTP_PORT" default:"587"`
	SMTPUsername string `envconfig:"SMTP_USERNAME"`
	SMTPPassword string `envconfig:"SMTP_PASSWORD"`
	SMTPFrom     string `envconfig:"SMTP_FROM" default:"noreply@example.com"`
	// Branded OTP email template (M-Login): header accent color (#, no hash)
	EmailPrimaryColor string `envconfig:"EMAIL_PRIMARY_COLOR" default:"#145377"`
	// Mailgun (M-CIM): preferred email sender when MAILGUN_API_KEY configured
	MailgunAPIKey  string `envconfig:"MAILGUN_API_KEY"`
	MailgunDomain  string `envconfig:"MAILGUN_API_DOMAIN"`
	MailgunBaseURL string `envconfig:"MAILGUN_BASE_URL" default:"https://api.mailgun.net"`
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	// Set up logging level
	setLogLevel(cfg.LogLevel)

	slog.Info("configuration loaded",
		slog.String("app_env", cfg.AppEnv),
		slog.Int("port", cfg.Port),
		slog.Bool("mock_enabled", cfg.MockGatewayEnabled),
	)

	return &cfg, nil
}

// Validate validates the configuration
func (c *Config) Validate() error {
	var errs []string

	// Safety check: mock gateway must not be enabled in production
	if c.AppEnv == "production" && c.MockGatewayEnabled {
		errs = append(errs, "MOCK_GATEWAY_ENABLED cannot be true in production")
	}

	// Mock gateway requires webhook secret when enabled
	if c.MockGatewayEnabled && c.MockGatewayWebhookSecret == "" {
		errs = append(errs, "MOCK_GATEWAY_WEBHOOK_SECRET is required when mock gateway is enabled")
	}

	// Admin HMAC secret must be sufficiently long
	if len(c.AdminHMACSecret) < 32 {
		errs = append(errs, "ADMIN_HMAC_SECRET must be at least 32 characters")
	}

	// Identity HMAC secret must be sufficiently long when configured
	if c.IdentityHMACSecret != "" && len(c.IdentityHMACSecret) < 32 {
		errs = append(errs, "IDENTITY_HMAC_SECRET must be at least 32 characters")
	}

	// Django internal API key must be sufficiently long
	if len(c.DjangoInternalAPIKey) < 32 {
		errs = append(errs, "DJANGO_INTERNAL_API_KEY must be at least 32 characters")
	}

	// Paynow: enabled only when server-side credentials/URLs are set (not PAYNOW_RETURN_URL alone —
	// mock pilot uses MOCK_RETURN_URL; .env.example often sets RETURN for customer-app redirects).
	paynowConfigured := c.PaynowIntegrationID != "" || c.PaynowIntegrationKey != "" || c.PaynowResultURL != ""
	if paynowConfigured {
		if c.PaynowIntegrationID == "" {
			errs = append(errs, "PAYNOW_INTEGRATION_ID is required when Paynow is configured")
		}
		if c.PaynowIntegrationKey == "" {
			errs = append(errs, "PAYNOW_INTEGRATION_KEY is required when Paynow is configured")
		}
		if c.PaynowResultURL == "" {
			errs = append(errs, "PAYNOW_RESULT_URL is required when Paynow is configured")
		}
		if c.PaynowReturnURL == "" {
			errs = append(errs, "PAYNOW_RETURN_URL is required when Paynow is configured")
		}
	}

	// EcoCash: enabled only when any credential is set
	ecocashConfigured := c.EcoCashAPIKey != "" || c.EcoCashMerchantCode != "" || c.EcoCashMerchantNumber != ""
	if ecocashConfigured {
		if c.EcoCashAPIKey == "" {
			errs = append(errs, "ECOCASH_API_KEY is required when EcoCash is configured")
		}
		if c.EcoCashMerchantCode == "" {
			errs = append(errs, "ECOCASH_MERCHANT_CODE is required when EcoCash is configured")
		}
		if c.EcoCashMerchantPin == "" {
			errs = append(errs, "ECOCASH_MERCHANT_PIN is required when EcoCash is configured")
		}
		if c.EcoCashMerchantNumber == "" {
			errs = append(errs, "ECOCASH_MERCHANT_NUMBER is required when EcoCash is configured")
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}

	return nil
}

// IsProduction returns true if running in production mode
func (c *Config) IsProduction() bool {
	return c.AppEnv == "production"
}

// IsDevelopment returns true if running in development mode
func (c *Config) IsDevelopment() bool {
	return c.AppEnv == "development"
}

func setLogLevel(level string) {
	var slogLevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn", "warning":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slogLevel,
	}))
	slog.SetDefault(logger)
}
