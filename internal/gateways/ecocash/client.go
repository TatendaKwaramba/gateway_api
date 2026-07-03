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

const (
	sandboxBaseURL    = "https://developers.ecocash.co.zw/sandbox"
	productionBaseURL = "https://developers.ecocash.co.zw"
	chargePath        = "/payment/v1/transactions/amount/"
	refundPath        = "/transactions/refund/"
)

// Client is an HTTP client for the EcoCash API.
type Client struct {
	apiKey           string
	merchantCode     string
	merchantPin      string
	merchantNumber   string
	terminalID       string
	superMerchantName string
	merchantName     string
	httpClient       *http.Client
	baseURL          string
	basicAuthToken   string // base64(apiKey + ":" + merchantPin), computed once
}

// NewClient creates a new EcoCash API client.
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

// ChargingInformation contains the payment amount and currency.
type ChargingInformation struct {
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
}

// ChargeMetaData contains additional metadata for the charge.
type ChargeMetaData struct {
	Channel string `json:"channel"`
}

// ChargeRequest contains the parameters for initiating a C2B charge.
type ChargeRequest struct {
	ClientCorrelator      string           `json:"clientCorrelator"`
	ReferenceCode         string           `json:"referenceCode"`
	EndUserMSISN          string           `json:"endUserMSISN"`
	TranType              string           `json:"tranType"`
	Remarks               string           `json:"remarks"`
	ChargingInformation   ChargingInformation `json:"chargingInformation"`
	ChargeMetaData        ChargeMetaData   `json:"chargeMetaData"`
	MerchantCode          string           `json:"merchantCode"`
	MerchantPin           string           `json:"merchantPin"`
	MerchantNumber        string           `json:"merchantNumber"`
	CountryCode           string           `json:"countryCode"`
	TerminalID            string           `json:"terminalID"`
	NotifyURL             string           `json:"notifyUrl"`
	SuperMerchantName     string           `json:"superMerchantName"`
	MerchantName          string           `json:"merchantName"`
	TransactionOperationStatus string      `json:"transactionOperationStatus"`
}

// ChargeResponse contains the response from an EcoCash charge request.
type ChargeResponse struct {
	TransactionID     string `json:"transactionId"`
	ClientCorrelator  string `json:"clientCorrelator"`
	Status            string `json:"status"`
	StatusMessage     string `json:"statusMessage"`
	EcoCashRef        string `json:"ecocashRef"`
	Currency          string `json:"currency"`
	EndUserMSISN      string `json:"endUserMSISN"`
	MerchantCode      string `json:"merchantCode"`
	Timestamp         string `json:"timestamp"`
}

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
type RefundResponse struct {
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage"`
	TransactionID string `json:"transactionId"`
}

// LookupResponse contains the result of a transaction status lookup.
type LookupResponse struct {
	TransactionID     string `json:"transactionId"`
	ClientCorrelator  string `json:"clientCorrelator"`
	Status            string `json:"status"`
	StatusMessage     string `json:"statusMessage"`
	Currency          string `json:"currency"`
	EndUserMSISN      string `json:"endUserMSISN"`
	MerchantCode      string `json:"merchantCode"`
	Amount            string `json:"amount"`
	Timestamp         string `json:"timestamp"`
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
	if req.SuperMerchantName == "" {
		req.SuperMerchantName = c.superMerchantName
	}
	if req.MerchantName == "" {
		req.MerchantName = c.merchantName
	}
	if req.TranType == "" {
		req.TranType = "MCR"
	}
	if req.ChargeMetaData.Channel == "" {
		req.ChargeMetaData.Channel = "Online"
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

	return &refundResp, nil
}

// VerifyWebhook validates and parses an EcoCash webhook notification.
func (c *Client) VerifyWebhook(ctx context.Context, headers http.Header, body []byte) (gateways.WebhookEvent, error) {
	// Parse the webhook JSON body
	var webhook struct {
		TransactionID    string `json:"transactionId"`
		ClientCorrelator string `json:"clientCorrelator"`
		Status           string `json:"status"`
		StatusMessage    string `json:"statusMessage"`
		Currency         string `json:"currency"`
		EndUserMSISN     string `json:"endUserMSISN"`
		MerchantCode     string `json:"merchantCode"`
		Amount           string `json:"amount"`
		Timestamp        string `json:"timestamp"`
	}

	if err := json.Unmarshal(body, &webhook); err != nil {
		return gateways.WebhookEvent{}, fmt.Errorf("failed to parse webhook body: %w", err)
	}

	if webhook.TransactionID == "" {
		return gateways.WebhookEvent{}, fmt.Errorf("missing transactionId in webhook")
	}

	// Parse amount
	var amountCents int64
	if webhook.Amount != "" {
		// Amount may be decimal string like "5.00" or "5"
		var amountFloat float64
		if _, err := fmt.Sscanf(webhook.Amount, "%f", &amountFloat); err == nil {
			amountCents = int64(amountFloat * 100)
		}
	}

	// Determine event type from status
	status := strings.ToLower(webhook.Status)
	eventType := "unknown"
	switch status {
	case "successful", "delivered":
		eventType = "payment.completed"
	case "pending", "initiated":
		eventType = "payment.status_update"
	case "failed", "rejected":
		eventType = "payment.failed"
	case "cancelled":
		eventType = "payment.cancelled"
	case "reversed":
		eventType = "payment.refunded"
	default:
		eventType = "payment.status_update"
	}

	return gateways.WebhookEvent{
		ExternalReference: webhook.TransactionID,
		State:             mapEcoCashStatus(webhook.Status),
		Amount:            gateways.Money{Amount: amountCents, Currency: webhook.Currency},
		Currency:          webhook.Currency,
		EventType:         eventType,
		Metadata: map[string]string{
			"clientCorrelator": webhook.ClientCorrelator,
			"status":           webhook.Status,
			"statusMessage":    webhook.StatusMessage,
			"endUserMSISN":     webhook.EndUserMSISN,
			"merchantCode":     webhook.MerchantCode,
			"timestamp":        webhook.Timestamp,
		},
	}, nil
}

// mapEcoCashStatus maps EcoCash status strings to our state machine states.
func mapEcoCashStatus(status string) string {
	switch strings.ToLower(status) {
	case "successful", "delivered":
		return "completed"
	case "pending", "initiated":
		return "pending"
	case "failed", "rejected":
		return "failed"
	case "cancelled":
		return "cancelled"
	case "reversed":
		return "refunded"
	default:
		return "pending"
	}
}
