// Package ecocash implements the EcoCash Zimbabwe direct payment gateway adapter.
//
// EcoCash API documentation: https://developers.ecocash.co.zw
//
// The adapter supports one payment method:
//   - ecocash-ecocash (EcoCash mobile money push via C2B charge)
//
// Authentication: Basic Auth with API key + merchant PIN.
// Request format: JSON.
// Webhooks: async notification via notifyUrl callback.
package ecocash

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/freeradius/payments-api/internal/gateways"
)

const statePending = "pending"

const (
	sandboxBaseURL    = "https://developers.ecocash.co.zw/sandbox"
	productionBaseURL = "https://developers.ecocash.co.zw"
	chargePath        = "/payment/v1/transactions/amount/"
	refundPath        = "/transactions/refund/"
)

// Client is an HTTP client for the EcoCash API.
type Client struct {
	apiKey            string
	merchantCode      string
	merchantPin       string
	merchantNumber    string
	terminalID        string
	superMerchantName string
	merchantName      string
	location          string
	httpClient        *http.Client
	baseURL           string
	basicAuthToken    string // base64(apiKey + ":" + apiCredential), computed once
}

// NewClient creates a new EcoCash API client.
//
// Basic auth is base64(apiKey:apiCredential) per the EcoCash developer
// docs (username:password). apiCredential is the separate credential
// minted with the API key — NOT the merchant PIN, which travels in the
// charge payload. Without WithAPICredential the legacy behaviour of
// authing with apiKey:merchantPin is kept (older sandboxes).
func NewClient(apiKey, merchantCode, merchantPin, merchantNumber, terminalID, baseURL string) *Client {
	authToken := base64.StdEncoding.EncodeToString(
		[]byte(apiKey + ":" + merchantPin),
	)

	return &Client{
		apiKey:         apiKey,
		merchantCode:   merchantCode,
		merchantPin:    merchantPin,
		merchantNumber: merchantNumber,
		terminalID:     terminalID,
		baseURL:        strings.TrimRight(baseURL, "/"),
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		basicAuthToken: authToken,
	}
}

// NewProductionClient creates a client configured for EcoCash production.
func NewProductionClient(apiKey, merchantCode, merchantPin, merchantNumber, terminalID string) *Client {
	return NewClient(apiKey, merchantCode, merchantPin, merchantNumber, terminalID, productionBaseURL)
}

// WithAPICredential overrides the Basic-auth password with the dedicated
// API credential (the developer-portal password paired with the API key).
// Returns the client for chaining.
func (c *Client) WithAPICredential(credential string) *Client {
	c.basicAuthToken = base64.StdEncoding.EncodeToString(
		[]byte(c.apiKey + ":" + credential),
	)
	return c
}

// SetHTTPClient allows injecting a custom HTTP client (useful for testing).
func (c *Client) SetHTTPClient(client *http.Client) {
	c.httpClient = client
}

// SetBaseURL overrides the API base URL (useful for testing with httptest).
func (c *Client) SetBaseURL(baseURL string) {
	c.baseURL = strings.TrimRight(baseURL, "/")
}

// SetMerchantDetails sets merchant metadata for charge requests.
func (c *Client) SetMerchantDetails(superMerchantName, merchantName string) {
	c.superMerchantName = superMerchantName
	c.merchantName = merchantName
}

// SetLocation sets the charge-request location (EcoCash rejects blank).
func (c *Client) SetLocation(location string) {
	c.location = location
}

// ChargingInformation contains the payment amount and currency.
type ChargingInformation struct {
	Amount      float64 `json:"amount"`
	Currency    string  `json:"currency"`
	Description string  `json:"description"`
}

// ChargeMetaData contains additional metadata for the charge.
type ChargeMetaData struct {
	Channel              string `json:"channel"`
	PurchaseCategoryCode string `json:"purchaseCategoryCode,omitempty"`
	OnBeHalfOf           string `json:"onBeHalfOf,omitempty"`
}

// PaymentAmount groups the charging information and metadata exactly as the
// EcoCash sandbox expects (note the all-lowercase "charginginformation").
type PaymentAmount struct {
	ChargingInformation ChargingInformation `json:"charginginformation"`
	ChargeMetaData      ChargeMetaData      `json:"chargeMetaData"`
}

// ChargeRequest contains the parameters for initiating a C2B charge.
// Field names/casing mirror the documented sandbox sample:
// https://developers.ecocash.co.zw — paymentAmount.charginginformation,
// endUserId, merchantCode/Pin/Number, countryCode, terminalID.
type ChargeRequest struct {
	ClientCorrelator           string        `json:"clientCorrelator"`
	ReferenceCode              string        `json:"referenceCode"`
	TranType                   string        `json:"tranType"`
	EndUserId                  string        `json:"endUserId"`
	PaymentAmount              PaymentAmount `json:"paymentAmount"`
	Location                   string        `json:"location,omitempty"`
	MerchantCode               string        `json:"merchantCode"`
	MerchantPin                string        `json:"merchantPin"`
	MerchantNumber             string        `json:"merchantNumber"`
	CountryCode                string        `json:"countryCode"`
	TerminalID                 string        `json:"terminalID"`
	NotifyURL                  string        `json:"notifyUrl,omitempty"`
	SuperMerchantName          string        `json:"superMerchantName,omitempty"`
	MerchantName               string        `json:"merchantName,omitempty"`
	TransactionOperationStatus string        `json:"transactionOperationStatus,omitempty"`
	Remarks                    string        `json:"remarks,omitempty"`
}

// ChargeResponse contains the response from an EcoCash charge request
// (same wire shape as lookup/refund — see EcoCashTransaction).
type ChargeResponse = EcoCashTransaction

// RefundRequest contains the parameters for a refund/reversal.
type RefundRequest struct {
	TransactionID    string `json:"transactionId"`
	ClientCorrelator string `json:"clientCorrelator"`
	RefundAmount     string `json:"refundAmount"`
	Currency         string `json:"currency"`
	MerchantCode     string `json:"merchantCode"`
	MerchantPin      string `json:"merchantPin"`
}

// RefundResponse contains the response from a refund request.
type RefundResponse = EcoCashTransaction

// LookupResponse contains the result of a transaction status lookup.
type LookupResponse = EcoCashTransaction

// EcoCashTransaction is the wire payload shared by the Charge, Lookup and
// Refund endpoints (field-verified against the sandbox 2026-10-21 with
// correlators amaniconnect-probe-004/005). It differs substantially from
// the older developer-portal examples: the transaction identifier is the
// numeric "id" and status lives in transactionOperationStatus /
// ecocashResponseCode / responseMessage.
type EcoCashTransaction struct {
	ID                         json.Number `json:"id"`
	Version                    int64       `json:"version"`
	ServerReferenceCode        string      `json:"serverReferenceCode"`
	ClientCorrelator           string      `json:"clientCorrelator"`
	ReferenceCode              string      `json:"referenceCode"`
	OriginMerchantReference    string      `json:"orginalMerchantReference"` // sic — EcoCash spells it without the "i"
	EndUserId                  string      `json:"endUserId"`
	TranType                   string      `json:"tranType"`
	TransactionOperationStatus string      `json:"transactionOperationStatus"`
	EcoCashReference           string      `json:"ecocashReference"`
	EcoCashResponseCode        string      `json:"ecocashResponseCode"`
	ResponseCode               string      `json:"responseCode"`
	ResponseMessage            string      `json:"responseMessage"`
	Text                       string      `json:"text"`
	CurrencyCode               string      `json:"currencyCode"`
	MerchantCode               string      `json:"merchantCode"`
	MerchantNumber             string      `json:"merchantNumber"`
	MerchantName               string      `json:"merchantName"`
	SuperMerchantName          string      `json:"superMerchantName"`
	Remarks                    string      `json:"remarks"`
	TransactionDate            int64       `json:"transactionDate"`
	NotifyURL                  string      `json:"notifyUrl"`
	PaymentAmount              struct {
		TotalAmountCharged float64 `json:"totalAmountCharged"`
	} `json:"paymentAmount"`

	// TransactionLegacyID catches notifications sent in the older summary
	// shape ({"transactionId": "..."}) that carry no id/serverReferenceCode.
	TransactionLegacyID string `json:"-"`
}

// TxID returns the transaction identifier for gateway bookkeeping. The
// numeric id is primary; serverReferenceCode is the fallback for payloads
// that omit it.
func (t *EcoCashTransaction) TxID() string {
	if t.ID.String() != "" && t.ID.String() != "0" {
		return t.ID.String()
	}
	if t.ServerReferenceCode != "" {
		return t.ServerReferenceCode
	}
	return t.TransactionLegacyID
}

// Message returns the best human-readable status text.
func (t *EcoCashTransaction) Message() string {
	if t.Text != "" {
		return t.Text
	}
	if t.ResponseMessage != "" {
		return t.ResponseMessage
	}
	return t.TransactionOperationStatus
}

// mapEcoCashTxnState maps a wire transaction payload to our state machine.
// Field-verified states (sandbox):
//   - pending: transactionOperationStatus "PENDING SUBSCRIBER VALIDATION",
//     ecocashResponseCode "PAUSED", responseMessage "PENDING_VALIDATION"
//   - completed: op "COMPLETED", code "SUCCEEDED", ecocashReference set,
//     responseMessage "process service request successfully."
// Failure simulations (insufficient funds, invalid PIN, limit) are mapped
// from the responseMessage / responseCode heuristics below; anything
// unrecognized stays pending so polling/webhook can resolve it later.
func mapEcoCashTxnState(txn EcoCashTransaction) string {
	op := strings.ToLower(strings.TrimSpace(txn.TransactionOperationStatus))
	code := strings.ToLower(strings.TrimSpace(txn.EcoCashResponseCode))
	respCode := strings.ToLower(strings.TrimSpace(txn.ResponseCode))
	msg := strings.ToLower(txn.ResponseMessage + " " + txn.Text + " " + txn.TransactionOperationStatus)

	switch {
	case strings.Contains(op, "pending") || code == "paused" || respCode == "pending":
		return statePending
	case code == "failed" || respCode == "failed":
		return "failed"
	case strings.Contains(msg, "insufficient") ||
		strings.Contains(msg, "invalid pin") ||
		strings.Contains(msg, "limit") ||
		strings.Contains(msg, "failed"):
		return "failed"
	case code == "succeeded" || op == "completed" ||
		op == "successful" || op == "delivered" || op == "success":
		if txn.EcoCashReference != "" {
			return "completed"
		}
		// Completed without succeeded code and without a wallet reference —
		// treat as failed unless the message affirmatively says success.
		if strings.Contains(msg, "successfully") || strings.Contains(msg, "successful") {
			return "completed"
		}
		return "failed"
	case code == "cancelled" || respCode == "cancelled" || op == "cancelled":
		return "cancelled"
	case code == "reversed" || respCode == "reversed" || op == "reversed":
		return "refunded"
	default:
		return statePending
	}
}

// Charge sends a C2B charge request to EcoCash, initiating a USSD PIN push to the customer.
func (c *Client) Charge(ctx context.Context, req ChargeRequest) (*ChargeResponse, error) {
	// Set defaults
	if req.MerchantCode == "" {
		req.MerchantCode = c.merchantCode
	}
	if req.MerchantPin == "" {
		req.MerchantPin = c.merchantPin
	}
	if req.MerchantNumber == "" {
		req.MerchantNumber = c.merchantNumber
	}
	if req.CountryCode == "" {
		req.CountryCode = "ZW"
	}
	if req.TerminalID == "" {
		req.TerminalID = c.terminalID
	}
	if req.Location == "" {
		req.Location = c.location
	}
	if req.SuperMerchantName == "" {
		req.SuperMerchantName = c.superMerchantName
	}
	if req.MerchantName == "" {
		req.MerchantName = c.merchantName
	}
	if req.TranType == "" {
		req.TranType = "MCR"
	}
	if req.PaymentAmount.ChargeMetaData.Channel == "" {
		req.PaymentAmount.ChargeMetaData.Channel = "WEB"
	}
	if req.TransactionOperationStatus == "" {
		req.TransactionOperationStatus = "Charged"
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal charge request: %w", err)
	}

	url := c.baseURL + chargePath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Basic "+c.basicAuthToken)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("charge request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	slog.Debug("ecocash charge response",
		slog.Int("status_code", resp.StatusCode),
		slog.String("body", string(respBody)),
	)

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("ecocash unauthorized — check API key and merchant PIN")
	}
	if resp.StatusCode == http.StatusBadRequest {
		return nil, fmt.Errorf("ecocash bad request: %s", string(respBody))
	}
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return nil, fmt.Errorf("ecocash business rule violation: %s", string(respBody))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("ecocash server error (%d): %s", resp.StatusCode, string(respBody))
	}

	var chargeResp ChargeResponse
	if err := json.Unmarshal(respBody, &chargeResp); err != nil {
		return nil, fmt.Errorf("failed to parse charge response: %w", err)
	}
	legacyTxnFill(&chargeResp, respBody)

	return &chargeResp, nil
}

// Lookup queries the status of a transaction by MSISDN and client correlator.
func (c *Client) Lookup(ctx context.Context, msisdn, clientCorrelator string) (*LookupResponse, error) {
	url := fmt.Sprintf("%s/%s/transactions/amount/%s", c.baseURL, msisdn, clientCorrelator)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build lookup request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Basic "+c.basicAuthToken)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("lookup request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read lookup response: %w", err)
	}

	slog.Debug("ecocash lookup response",
		slog.Int("status_code", resp.StatusCode),
		slog.String("body", string(respBody)),
	)

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("transaction not found: %s/%s", msisdn, clientCorrelator)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ecocash lookup error (%d): %s", resp.StatusCode, string(respBody))
	}

	var lookupResp LookupResponse
	if err := json.Unmarshal(respBody, &lookupResp); err != nil {
		return nil, fmt.Errorf("failed to parse lookup response: %w", err)
	}
	legacyTxnFill(&lookupResp, respBody)

	return &lookupResp, nil
}

// Refund reverses a completed transaction.
func (c *Client) Refund(ctx context.Context, req RefundRequest) (*RefundResponse, error) {
	if req.MerchantCode == "" {
		req.MerchantCode = c.merchantCode
	}
	if req.MerchantPin == "" {
		req.MerchantPin = c.merchantPin
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal refund request: %w", err)
	}

	url := c.baseURL + refundPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to build refund request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Basic "+c.basicAuthToken)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("refund request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read refund response: %w", err)
	}

	slog.Debug("ecocash refund response",
		slog.Int("status_code", resp.StatusCode),
		slog.String("body", string(respBody)),
	)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ecocash refund error (%d): %s", resp.StatusCode, string(respBody))
	}

	var refundResp RefundResponse
	if err := json.Unmarshal(respBody, &refundResp); err != nil {
		return nil, fmt.Errorf("failed to parse refund response: %w", err)
	}
	legacyTxnFill(&refundResp, respBody)

	return &refundResp, nil
}

// legacyTxnFill backfills a transaction struct from the older summary wire
// shape ({"transactionId", "status", "statusMessage", ...}) when the real
// fields are missing. Used by all response parsers and the webhook handler
// so a legacy variant can never read as an empty identifier/status.
func legacyTxnFill(txn *EcoCashTransaction, body []byte) {
	if txn.TxID() != "" && txn.TransactionOperationStatus != "" &&
		txn.CurrencyCode != "" && txn.PaymentAmount.TotalAmountCharged != 0 {
		return
	}
	var legacy struct {
		TransactionID string `json:"transactionId"`
		Status        string `json:"status"`
		StatusMessage string `json:"statusMessage"`
		Currency      string `json:"currency"`
		Amount        string `json:"amount"`
		EndUserMSISN  string `json:"endUserMSISN"`
	}
	if err := json.Unmarshal(body, &legacy); err != nil || (legacy.TransactionID == "" &&
		legacy.Status == "" && legacy.StatusMessage == "" && legacy.Amount == "") {
		return
	}
	if legacy.TransactionID != "" && txn.TxID() == "" {
		txn.TransactionLegacyID = legacy.TransactionID
	}
	if legacy.Status != "" && txn.TransactionOperationStatus == "" {
		txn.TransactionOperationStatus = legacy.Status
	}
	if legacy.StatusMessage != "" && txn.ResponseMessage == "" {
		txn.ResponseMessage = legacy.StatusMessage
	}
	if legacy.Currency != "" && txn.CurrencyCode == "" {
		txn.CurrencyCode = legacy.Currency
	}
	if legacy.EndUserMSISN != "" && txn.EndUserId == "" {
		txn.EndUserId = legacy.EndUserMSISN
	}
	if legacy.Amount != "" && txn.PaymentAmount.TotalAmountCharged == 0 {
		var amt float64
		if _, err := fmt.Sscanf(legacy.Amount, "%f", &amt); err == nil {
			txn.PaymentAmount.TotalAmountCharged = amt
		}
	}
}

// VerifyWebhook validates and parses an EcoCash webhook notification.
//
// The notification payload is not HMAC-signed (no signature scheme in the
// current spec). We parse the transaction wire shape (EcoCashTransaction,
// the same as the lookup endpoint) plus a legacy summary variant. The
// only in-band authenticity signal is the echoed merchant code (pinned
// below); TLS termination in front of the webhook endpoint is the
// additional transport hardening.
func (c *Client) VerifyWebhook(ctx context.Context, headers http.Header, body []byte) (gateways.WebhookEvent, error) {
	// The notification body mirrors the transaction wire shape; only the
	// identifier key differs between variants (id vs transactionId). The
	// legacy summary shape is backfilled by legacyTxnFill.
	var webhook EcoCashTransaction
	if err := json.Unmarshal(body, &webhook); err != nil {
		return gateways.WebhookEvent{}, fmt.Errorf("failed to parse webhook body: %w", err)
	}
	legacyTxnFill(&webhook, body)

	if webhook.TxID() == "" {
		return gateways.WebhookEvent{}, fmt.Errorf("missing transactionId in webhook")
	}

	// Merchant-code pinning: EcoCash webhooks carry no HMAC, so the only
	// in-band authenticity signal is the echoed merchant code. Accept the
	// payload when it is absent (some integrations do not send it — logged
	// by the caller via the webhook audit row) but reject a mismatch so a
	// spoofed notification for a different merchant can never complete one
	// of our transactions.
	if webhook.MerchantCode != "" && webhook.MerchantCode != c.merchantCode {
		return gateways.WebhookEvent{}, fmt.Errorf(
			"webhook merchantCode mismatch: got %q, want %q",
			webhook.MerchantCode, c.merchantCode,
		)
	}

	state := mapEcoCashTxnState(webhook)
	var eventType string
	switch state {
	case "completed":
		eventType = "payment.completed"
	case statePending:
		eventType = "payment.status_update"
	case "failed":
		eventType = "payment.failed"
	case "cancelled":
		eventType = "payment.cancelled"
	case "refunded":
		eventType = "payment.refunded"
	default:
		eventType = "payment.status_update"
	}

	return gateways.WebhookEvent{
		ExternalReference: webhook.TxID(),
		State:             state,
		Amount:            gateways.Money{Amount: int64(webhook.PaymentAmount.TotalAmountCharged * 100), Currency: webhook.CurrencyCode},
		Currency:          webhook.CurrencyCode,
		EventType:         eventType,
		Metadata: map[string]string{
			"clientCorrelator":     webhook.ClientCorrelator,
			"status":               webhook.TransactionOperationStatus,
			"statusMessage":        webhook.Message(),
			"ecocashReference":     webhook.EcoCashReference,
			"endUserId":            webhook.EndUserId,
			"merchantCode":         webhook.MerchantCode,
			"serverReferenceCode":  webhook.ServerReferenceCode,
		},
	}, nil
}
