package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/freeradius/payments-api/internal/config"
	"github.com/freeradius/payments-api/internal/django"
	"github.com/freeradius/payments-api/internal/fulfillment"
	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/freeradius/payments-api/internal/gateways/ecocash"
	"github.com/freeradius/payments-api/internal/gateways/mock"
	"github.com/freeradius/payments-api/internal/gateways/paynow"
	"github.com/freeradius/payments-api/internal/httpapi"
	"github.com/freeradius/payments-api/internal/identity"
	"github.com/freeradius/payments-api/internal/notify"
	"github.com/freeradius/payments-api/internal/notify/africastalking"
	"github.com/freeradius/payments-api/internal/notify/expo"
	"github.com/freeradius/payments-api/internal/notify/mailgun"
	notifymock "github.com/freeradius/payments-api/internal/notify/mock"
	"github.com/freeradius/payments-api/internal/notify/smtp"
	"github.com/freeradius/payments-api/internal/notify/twilio"
	"github.com/freeradius/payments-api/internal/payments"
	"github.com/freeradius/payments-api/internal/workers"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	if err := run(); err != nil {
		slog.Error("payments-api startup failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	slog.Info("starting payments-api",
		slog.String("app_env", cfg.AppEnv),
		slog.Int("port", cfg.Port),
	)

	// Connect to database
	db, err := connectDB(cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	// Verify database connection
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return fmt.Errorf("database ping: %w", err)
	}
	defer db.Close()

	slog.Info("database connected")

	// Create gateway registry
	registry := gateways.NewRegistry()

	// Register mock gateway if enabled
	if cfg.MockGatewayEnabled {
		slog.Info("registering mock gateway")
		mockAdapter := mock.NewAdapter(cfg.MockGatewayWebhookSecret, cfg.MockReturnURL)
		mockAdapter.SetWebhookBaseURL(fmt.Sprintf("http://localhost:%d", cfg.Port))
		if err := registry.Register(mockAdapter); err != nil {
			return fmt.Errorf("register mock gateway: %w", err)
		}

		// In dev mode, register a mock adapter for the ecocash gateway when real
		// EcoCash credentials aren't configured. This lets the customer app show
		// and test the full EcoCash checkout flow.
		if cfg.EcoCashAPIKey == "" || cfg.EcoCashMerchantCode == "" {
			slog.Info("registering mock adapter for ecocash gateway (dev mode)")
			ecocashMock := mock.NewAdapterWithCode("ecocash", cfg.MockGatewayWebhookSecret, cfg.MockReturnURL)
			ecocashMock.SetWebhookBaseURL(fmt.Sprintf("http://localhost:%d", cfg.Port))
			if err := registry.Register(ecocashMock); err != nil {
				return fmt.Errorf("register mock ecocash adapter: %w", err)
			}
		}
	}

	// Register Paynow gateway if credentials are configured
	if cfg.PaynowIntegrationID != "" && cfg.PaynowIntegrationKey != "" {
		slog.Info("registering paynow gateway")
		paynowAdapter := paynow.NewAdapter(
			cfg.PaynowIntegrationID,
			cfg.PaynowIntegrationKey,
			cfg.PaynowResultURL,
			cfg.PaynowReturnURL,
		)
		if err := registry.Register(paynowAdapter); err != nil {
			return fmt.Errorf("register paynow gateway: %w", err)
		}
	}

	// Register EcoCash gateway if credentials are configured
	if cfg.EcoCashAPIKey != "" && cfg.EcoCashMerchantCode != "" {
		slog.Info("registering ecocash gateway")
		ecocashAdapter := ecocash.NewAdapter(
			cfg.EcoCashAPIKey,
			cfg.EcoCashMerchantCode,
			cfg.EcoCashMerchantPin,
			cfg.EcoCashMerchantNumber,
			cfg.EcoCashTerminalID,
			cfg.EcoCashBaseURL,
			cfg.EcoCashNotifyURL,
			cfg.EcoCashReturnURL,
		)
		if err := registry.Register(ecocashAdapter); err != nil {
			return fmt.Errorf("register ecocash gateway: %w", err)
		}
	}

	// Create notification provider
	var notificationProvider notify.Provider
	switch cfg.NotifyProvider {
	case "twilio":
		p, err := twilio.NewTwilioProvider(cfg.TwilioAccountSID, cfg.TwilioAuthToken, cfg.TwilioPhoneNumber)
		if err != nil {
			return fmt.Errorf("create twilio provider: %w", err)
		}
		notificationProvider = p
	case "africastalking", "at":
		p, err := africastalking.NewATProvider(cfg.ATUsername, cfg.ATAPIKey)
		if err != nil {
			return fmt.Errorf("create africastalking provider: %w", err)
		}
		notificationProvider = p
	case "mock":
		notificationProvider = notifymock.NewMockProvider()
	default:
		return fmt.Errorf("unknown notification provider %q", cfg.NotifyProvider)
	}
	slog.Info("notification provider configured", slog.String("provider", notificationProvider.Name()))

	// Email provider (M-CIM): Mailgun HTTP API wins when configured; SMTP
	// (Gmail submission etc.) remains the fallback for plain SMTP setups.
	var emailProvider notify.Provider
	switch {
	case cfg.MailgunAPIKey != "":
		emailProvider = mailgun.NewProvider(
			cfg.MailgunAPIKey,
			cfg.MailgunDomain,
			fmt.Sprintf("Amani Connect <postmaster@%s>", cfg.MailgunDomain),
			cfg.MailgunBaseURL,
		)
		slog.Info("email provider configured", slog.String("provider", emailProvider.Name()))
	case cfg.SMTPHost != "":
		emailProvider = smtp.NewProvider(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPFrom)
		slog.Info("email provider configured", slog.String("provider", emailProvider.Name()))
	}

	// Mobile push sender (Expo)
	var pushSender notify.PushSender
	if cfg.ExpoPushEnabled {
		pushSender = expo.NewProvider()
		slog.Info("mobile push enabled", slog.String("provider", pushSender.Name()))
	}

	// Identity service (mobile OTP login + device registry); nil-safe when unconfigured
	identityService := identity.NewService(db, notificationProvider, emailProvider, cfg.IdentityHMACSecret, cfg.SMTPFrom)
	identityService.SetBrandName(cfg.BrandName)
	identityService.SetEmailAccentColor(strings.TrimPrefix(cfg.EmailPrimaryColor, "#"))
	identityService.SetPreviousSecret(cfg.IdentityHMACSecretPrevious)
	identityService.SetDefaultOrgID(cfg.DefaultOrgID)
	if googleIDs := parseClientIDs(cfg.GoogleClientIDs); googleIDs != nil {
		if err := identityService.ConfigureGoogle(googleIDs, nil); err != nil {
			slog.Error("identity: Google sign-in disabled (discovery failed)", slog.Any("error", err))
		}
	}
	if identityService.Enabled() {
		slog.Info("identity service enabled")
	} else {
		slog.Warn("identity service disabled (IDENTITY_HMAC_SECRET not configured)")
	}

	// Create fulfillment service
	fulfillmentService := fulfillment.NewService(db, notificationProvider)
	fulfillmentService.SetPushSender(pushSender, identityService)

	// Create Django internal API client
	djangoClient := django.NewClient(cfg.DjangoBaseURL, cfg.DjangoInternalAPIKey)

	// Create payment service with fulfillment integration
	paymentService := payments.NewService(db, registry, fulfillmentService, djangoClient, cfg.DefaultCurrency)

	// Surface gateway-vs-DB currency divergence at boot (warn-only; the
	// initiate guard and currency filters make stale rows inert regardless).
	if err := payments.CheckGatewayCurrencyDrift(context.Background(), db, registry); err != nil {
		slog.Warn("currency drift check failed", slog.Any("error", err))
	}

	// Create admin auth middleware
	adminAuth := httpapi.AdminHMACMiddleware(cfg.AdminHMACSecret)

	// Create HTTP router
	router := httpapi.NewRouter(paymentService, registry, adminAuth, identityService, fulfillmentService)
	httpHandler := router.Setup()

	// Create HTTP server
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      httpHandler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start background workers
	poller := workers.NewPoller(db, registry, paymentService.TriggerFulfillment)
	poller.Start()
	defer poller.Stop()

	notifRetry := workers.NewNotificationRetry(db, notificationProvider)
	notifRetry.SetPushSender(pushSender, cfg.BrandName)
	notifRetry.Start()
	defer notifRetry.Stop()

	// Start server in a goroutine
	go func() {
		slog.Info("HTTP server starting", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	slog.Info("shutting down gracefully")

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("server shutdown failed", slog.Any("error", err))
	}

	slog.Info("server stopped")
	return nil
}

func connectDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	return db, nil
}

// parseClientIDs splits a comma-separated OAuth audience allowlist
func parseClientIDs(raw string) []string {
	ids := []string{}
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}
