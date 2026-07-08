// Package mock implements a mock payment gateway for testing
package mock

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/google/uuid"
	"github.com/xeipuuv/gojsonschema"
)

const (
	// GatewayCode is the unique identifier for this gateway
	GatewayCode = "mock"
	
	// Method codes
	MethodInstant       = "mock-instant"
	MethodEcoCash       = "mock-ecocash"
	MethodCardRedirect  = "mock-card-redirect"
	MethodSlow          = "mock-slow"
	MethodFlaky         = "mock-flaky"
)

// Adapter implements the gateways.Gateway interface for testing
type Adapter struct {
	code           string
	webhookSecret  string
	returnURL      string
	webhookBaseURL string
	
	// In-memory store for pending transactions (in production, this would be Redis/database)
	transactions map[string]*Transaction
}

// Transaction represents a mock transaction
type Transaction struct {
	ID                string
	ExternalReference string
	State             string
	Amount            gateways.Money
	MethodCode        string
	CustomerPhone     string
	CreatedAt         time.Time
	WebhookURL        string
}

// NewAdapter creates a new mock gateway adapter with the default "mock" code.
func NewAdapter(webhookSecret, returnURL string) *Adapter {
	return NewAdapterWithCode(GatewayCode, webhookSecret, returnURL)
}

// NewAdapterWithCode creates a mock gateway adapter with a custom gateway code.
// This allows simulating other gateway workflows (e.g., "ecocash") during development.
func NewAdapterWithCode(code, webhookSecret, returnURL string) *Adapter {
	return &Adapter{
		code:         code,
		webhookSecret: webhookSecret,
		returnURL:    returnURL,
		transactions: make(map[string]*Transaction),
	}
}

// SetWebhookBaseURL configures the base URL for mock webhook delivery.
// Should be set to the payments API base URL, e.g. "http://localhost:8080"
func (a *Adapter) SetWebhookBaseURL(baseURL string) {
	a.webhookBaseURL = baseURL
}

// Code returns the gateway code
func (a *Adapter) Code() string {
	return a.code
}

// Capabilities returns what this gateway supports
func (a *Adapter) Capabilities() gateways.Capabilities {
	return gateways.Capabilities{
		SupportsRefund:   true,
		SupportsPolling:  true,
		RequiresRedirect: true,
		RequiresPhone:    true,
		WebhookAsync:     true,
	}
}

// SupportedMethods returns available payment methods
func (a *Adapter) SupportedMethods() []gateways.Method {
	methods := []gateways.Method{
		{Code: MethodInstant, DisplayName: "Mock Instant", RequiresPhone: false, RequiresRedirect: false},
		{Code: MethodEcoCash, DisplayName: "Mock EcoCash", RequiresPhone: true, RequiresRedirect: false},
		{Code: MethodCardRedirect, DisplayName: "Mock Card (Redirect)", RequiresPhone: false, RequiresRedirect: true},
		{Code: MethodSlow, DisplayName: "Mock Slow", RequiresPhone: true, RequiresRedirect: false},
		{Code: MethodFlaky, DisplayName: "Mock Flaky", RequiresPhone: true, RequiresRedirect: false},
	}
	// When emulating the ecocash gateway, expose the real method code so the
	// customer app's method picker (which queries the DB) finds a matching adapter.
	if a.code == "ecocash" {
		methods = append(methods, gateways.Method{
			Code: "ecocash-ecocash", DisplayName: "EcoCash",
			RequiresPhone: true, RequiresRedirect: false,
		})
	}
	return methods
}

// SupportedCurrencies returns supported currencies
func (a *Adapter) SupportedCurrencies() []string {
	return []string{"USD", "ZWL", "ZAR"}
}

// ConfigSchema returns the JSON schema for configuration
func (a *Adapter) ConfigSchema() *gojsonschema.Schema {
	schemaJSON := `{
		"type": "object",
		"properties": {
			"webhook_secret": {"type": "string", "minLength": 32},
			"return_url": {"type": "string", "format": "uri"}
		},
		"required": ["webhook_secret"]
	}`
	
	loader := gojsonschema.NewStringLoader(schemaJSON)
	schema, _ := gojsonschema.NewSchema(loader)
	return schema
}

// Initiate starts a new payment
func (a *Adapter) Initiate(ctx context.Context, req gateways.InitiateRequest) (gateways.InitiateResult, error) {
	// Generate external reference
	externalRef := uuid.New().String()
	
	// Determine outcome based on magic values
	outcome := a.determineOutcome(req.CustomerPhone, req.Amount.Amount)
	
	// Create transaction
	tx := &Transaction{
		ID:                uuid.New().String(),
		ExternalReference: externalRef,
		State:             "pending",
		Amount:            req.Amount,
		MethodCode:        req.MethodCode,
		CustomerPhone:     req.CustomerPhone,
		CreatedAt:         time.Now(),
	}
	
	a.transactions[externalRef] = tx
	
	// Handle immediate outcomes
	result := gateways.InitiateResult{
		ExternalReference: externalRef,
		State:             "pending",
	}
	
	switch outcome {
	case "instant-success":
		tx.State = "completed"
		result.State = "completed"
		result.RedirectURL = ""
		
	case "instant-fail":
		tx.State = "failed"
		result.State = "failed"
		result.RedirectURL = ""
		
	case "network-error":
		return gateways.InitiateResult{}, fmt.Errorf("network error: connection refused")
		
	case "slow-initiate":
		// Simulate slow response
		time.Sleep(10 * time.Second)
		
	default:
		// Async methods - schedule webhook
		go a.scheduleWebhook(externalRef, outcome)
		
		// Set redirect URL for redirect methods
		if req.MethodCode == MethodCardRedirect {
			result.RedirectURL = fmt.Sprintf("%s/mock/checkout/%s", a.returnURL, externalRef)
		}
	}
	
	return result, nil
}

// Status checks the current status
func (a *Adapter) Status(ctx context.Context, externalRef string) (gateways.StatusResult, error) {
	tx, ok := a.transactions[externalRef]
	if !ok {
		return gateways.StatusResult{}, fmt.Errorf("transaction not found: %s", externalRef)
	}
	
	return gateways.StatusResult{
		State:    tx.State,
		Amount:   tx.Amount,
		Currency: tx.Amount.Currency,
	}, nil
}

// VerifyWebhook validates webhook signature
func (a *Adapter) VerifyWebhook(ctx context.Context, headers http.Header, body []byte) (gateways.WebhookEvent, error) {
	// Get signature from header
	sig := headers.Get("X-Mock-Signature")
	if sig == "" {
		return gateways.WebhookEvent{}, fmt.Errorf("missing X-Mock-Signature header")
	}
	
	// Get timestamp from header
	timestamp := headers.Get("X-Mock-Timestamp")
	if timestamp == "" {
		return gateways.WebhookEvent{}, fmt.Errorf("missing X-Mock-Timestamp header")
	}
	
	// Verify timestamp is within 5 minute window
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return gateways.WebhookEvent{}, fmt.Errorf("invalid timestamp: %w", err)
	}
	
	now := time.Now().Unix()
	if now-ts > 300 { // 5 minutes
		return gateways.WebhookEvent{}, fmt.Errorf("webhook timestamp too old")
	}
	
	// Verify signature
	expectedSig := a.computeSignature(timestamp, body)
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return gateways.WebhookEvent{}, fmt.Errorf("invalid signature")
	}
	
	// Parse webhook payload
	var payload struct {
		ExternalReference string            `json:"external_reference"`
		State             string            `json:"state"`
		Amount            int64             `json:"amount"`
		Currency          string            `json:"currency"`
		EventType         string            `json:"event_type"`
		Metadata          map[string]string `json:"metadata"`
	}
	
	if err := json.Unmarshal(body, &payload); err != nil {
		return gateways.WebhookEvent{}, fmt.Errorf("failed to parse webhook body: %w", err)
	}
	
	return gateways.WebhookEvent{
		ExternalReference: payload.ExternalReference,
		State:             payload.State,
		Amount: gateways.Money{
			Amount:   payload.Amount,
			Currency: payload.Currency,
		},
		Currency:  payload.Currency,
		EventType: payload.EventType,
		Metadata:  payload.Metadata,
	}, nil
}

// Refund processes a refund
func (a *Adapter) Refund(ctx context.Context, externalRef string, amount gateways.Money) (gateways.RefundResult, error) {
	tx, ok := a.transactions[externalRef]
	if !ok {
		// Transaction not in memory (e.g. after container restart).
		// Accept the refund anyway — the real state transition happens in the DB.
		slog.Warn("mock adapter: transaction not found in memory, assuming completed for refund",
			slog.String("external_ref", externalRef),
		)
		return gateways.RefundResult{
			ExternalReference: externalRef + "_refund",
			State:             "completed",
		}, nil
	}
	
	if tx.State != "completed" {
		return gateways.RefundResult{}, fmt.Errorf("cannot refund transaction in state: %s", tx.State)
	}
	
	tx.State = "refunded"
	
	return gateways.RefundResult{
		ExternalReference: externalRef + "_refund",
		State:             "completed",
	}, nil
}

// determineOutcome determines the payment outcome based on magic values
func (a *Adapter) determineOutcome(phone string, amountCents int64) string {
	// Phone suffix triggers
	if phone != "" {
		suffix := getLastNChars(phone, 4)
		switch suffix {
		case "0001":
			return "instant-success"
		case "0002":
			return "async-success"
		case "0003":
			return "fail-insufficient-funds"
		case "0004":
			return "fail-declined"
		case "0005":
			return "pending-forever"
		case "0006":
			return "invalid-signature"
		case "0007":
			return "replay-test"
		case "0008":
			return "network-error"
		case "0009":
			return "slow-initiate"
		}
	}
	
	// Amount decimal triggers
	amountStr := fmt.Sprintf("%d", amountCents)
	if strings.HasSuffix(amountStr, "13") {
		return "chargeback-simulation"
	}
	if strings.HasSuffix(amountStr, "99") {
		return "fulfillment-failure"
	}
	
	// Default: async success after 3 seconds
	return "async-success"
}

// scheduleWebhook schedules an async webhook delivery
func (a *Adapter) scheduleWebhook(externalRef, outcome string) {
	// Determine delay and final state based on outcome
	var delay time.Duration
	var finalState string
	var eventType string
	
	switch outcome {
	case "async-success":
		delay = 3 * time.Second
		finalState = "completed"
		eventType = "payment.completed"
	case "fail-insufficient-funds":
		delay = 2 * time.Second
		finalState = "failed"
		eventType = "payment.failed"
	case "fail-declined":
		delay = 2 * time.Second
		finalState = "failed"
		eventType = "payment.failed"
	case "slow":
		delay = 30 * time.Second
		finalState = "completed"
		eventType = "payment.completed"
	case "chargeback-simulation":
		delay = 10 * time.Second
		finalState = "refunded"
		eventType = "payment.refunded"
	default:
		delay = 3 * time.Second
		finalState = "completed"
		eventType = "payment.completed"
	}
	
	// Schedule webhook
	time.AfterFunc(delay, func() {
		tx, ok := a.transactions[externalRef]
		if !ok {
			return
		}
		
		tx.State = finalState
		
		// Deliver webhook if base URL is configured
		if a.webhookBaseURL != "" {
			a.deliverWebhook(tx, finalState, eventType)
		}
	})
}

// deliverWebhook POSTs a signed webhook to the payments API
func (a *Adapter) deliverWebhook(tx *Transaction, state, eventType string) {
	payload := map[string]interface{}{
		"external_reference": tx.ExternalReference,
		"state":              state,
		"amount":             tx.Amount.Amount,
		"currency":           tx.Amount.Currency,
		"event_type":         eventType,
		"metadata":           map[string]string{"source": "mock-gateway"},
	}
	
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	sig := a.computeSignature(timestamp, body)
	
	webhookURL := fmt.Sprintf("%s/webhooks/%s", a.webhookBaseURL, a.code)
	req, err := http.NewRequest(http.MethodPost, webhookURL, strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mock-Signature", sig)
	req.Header.Set("X-Mock-Timestamp", timestamp)
	
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}

// computeSignature computes HMAC-SHA256 signature for webhook
func (a *Adapter) computeSignature(timestamp string, body []byte) string {
	message := timestamp + "." + string(body)
	h := hmac.New(sha256.New, []byte(a.webhookSecret))
	h.Write([]byte(message))
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeWebhookSignature exports computeSignature for use by HTTP handlers.
func (a *Adapter) ComputeWebhookSignature(timestamp string, body []byte) string {
	return a.computeSignature(timestamp, body)
}

// getLastNChars returns the last n characters of a string
func getLastNChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// Admin methods for test control

// CompleteTransaction manually completes a transaction (for testing)
func (a *Adapter) CompleteTransaction(externalRef string) error {
	tx, ok := a.transactions[externalRef]
	if !ok {
		return fmt.Errorf("transaction not found: %s", externalRef)
	}
	tx.State = "completed"
	return nil
}

// FailTransaction manually fails a transaction (for testing)
func (a *Adapter) FailTransaction(externalRef string) error {
	tx, ok := a.transactions[externalRef]
	if !ok {
		return fmt.Errorf("transaction not found: %s", externalRef)
	}
	tx.State = "failed"
	return nil
}

// RefundTransaction manually refunds a transaction (for testing)
func (a *Adapter) RefundTransaction(externalRef string) error {
	tx, ok := a.transactions[externalRef]
	if !ok {
		return fmt.Errorf("transaction not found: %s", externalRef)
	}
	if tx.State != "completed" {
		return fmt.Errorf("cannot refund transaction in state: %s", tx.State)
	}
	tx.State = "refunded"
	return nil
}

// GetTransaction returns a transaction by external reference (for testing)
func (a *Adapter) GetTransaction(externalRef string) (*Transaction, bool) {
	tx, ok := a.transactions[externalRef]
	return tx, ok
}

// TransactionListItem is a serializable summary of a mock transaction.
type TransactionListItem struct {
	ExternalReference string          `json:"external_reference"`
	State             string          `json:"state"`
	Amount            int64           `json:"amount"`
	Currency          string          `json:"currency"`
	MethodCode        string          `json:"method_code"`
	CustomerPhone     string          `json:"customer_phone"`
	CreatedAt         time.Time       `json:"created_at"`
}

// ListTransactions returns all transactions stored in the adapter.
func (a *Adapter) ListTransactions() []TransactionListItem {
	items := make([]TransactionListItem, 0, len(a.transactions))
	for _, tx := range a.transactions {
		items = append(items, TransactionListItem{
			ExternalReference: tx.ExternalReference,
			State:             tx.State,
			Amount:            tx.Amount.Amount,
			Currency:          tx.Amount.Currency,
			MethodCode:        tx.MethodCode,
			CustomerPhone:     tx.CustomerPhone,
			CreatedAt:         tx.CreatedAt,
		})
	}
	return items
}
