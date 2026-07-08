package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	"github.com/freeradius/payments-api/internal/notify"
	"github.com/freeradius/payments-api/internal/notify/africastalking"
	notifymock "github.com/freeradius/payments-api/internal/notify/mock"
	"github.com/freeradius/payments-api/internal/notify/twilio"
	"github.com/freeradius/payments-api/internal/payments"
	"github.com/freeradius/payments-api/internal/workers"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load configuration", slog.Any("error", err))
		os.Exit(1)
	}

	slog.Info("starting payments-api",
		slog.String("app_env", cfg.AppEnv),
		slog.Int("port", cfg.Port),
	)

	// Connect to database
	db, err := connectDB(cfg.DBDSN)
	if err != nil {
		slog.Error("failed to connect to database", slog.Any("error", err))
		os.Exit(1)
	}
	defer db.Close()

	// Verify database connection
	if err := db.Ping(); err != nil {
		slog.Error("database ping failed", slog.Any("error", err))
		os.Exit(1)
	}

	slog.Info("database connected")

	// Create gateway registry
	registry := gateways.NewRegistry()

	// Register mock gateway if enabled
	if cfg.MockGatewayEnabled {
		slog.Info("registering mock gateway")
		mockAdapter := mock.NewAdapter(cfg.MockGatewayWebhookSecret, cfg.MockReturnURL)
		mockAdapter.SetWebhookBaseURL(fmt.Sprintf("http://localhost:%d", cfg.Port))
		if err := registry.Register(mockAdapter); err != nil {
			slog.Error("failed to register mock gateway", slog.Any("error", err))
			os.Exit(1)
		}

		// In dev mode, register a mock adapter for the ecocash gateway when real
		// EcoCash credentials aren't configured. This lets the customer app show
		// and test the full EcoCash checkout flow.
		if cfg.EcoCashAPIKey == "" || cfg.EcoCashMerchantCode == "" {
			slog.Info("registering mock adapter for ecocash gateway (dev mode)")
			ecocashMock := mock.NewAdapterWithCode("ecocash", cfg.MockGatewayWebhookSecret, cfg.MockReturnURL)
			ecocashMock.SetWebhookBaseURL(fmt.Sprintf("http://localhost:%d", cfg.Port))
			if err := registry.Register(ecocashMock); err != nil {
				slog.Error("failed to register mock ecocash adapter", slog.Any("error", err))
				os.Exit(1)
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
			slog.Error("failed to register paynow gateway", slog.Any("error", err))
			os.Exit(1)
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
			slog.Error("failed to register ecocash gateway", slog.Any("error", err))
			os.Exit(1)
		}
	}

	// Create notification provider
	var notificationProvider notify.Provider
	switch cfg.NotifyProvider {
	case "twilio":
		p, err := twilio.NewTwilioProvider(cfg.TwilioAccountSID, cfg.TwilioAuthToken, cfg.TwilioPhoneNumber)
		if err != nil {
			slog.Error("failed to create twilio provider", slog.Any("error", err))
			os.Exit(1)
		}
		notificationProvider = p
	case "africastalking", "at":
		p, err := africastalking.NewATProvider(cfg.ATUsername, cfg.ATAPIKey)
		if err != nil {
			slog.Error("failed to create africastalking provider", slog.Any("error", err))
			os.Exit(1)
		}
		notificationProvider = p
	case "mock":
		notificationProvider = notifymock.NewMockProvider()
	default:
		slog.Error("unknown notification provider", slog.String("provider", cfg.NotifyProvider))
		os.Exit(1)
	}
	slog.Info("notification provider configured", slog.String("provider", notificationProvider.Name()))

	// Create fulfillment service
	fulfillmentService := fulfillment.NewService(db, notificationProvider)

	// Create Django internal API client
	djangoClient := django.NewClient(cfg.DjangoBaseURL, cfg.DjangoInternalAPIKey)

	// Create payment service with fulfillment integration
	paymentService := payments.NewService(db, registry, fulfillmentService, djangoClient, cfg.DefaultCurrency)

	// Create admin auth middleware
	adminAuth := httpapi.AdminHMACMiddleware(cfg.AdminHMACSecret)

	// Create HTTP router
	router := httpapi.NewRouter(paymentService, registry, adminAuth)
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
