// Package paynow implements the Paynow Zimbabwe payment gateway adapter.
//
// Paynow API documentation: https://developers.paynow.co.zw
//
// The adapter supports four payment methods:
//   - paynow-ecocash    (EcoCash mobile money push)
//   - paynow-onemoney   (OneMoney mobile money push)
//   - paynow-zipit      (ZIPIT bank transfer)
//   - paynow-card       (Card payment via redirect)
//
// Hash verification follows Paynow's spec: concatenate all field values
// (alphabetical by key) + integration_key, then SHA-512 → uppercase hex.
package paynow

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

const GatewayCode = "paynow"

// Method codes
const (
	MethodEcoCash  = "paynow-ecocash"
	MethodOneMoney = "paynow-onemoney"
	MethodZipit    = "paynow-zipit"
	MethodCard     = "paynow-card"
)

// Adapter implements the gateways.Gateway interface for Paynow Zimbabwe.
type Adapter struct {
	client *Client

	// pollURLs maps Paynow reference → poll URL for status checks.
	// This is an in-memory cache; a service restart clears it.
	// Phase 2 will persist poll URLs in the database.
	pollURLs   map[string]string
	pollURLsMu sync.RWMutex
}

// NewAdapter creates a new Paynow gateway adapter.
func NewAdapter(integrationID, integrationKey, resultURL, returnURL string) *Adapter {
	return &Adapter{
		client: NewClient(integrationID, integrationKey, resultURL, returnURL),
		pollURLs: make(map[string]string),
	}
}

// NewAdapterWithClient creates an adapter with a custom HTTP client (useful for testing).
func NewAdapterWithClient(client *Client) *Adapter {
	return &Adapter{
		client:   client,
		pollURLs: make(map[string]string),
	}
}

// Code returns the gateway code.
func (a *Adapter) Code() string {
	return GatewayCode
}

// Capabilities returns what this gateway supports.
func (a *Adapter) Capabilities() gateways.Capabilities {
	return gateways.Capabilities{
		SupportsRefund:   false,
		SupportsPolling:  true,
		RequiresRedirect: true, // card method uses redirect
		RequiresPhone:    true, // mobile money methods need phone
		WebhookAsync:     true,
	}
}

// SupportedMethods returns available payment methods.
func (a *Adapter) SupportedMethods() []gateways.Method {
	return []gateways.Method{
		{Code: MethodEcoCash, DisplayName: "EcoCash", RequiresPhone: true, RequiresRedirect: false},
		{Code: MethodOneMoney, DisplayName: "OneMoney", RequiresPhone: true, RequiresRedirect: false},
		{Code: MethodZipit, DisplayName: "ZIPIT", RequiresPhone: false, RequiresRedirect: false},
		{Code: MethodCard, DisplayName: "Card Payment", RequiresPhone: false, RequiresRedirect: true},
	}
}

// SupportedCurrencies returns supported currencies.
func (a *Adapter) SupportedCurrencies() []string {
	return []string{"USD", "ZWL", "ZAR"}
}

// ConfigSchema returns the JSON schema for gateway configuration.
func (a *Adapter) ConfigSchema() *gojsonschema.Schema {
	schemaJSON := `{
		"type": "object",
		"properties": {
			"integration_id": {"type": "string", "minLength": 1, "description": "Paynow integration ID"},
			"integration_key": {"type": "string", "minLength": 1, "description": "Paynow integration key"},
			"result_url": {"type": "string", "format": "uri", "description": "Webhook URL for Paynow callbacks"},
			"return_url": {"type": "string", "format": "uri", "description": "Customer return URL after payment"}
		},
		"required": ["integration_id", "integration_key", "result_url", "return_url"]
	}`
	loader := gojsonschema.NewStringLoader(schemaJSON)
	schema, _ := gojsonschema.NewSchema(loader)
	return schema
}

// Initiate starts a new payment transaction with Paynow.
func (a *Adapter) Initiate(ctx context.Context, req gateways.InitiateRequest) (gateways.InitiateResult, error) {
	// Validate method is supported
	methodPaynowCode := a.mapMethodCode(req.MethodCode)
	if !a.isSupportedMethod(req.MethodCode) {
		return gateways.InitiateResult{}, fmt.Errorf("unsupported payment method: %s", req.MethodCode)
	}

	if req.MethodCode == MethodEcoCash || req.MethodCode == MethodOneMoney {
		if req.CustomerPhone == "" {
			return gateways.InitiateResult{}, fmt.Errorf("customer_phone is required for %s", req.MethodCode)
		}
	}

	// Build initiate request
	amountStr := fmt.Sprintf("%.2f", req.Amount.Float64())
	reference := req.IdempotencyKey
	if reference == "" {
		reference = fmt.Sprintf("txn-%d", time.Now().Unix())
	}

	initiateReq := InitiateTransactionRequest{
		Reference:      reference,
		Amount:         amountStr,
		AdditionalInfo: req.Metadata["description"],
		AuthEmail:      req.CustomerEmail,
		Method:         methodPaynowCode,
		Phone:          req.CustomerPhone,
	}

	initiateResp, err := a.client.InitiateTransaction(ctx, initiateReq)
	if err != nil {
		return gateways.InitiateResult{}, fmt.Errorf("paynow initiate failed: %w", err)
	}

	if initiateResp.Status != "Ok" {
		return gateways.InitiateResult{}, fmt.Errorf("paynow error: %s", initiateResp.ErrorMessage)
	}

	// Cache poll URL for status checks
	a.pollURLsMu.Lock()
	a.pollURLs[initiateResp.PaynowReference] = initiateResp.PollURL
	a.pollURLsMu.Unlock()

	result := gateways.InitiateResult{
		ExternalReference: initiateResp.PaynowReference,
		State:             "pending",
		Metadata: map[string]string{
			"poll_url": initiateResp.PollURL,
			"instructions": initiateResp.Instructions,
		},
	}

	// Card method returns a redirect URL
	if req.MethodCode == MethodCard {
		result.RedirectURL = initiateResp.BrowserURL
	}

	return result, nil
}

// Status checks the current status of a transaction using its poll URL.
func (a *Adapter) Status(ctx context.Context, externalRef string) (gateways.StatusResult, error) {
	// Retrieve poll URL from cache
	a.pollURLsMu.RLock()
	pollURL, ok := a.pollURLs[externalRef]
	a.pollURLsMu.RUnlock()

	if !ok {
		// If not in cache, treat externalRef as the poll URL itself
		// (useful if the service stored it and passes it back)
		if strings.HasPrefix(externalRef, "http") {
			pollURL = externalRef
		} else {
			return gateways.StatusResult{}, fmt.Errorf("poll URL not found for reference: %s", externalRef)
		}
	}

	statusResp, err := a.client.CheckStatus(ctx, pollURL)
	if err != nil {
		return gateways.StatusResult{}, fmt.Errorf("paynow status check failed: %w", err)
	}

	state := mapPaynowStatus(statusResp.Status)
	amountCents := int64(statusResp.Amount * 100)

	return gateways.StatusResult{
		State:         state,
		Amount:        gateways.Money{Amount: amountCents, Currency: statusResp.Currency},
		Currency:      statusResp.Currency,
		FailureReason: statusResp.FailureReason,
	}, nil
}

// VerifyWebhook validates and parses a Paynow webhook payload.
func (a *Adapter) VerifyWebhook(ctx context.Context, headers http.Header, body []byte) (gateways.WebhookEvent, error) {
	return a.client.VerifyWebhook(ctx, headers, body)
}

// Refund processes a refund. Paynow Zimbabwe does not support refunds via API.
func (a *Adapter) Refund(ctx context.Context, externalRef string, amount gateways.Money) (gateways.RefundResult, error) {
	return gateways.RefundResult{}, fmt.Errorf("paynow does not support refunds via API")
}

// isSupportedMethod returns true if the given method code is supported by this adapter.
func (a *Adapter) isSupportedMethod(code string) bool {
	switch code {
	case MethodEcoCash, MethodOneMoney, MethodZipit, MethodCard:
		return true
	default:
		return false
	}
}

// mapMethodCode maps our method codes to Paynow's express checkout method codes.
func (a *Adapter) mapMethodCode(code string) string {
	switch code {
	case MethodEcoCash:
		return "ecocash"
	case MethodOneMoney:
		return "onemoney"
	case MethodZipit:
		return "zimdef"
	case MethodCard:
		return "" // card uses standard redirect, no express method
	default:
		return ""
	}
}

// mapPaynowStatus maps Paynow status strings to our state machine states.
func mapPaynowStatus(paynowStatus string) string {
	switch strings.ToLower(paynowStatus) {
	case "paid", "delivered", "awaiting delivery":
		return "completed"
	case "created", "sent", "pending":
		return "pending"
	case "cancelled":
		return "cancelled"
	case "failed", "disputed", "refunded":
		return "failed"
	default:
		return "pending"
	}
}
