package paynow

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/freeradius/payments-api/internal/gateways"
)

const (
	sandboxBaseURL    = "https://integrate.paynow.co.zw/app/api/v1/gateway/json"
	productionBaseURL = "https://www.paynow.co.zw/interface/initiatetransaction"
)

// Client is an HTTP client for the Paynow API.
type Client struct {
	integrationID  string
	integrationKey string
	resultURL      string
	returnURL      string
	httpClient     *http.Client
	baseURL        string
}

// NewClient creates a new Paynow API client.
func NewClient(integrationID, integrationKey, resultURL, returnURL string) *Client {
	return &Client{
		integrationID:  integrationID,
		integrationKey: integrationKey,
		resultURL:      resultURL,
		returnURL:      returnURL,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		baseURL:        sandboxBaseURL,
	}
}

// NewProductionClient creates a client configured for Paynow production.
func NewProductionClient(integrationID, integrationKey, resultURL, returnURL string) *Client {
	c := NewClient(integrationID, integrationKey, resultURL, returnURL)
	c.baseURL = productionBaseURL
	return c
}

// SetHTTPClient allows injecting a custom HTTP client (useful for testing).
func (c *Client) SetHTTPClient(client *http.Client) {
	c.httpClient = client
}

// SetBaseURL overrides the API base URL (useful for testing with httptest).
func (c *Client) SetBaseURL(baseURL string) {
	c.baseURL = baseURL
}

// InitiateTransactionRequest contains the parameters for initiating a payment.
type InitiateTransactionRequest struct {
	Reference      string
	Amount         string // decimal string, e.g. "5.00"
	AdditionalInfo string
	AuthEmail      string
	Method         string // express checkout method: ecocash, onemoney, zimdef
	Phone          string // customer phone for mobile money push
}

// InitiateTransactionResponse contains the response from Paynow initiation.
type InitiateTransactionResponse struct {
	Status          string // "Ok" or "Error"
	PollURL         string
	PaynowReference string
	BrowserURL      string
	Hash            string
	Instructions    string
	ErrorMessage    string
}

// InitiateTransaction sends an initiation request to Paynow.
func (c *Client) InitiateTransaction(ctx context.Context, req InitiateTransactionRequest) (*InitiateTransactionResponse, error) {
	// Build form values
	values := url.Values{}
	values.Set("id", c.integrationID)
	values.Set("reference", req.Reference)
	values.Set("amount", req.Amount)
	values.Set("additionalinfo", req.AdditionalInfo)
	values.Set("authemail", req.AuthEmail)
	values.Set("status", "Message")
	values.Set("resulturl", c.resultURL)
	values.Set("returnurl", fmt.Sprintf("%s?ref=%s", c.returnURL, url.QueryEscape(req.Reference)))

	if req.Method != "" {
		values.Set("method", req.Method)
	}
	if req.Phone != "" {
		values.Set("phone", req.Phone)
	}

	// Generate hash over the values
	hash := c.generateHashFromValues(values)
	values.Set("hash", hash)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(body))
	}

	// Parse JSON response
	parsed, err := parseJSONResponse(body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	result := &InitiateTransactionResponse{
		Status:          parsed["status"],
		PollURL:         parsed["pollurl"],
		PaynowReference: parsed["paynowreference"],
		BrowserURL:      parsed["browserurl"],
		Hash:            parsed["hash"],
		Instructions:    parsed["instructions"],
		ErrorMessage:    parsed["error"],
	}

	// Verify response hash if present
	if result.Hash != "" {
		respValues := map[string]string{
			"status":          result.Status,
			"pollurl":         result.PollURL,
			"paynowreference": result.PaynowReference,
			"browserurl":      result.BrowserURL,
			"instructions":    result.Instructions,
		}
		if !c.VerifyHash(respValues, result.Hash) {
			return nil, fmt.Errorf("response hash verification failed")
		}
	}

	return result, nil
}

// StatusResponse contains the result of a status check.
type StatusResponse struct {
	Status          string
	Amount          float64
	Currency        string
	Reference       string
	PaynowReference string
	Hash            string
	FailureReason   string
}

// CheckStatus polls Paynow for the current status of a transaction.
func (c *Client) CheckStatus(ctx context.Context, pollURL string) (*StatusResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("status check failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read status response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Paynow status responses are query-string formatted
	parsed, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("failed to parse status response: %w", err)
	}

	status := parsed.Get("status")
	amountStr := parsed.Get("amount")
	amount, _ := strconv.ParseFloat(amountStr, 64)

	result := &StatusResponse{
		Status:          status,
		Amount:          amount,
		Currency:        parsed.Get("currency"),
		Reference:       parsed.Get("reference"),
		PaynowReference: parsed.Get("paynowreference"),
		Hash:            parsed.Get("hash"),
	}

	// Verify hash if present
	if result.Hash != "" {
		values := map[string]string{
			"status":          result.Status,
			"amount":          amountStr,
			"reference":       result.Reference,
			"paynowreference": result.PaynowReference,
		}
		if !c.VerifyHash(values, result.Hash) {
			return nil, fmt.Errorf("status response hash verification failed")
		}
	}

	return result, nil
}

// VerifyWebhook validates a Paynow webhook payload.
func (c *Client) VerifyWebhook(ctx context.Context, headers http.Header, body []byte) (gateways.WebhookEvent, error) {
	// Parse form-encoded webhook body
	parsed, err := url.ParseQuery(string(body))
	if err != nil {
		return gateways.WebhookEvent{}, fmt.Errorf("failed to parse webhook body: %w", err)
	}

	// Extract hash
	hash := parsed.Get("hash")
	if hash == "" {
		return gateways.WebhookEvent{}, fmt.Errorf("missing hash in webhook payload")
	}

	// Build values map excluding hash
	values := make(map[string]string)
	for key, vals := range parsed {
		if key == "hash" || len(vals) == 0 {
			continue
		}
		values[key] = vals[0]
	}

	// Verify hash
	if !c.VerifyHash(values, hash) {
		return gateways.WebhookEvent{}, fmt.Errorf("webhook hash verification failed")
	}

	// Parse amount
	amountStr := parsed.Get("amount")
	var amountCents int64
	if amountStr != "" {
		if amt, err := strconv.ParseFloat(amountStr, 64); err == nil {
			amountCents = int64(amt * 100)
		}
	}

	// Determine event type from status
	status := strings.ToLower(parsed.Get("status"))
	eventType := "unknown"
	switch status {
	case "paid", "delivered":
		eventType = "payment.completed"
	case "cancelled":
		eventType = "payment.cancelled"
	case "failed", "disputed":
		eventType = "payment.failed"
	case "refunded":
		eventType = "payment.refunded"
	default:
		eventType = "payment.status_update"
	}

	return gateways.WebhookEvent{
		ExternalReference: parsed.Get("paynowreference"),
		State:             mapPaynowStatus(status),
		Amount:            gateways.Money{Amount: amountCents, Currency: parsed.Get("currency")},
		Currency:          parsed.Get("currency"),
		EventType:         eventType,
		Metadata: map[string]string{
			"reference":   parsed.Get("reference"),
			"status":      parsed.Get("status"),
			"paynowreference": parsed.Get("paynowreference"),
		},
	}, nil
}

// generateHashFromValues creates a Paynow hash from url.Values.
func (c *Client) generateHashFromValues(values url.Values) string {
	// Extract all values into a flat map
	flat := make(map[string]string)
	for key, vals := range values {
		if len(vals) > 0 {
			flat[key] = vals[0]
		}
	}
	return c.GenerateHash(flat)
}

// GenerateHash creates a Paynow SHA-512 hash.
// Concatenate all values sorted by key alphabetically, then append integration_key.
func (c *Client) GenerateHash(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(values[k])
	}
	sb.WriteString(c.integrationKey)

	h := sha512.New()
	h.Write([]byte(sb.String()))
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
}

// VerifyHash compares a generated hash against an expected hash.
func (c *Client) VerifyHash(values map[string]string, expectedHash string) bool {
	generated := c.GenerateHash(values)
	return strings.EqualFold(generated, expectedHash)
}

// parseJSONResponse parses a simple JSON response into a flat string map.
// Paynow returns very simple JSON objects; this avoids pulling in a JSON library.
func parseJSONResponse(body []byte) (map[string]string, error) {
	result := make(map[string]string)

	// Very lightweight JSON parser for Paynow's flat response format
	// Expected: {"status":"Ok","pollurl":"...","paynowreference":"...",...}
	text := string(body)
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "{") || !strings.HasSuffix(text, "}") {
		return result, nil // Not a JSON object, return empty
	}

	// Remove outer braces
	text = text[1 : len(text)-1]

	// Simple split by commas (Paynow responses don't contain nested objects or arrays)
	// This is intentionally simple; if Paynow changes their format, upgrade to encoding/json
	parts := splitJSONPairs(text)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		colonIdx := strings.Index(part, `":"`)
		if colonIdx == -1 {
			colonIdx = strings.Index(part, `":`)
		}
		if colonIdx == -1 {
			continue
		}
		key := strings.Trim(part[:colonIdx], `"`)
		val := strings.Trim(part[colonIdx+2:], `"`)
		result[key] = val
	}

	return result, nil
}

// splitJSONPairs splits a flat JSON object string by top-level commas.
func splitJSONPairs(text string) []string {
	var parts []string
	var start int
	inString := false

	for i := 0; i < len(text); i++ {
		ch := text[i]
		if ch == '"' && (i == 0 || text[i-1] != '\\') {
			inString = !inString
		} else if ch == ',' && !inString {
			parts = append(parts, text[start:i])
			start = i + 1
		}
	}
	if start < len(text) {
		parts = append(parts, text[start:])
	}
	return parts
}
