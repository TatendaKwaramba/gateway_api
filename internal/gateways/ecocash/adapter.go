package ecocash

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/xeipuuv/gojsonschema"
)

const GatewayCode = "ecocash"

// Method codes
const (
	MethodEcoCash = "ecocash-ecocash"
)

// Adapter implements the gateways.Gateway interface for EcoCash Zimbabwe.
type Adapter struct {
	client    *Client
	notifyURL string
	returnURL string

	// msisdnCache maps external reference → MSISDN for status lookups.
	// EcoCash lookup requires MSISDN + clientCorrelator; we cache MSISDN from Initiate.
	msisdnCache   map[string]string
	msisdnCacheMu sync.RWMutex
}

// NewAdapter creates a new EcoCash gateway adapter.
func NewAdapter(apiKey, merchantCode, merchantPin, merchantNumber, terminalID, baseURL, notifyURL, returnURL string) *Adapter {
	client := NewClient(apiKey, merchantCode, merchantPin, merchantNumber, terminalID, baseURL)
	return &Adapter{
		client:     client,
		notifyURL:  notifyURL,
		returnURL:  returnURL,
		msisdnCache: make(map[string]string),
	}
}

// NewAdapterWithClient creates an adapter with a custom HTTP client (useful for testing).
func NewAdapterWithClient(client *Client, notifyURL, returnURL string) *Adapter {
	return &Adapter{
		client:     client,
		notifyURL:  notifyURL,
		returnURL:  returnURL,
		msisdnCache: make(map[string]string),
	}
}

// Code returns the gateway code.
func (a *Adapter) Code() string {
	return GatewayCode
}

// Capabilities returns what this gateway supports.
func (a *Adapter) Capabilities() gateways.Capabilities {
	return gateways.Capabilities{
		SupportsRefund:   true,
		SupportsPolling:  true,
		RequiresRedirect: false,
		RequiresPhone:    true,
		WebhookAsync:     true,
	}
}

// SupportedMethods returns available payment methods.
func (a *Adapter) SupportedMethods() []gateways.Method {
	return []gateways.Method{
		{Code: MethodEcoCash, DisplayName: "EcoCash", RequiresPhone: true, RequiresRedirect: false},
	}
}

// SupportedCurrencies returns supported currencies.
func (a *Adapter) SupportedCurrencies() []string {
	return []string{"USD", "ZWL"}
}

// ConfigSchema returns the JSON schema for gateway configuration.
func (a *Adapter) ConfigSchema() *gojsonschema.Schema {
	schemaJSON := `{
		"type": "object",
		"properties": {
			"api_key": {"type": "string", "minLength": 1, "description": "EcoCash developer portal API key"},
			"merchant_code": {"type": "string", "minLength": 1, "description": "EcoCash-assigned merchant code"},
			"merchant_pin": {"type": "string", "minLength": 1, "description": "Merchant security PIN"},
			"merchant_number": {"type": "string", "minLength": 1, "description": "Merchant registered MSISDN"},
			"terminal_id": {"type": "string", "description": "POS terminal identifier"},
			"base_url": {"type": "string", "format": "uri", "description": "EcoCash API base URL (sandbox or production)"},
			"notify_url": {"type": "string", "format": "uri", "description": "Webhook callback URL for payment notifications"},
			"return_url": {"type": "string", "format": "uri", "description": "Customer return URL after payment"}
		},
		"required": ["api_key", "merchant_code", "merchant_pin", "merchant_number"]
	}`
	loader := gojsonschema.NewStringLoader(schemaJSON)
	schema, _ := gojsonschema.NewSchema(loader)
	return schema
}

// Initiate starts a new payment transaction with EcoCash.
func (a *Adapter) Initiate(ctx context.Context, req gateways.InitiateRequest) (gateways.InitiateResult, error) {
	if !a.isSupportedMethod(req.MethodCode) {
		return gateways.InitiateResult{}, fmt.Errorf("unsupported payment method: %s", req.MethodCode)
	}

	if req.CustomerPhone == "" {
		return gateways.InitiateResult{}, fmt.Errorf("customer_phone is required for %s", req.MethodCode)
	}

	// Build client correlator from idempotency key or timestamp
	clientCorrelator := req.IdempotencyKey
	if clientCorrelator == "" {
		clientCorrelator = fmt.Sprintf("%d", time.Now().Unix())
	}

	// Build reference code
	referenceCode := req.Metadata["reference"]
	if referenceCode == "" {
		referenceCode = clientCorrelator
	}

	// Amount as decimal string
	amountStr := fmt.Sprintf("%.2f", req.Amount.Float64())

	description := req.Metadata["description"]
	if description == "" {
		description = "Payment"
	}

	chargeReq := ChargeRequest{
		ClientCorrelator: clientCorrelator,
		ReferenceCode:    referenceCode,
		EndUserMSISN:     normalizeMSISDN(req.CustomerPhone),
		TranType:         "MCR",
		Remarks:          description,
		ChargingInformation: ChargingInformation{
			Amount:      amountStr,
			Currency:    req.Currency,
			Description: description,
		},
		ChargeMetaData: ChargeMetaData{
			Channel: "Online",
		},
		NotifyURL: a.notifyURL,
	}

	chargeResp, err := a.client.Charge(ctx, chargeReq)
	if err != nil {
		return gateways.InitiateResult{}, fmt.Errorf("ecocash initiate failed: %w", err)
	}

	// Cache MSISDN for status lookups
	a.msisdnCacheMu.Lock()
	a.msisdnCache[chargeResp.TransactionID] = chargeResp.EndUserMSISN
	a.msisdnCacheMu.Unlock()

	// Map status
	state := mapEcoCashStatus(chargeResp.Status)

	result := gateways.InitiateResult{
		ExternalReference: chargeResp.TransactionID,
		State:             state,
		Metadata: map[string]string{
			"clientCorrelator": chargeResp.ClientCorrelator,
			"statusMessage":    chargeResp.StatusMessage,
			"timestamp":        chargeResp.Timestamp,
		},
	}

	return result, nil
}

// Status checks the current status of a transaction.
func (a *Adapter) Status(ctx context.Context, externalRef string) (gateways.StatusResult, error) {
	// Retrieve MSISDN from cache
	a.msisdnCacheMu.RLock()
	msisdn, ok := a.msisdnCache[externalRef]
	a.msisdnCacheMu.RUnlock()

	if !ok {
		return gateways.StatusResult{}, fmt.Errorf("MSISDN not found for reference: %s (cache cold — use webhook or poll)", externalRef)
	}

	// Extract client correlator from external reference
	// EcoCash transactionId format: "PP1204433.1148.78123456"
	// The clientCorrelator was used to create the transaction, so we need to look it up
	// by MSISDN + the external reference works as the correlator in the lookup API
	lookupResp, err := a.client.Lookup(ctx, msisdn, externalRef)
	if err != nil {
		return gateways.StatusResult{}, fmt.Errorf("ecocash status check failed: %w", err)
	}

	state := mapEcoCashStatus(lookupResp.Status)

	var amountCents int64
	if lookupResp.Amount != "" {
		var amountFloat float64
		if _, err := fmt.Sscanf(lookupResp.Amount, "%f", &amountFloat); err == nil {
			amountCents = int64(amountFloat * 100)
		}
	}

	return gateways.StatusResult{
		State:    state,
		Amount:   gateways.Money{Amount: amountCents, Currency: lookupResp.Currency},
		Currency: lookupResp.Currency,
	}, nil
}

// VerifyWebhook validates and parses an EcoCash webhook payload.
func (a *Adapter) VerifyWebhook(ctx context.Context, headers http.Header, body []byte) (gateways.WebhookEvent, error) {
	return a.client.VerifyWebhook(ctx, headers, body)
}

// Refund processes a refund for a completed transaction.
func (a *Adapter) Refund(ctx context.Context, externalRef string, amount gateways.Money) (gateways.RefundResult, error) {
	amountStr := fmt.Sprintf("%.2f", amount.Float64())

	refundReq := RefundRequest{
		TransactionID:    externalRef,
		ClientCorrelator: externalRef,
		RefundAmount:     amountStr,
		Currency:         amount.Currency,
	}

	refundResp, err := a.client.Refund(ctx, refundReq)
	if err != nil {
		return gateways.RefundResult{}, fmt.Errorf("ecocash refund failed: %w", err)
	}

	// A successful refund API call means the reversal was accepted.
	// Use the mapped status, but default to "refunded" if EcoCash returns "Successful"
	// (which would otherwise map to "completed").
	state := mapEcoCashStatus(refundResp.Status)
	if state == "completed" {
		state = "refunded"
	}

	return gateways.RefundResult{
		ExternalReference: refundResp.TransactionID,
		State:             state,
	}, nil
}

// isSupportedMethod returns true if the given method code is supported by this adapter.
func (a *Adapter) isSupportedMethod(code string) bool {
	return code == MethodEcoCash
}

// normalizeMSISDN ensures the phone number is in the format EcoCash expects.
// Strips leading + and country code (263), ensures leading 0 for local format.
func normalizeMSISDN(phone string) string {
	phone = strings.TrimSpace(phone)
	phone = strings.TrimPrefix(phone, "+")

	// Strip Zimbabwe country code
	if strings.HasPrefix(phone, "263") {
		rest := phone[3:]
		if !strings.HasPrefix(rest, "0") {
			phone = "0" + rest
		} else {
			phone = rest
		}
	}

	// Ensure it starts with 0
	if !strings.HasPrefix(phone, "0") {
		phone = "0" + phone
	}

	return phone
}
