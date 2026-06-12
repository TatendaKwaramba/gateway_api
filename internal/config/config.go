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
	AppEnv      string `envconfig:"APP_ENV" default:"development"`
	Port        int    `envconfig:"PORT" default:"8080"`
	LogLevel    string `envconfig:"LOG_LEVEL" default:"info"`
	
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
	
	PaynowIntegrationID string `envconfig:"PAYNOW_INTEGRATION_ID"`
	PaynowIntegrationKey string `envconfig:"PAYNOW_INTEGRATION_KEY"`
	PaynowResultURL     string `envconfig:"PAYNOW_RESULT_URL"`
	PaynowReturnURL     string `envconfig:"PAYNOW_RETURN_URL"`
	
	// Django internal API (for CoA disconnect, etc.)
	DjangoBaseURL      string `envconfig:"DJANGO_BASE_URL" default:"http://flash-api:8000"`
	DjangoInternalAPIKey string `envconfig:"DJANGO_INTERNAL_API_KEY" required:"true"`

	// Notifications
	NotifyProvider     string `envconfig:"NOTIFY_PROVIDER" default:"mock"`
	TwilioAccountSID   string `envconfig:"TWILIO_ACCOUNT_SID"`
	TwilioAuthToken    string `envconfig:"TWILIO_AUTH_TOKEN"`
	TwilioPhoneNumber  string `envconfig:"TWILIO_PHONE_NUMBER"`
	ATAPIKey           string `envconfig:"AT_API_KEY"`
	ATUsername         string `envconfig:"AT_USERNAME"`
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
