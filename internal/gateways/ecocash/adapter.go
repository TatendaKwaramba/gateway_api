package ecocash

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/freeradius/payments-api/internal/gateways"
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

	// msisdnCache maps external reference (EcoCash transactionId) → lookup
	// context for status queries. EcoCash lookup requires MSISDN +
	// clientCorrelator; we capture both from the Initiate response.
	msisdnCache   map[string]cacheEntry
	msisdnCacheMu sync.RWMutex
}

// cacheEntry holds the per-transaction status-lookup context.
// correlator is empty for legacy entries where only the MSISDN is known;
// CustomerLookup then falls back to using the external reference as the
// correlator.
type cacheEntry struct {
	msisdn    string
	correlator string
}

// NewAdapter creates a new EcoCash gateway adapter.
func NewAdapter(apiKey, merchantCode, merchantPin, merchantNumber, terminalID, baseURL, notifyURL, returnURL string) *Adapter {
	client := NewClient(apiKey, merchantCode, merchantPin, merchantNumber, terminalID, baseURL)
	return &Adapter{
		client:      client,
		notifyURL:   notifyURL,
		returnURL:   returnURL,
		msisdnCache: make(map[string]cacheEntry),
	}
}

// NewAdapterWithClient creates an adapter with a custom HTTP client (useful for testing).
func NewAdapterWithClient(client *Client, notifyURL, returnURL string) *Adapter {
	return &Adapter{
		client:      client,
		notifyURL:   notifyURL,
		returnURL:   returnURL,
		msisdnCache: make(map[string]cacheEntry),
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
//
// USD and ZiG only. ZWL (legacy Zimbabwe dollar) is dropped — EcoCash wallets
// operate in USD and ZiG (ZWG). The string "ZiG" deliberately matches
// services_supportedcurrency.code so gateway-currency links resolve without
// aliasing (ISO ZWG naming documented in docs/payments/payment-gateway-modular-plan.md).
func (a *Adapter) SupportedCurrencies() []string {
	return []string{"USD", "ZiG"}
}

// ConfigSchema returns the JSON schema for gateway configuration.
func (a *Adapter) ConfigSchema() string {
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
	return schemaJSON
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

	// Cache lookup context for status (msisdn + clientCorrelator). Prefer the
	// echoed correlator from the charge response; fall back to the locally
	// built one when the response omits it.
	correlator := chargeResp.ClientCorrelator
	if correlator == "" {
		correlator = clientCorrelator
	}
	a.msisdnCacheMu.Lock()
	a.msisdnCache[chargeResp.TransactionID] = cacheEntry{
		msisdn:     chargeResp.EndUserMSISN,
		correlator: correlator,
	}
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
	// Retrieve lookup context from cache
	a.msisdnCacheMu.RLock()
	entry, ok := a.msisdnCache[externalRef]
	a.msisdnCacheMu.RUnlock()

	if !ok {
		return gateways.StatusResult{}, fmt.Errorf("lookup context not found for reference: %s (cache cold — use webhook or poll)", externalRef)
	}

	// EcoCash lookup is keyed by MSISDN + clientCorrelator. Fresh entries
	// carry the correlator echoed at initiate; legacy entries fall back to
	// using the external reference as the correlator.
	correlator := entry.correlator
	if correlator == "" {
		correlator = externalRef
	}
	lookupResp, err := a.client.Lookup(ctx, entry.msisdn, correlator)
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

// PreloadStatusData implements gateways.StatusCachePreloader. Rehydrates the
// per-transaction lookup context from the persisted transaction row so a
// restart-cold adapter can still poll pending EcoCash transactions:
//   - MSISDN: from the transaction's customer_phone (normalized the same way
//     as at initiate — normalizeMSISDN is idempotent)
//   - clientCorrelator: from the gateway_response metadata persisted
//     at initiate (InitiateResult.Metadata["clientCorrelator"])
func (a *Adapter) PreloadStatusData(externalRef, customerPhone, initiateResponseJSON string) {
	if externalRef == "" {
		return
	}

	entry := cacheEntry{}

	if customerPhone != "" {
		entry.msisdn = normalizeMSISDN(customerPhone)
	}

	if initiateResponseJSON != "" {
		// gateway_response is a flat map (request metadata merged with
		// state/external_reference/gateway metadata). The correlator lives
		// at the top level once the service merges InitiateResult.Metadata;
		// accept a nested {"metadata": {...}} shape defensively.
		var persisted map[string]interface{}
		if err := json.Unmarshal([]byte(initiateResponseJSON), &persisted); err == nil {
			if v, ok := persisted["clientCorrelator"].(string); ok && v != "" {
				entry.correlator = v
			} else if nested, ok := persisted["metadata"].(map[string]interface{}); ok {
				if v, ok := nested["clientCorrelator"].(string); ok && v != "" {
					entry.correlator = v
				}
			}
		}
	}

	if entry.msisdn == "" && entry.correlator == "" {
		return
	}

	a.msisdnCacheMu.Lock()
	// Never clobber a fresher webhook-driven entry: preload only fills gaps.
	if _, exists := a.msisdnCache[externalRef]; !exists {
		a.msisdnCache[externalRef] = entry
	}
	a.msisdnCacheMu.Unlock()
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
