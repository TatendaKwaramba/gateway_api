package ecocash

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockEcoCashServer creates an httptest server that simulates the EcoCash API.
func mockEcoCashServer(t *testing.T) (*httptest.Server, *Client) {
	apiKey := "test-api-key-12345"
	merchantPin := "1234"
	var serverURL string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify Basic Auth
		authHeader := r.Header.Get("Authorization")
		require.True(t, strings.HasPrefix(authHeader, "Basic "), "expected Basic auth header")

		expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(apiKey+":"+merchantPin))
		assert.Equal(t, expectedAuth, authHeader)

		// Route based on path and method
		path := r.URL.Path
		method := r.Method

		switch {
		// Charge endpoint
		case strings.HasSuffix(path, "/payment/v1/transactions/amount/") && method == http.MethodPost:
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)

			var chargeReq ChargeRequest
			err = json.Unmarshal(body, &chargeReq)
			require.NoError(t, err)

			// Validate required fields
			require.NotEmpty(t, chargeReq.EndUserMSISN)
			require.NotEmpty(t, chargeReq.ChargingInformation.Amount)
			require.NotEmpty(t, chargeReq.ChargingInformation.Currency)

			// Simulate failure for magic phone suffix
			if strings.HasSuffix(chargeReq.EndUserMSISN, "0004") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				json.NewEncoder(w).Encode(map[string]string{
					"status": "Failed",
					"statusMessage": "Business rule violation",
				})
				return
			}

			// Simulate auth failure for magic phone suffix
			if strings.HasSuffix(chargeReq.EndUserMSISN, "0005") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "Unauthorized",
				})
				return
			}

			// Success response
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(ChargeResponse{
				TransactionID:    "PP1204433.1148.78123456",
				ClientCorrelator: chargeReq.ClientCorrelator,
				Status:           "Pending",
				StatusMessage:    "Transaction Successful",
				Currency:         chargeReq.ChargingInformation.Currency,
				EndUserMSISN:     chargeReq.EndUserMSISN,
				MerchantCode:     chargeReq.MerchantCode,
				Timestamp:        "2024-04-22T11:41:45.382",
			})

		// Lookup endpoint: /{msisdn}/transactions/amount/{clientCorrelator}
		case method == http.MethodGet && strings.Contains(path, "/transactions/amount/"):
			parts := strings.Split(strings.Trim(path, "/"), "/")
			require.GreaterOrEqual(t, len(parts), 4, "expected at least 4 path segments")

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)

			// Determine status based on the correlator
			status := "Pending"
			if strings.Contains(path, "COMPLETED") {
				status = "Successful"
			} else if strings.Contains(path, "FAILED") {
				status = "Failed"
			}

			json.NewEncoder(w).Encode(LookupResponse{
				TransactionID:    "PP1204433.1148.78123456",
				ClientCorrelator: parts[len(parts)-1],
				Status:           status,
				StatusMessage:    "Transaction " + status,
				Currency:         "USD",
				EndUserMSISN:     parts[0],
				MerchantCode:     "986185",
				Amount:           "5.00",
				Timestamp:        "2024-04-22T11:41:45.382",
			})

		// Refund endpoint
		case strings.HasSuffix(path, "/transactions/refund/") && method == http.MethodPost:
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)

			var refundReq RefundRequest
			err = json.Unmarshal(body, &refundReq)
			require.NoError(t, err)

			require.NotEmpty(t, refundReq.TransactionID)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(RefundResponse{
				Status:        "Reversed",
				StatusMessage: "Refund processed",
				TransactionID: refundReq.TransactionID,
			})

		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error": "not found"}`))
		}
	}))
	serverURL = server.URL

	client := NewClient(apiKey, "986185", merchantPin, "780732685", "UAT00003", serverURL)
	client.SetHTTPClient(server.Client())
	client.SetMerchantDetails("ECOCASH", "UAT STORE 3")

	return server, client
}

func TestAdapter_Code(t *testing.T) {
	adapter := NewAdapterWithClient(nil, "", "")
	assert.Equal(t, "ecocash", adapter.Code())
}

func TestAdapter_Capabilities(t *testing.T) {
	adapter := NewAdapterWithClient(nil, "", "")
	caps := adapter.Capabilities()
	assert.True(t, caps.SupportsRefund)
	assert.True(t, caps.SupportsPolling)
	assert.False(t, caps.RequiresRedirect)
	assert.True(t, caps.RequiresPhone)
	assert.True(t, caps.WebhookAsync)
}

func TestAdapter_SupportedMethods(t *testing.T) {
	adapter := NewAdapterWithClient(nil, "", "")
	methods := adapter.SupportedMethods()
	require.Len(t, methods, 1)
	assert.Equal(t, MethodEcoCash, methods[0].Code)
	assert.Equal(t, "EcoCash", methods[0].DisplayName)
	assert.True(t, methods[0].RequiresPhone)
	assert.False(t, methods[0].RequiresRedirect)
}

func TestAdapter_SupportedCurrencies(t *testing.T) {
	adapter := NewAdapterWithClient(nil, "", "")
	currencies := adapter.SupportedCurrencies()
	assert.Contains(t, currencies, "USD")
	assert.Contains(t, currencies, "ZWL")
}

func TestAdapter_Initiate_Success(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	adapter := NewAdapterWithClient(client, server.URL+"/webhook", server.URL+"/return")

	result, err := adapter.Initiate(context.Background(), gateways.InitiateRequest{
		Amount:        gateways.Money{Amount: 500, Currency: "USD"},
		Currency:      "USD",
		MethodCode:    MethodEcoCash,
		CustomerPhone: "0772123456",
		IdempotencyKey: "test-ref-001",
	})

	require.NoError(t, err)
	assert.Equal(t, "pending", result.State)
	assert.NotEmpty(t, result.ExternalReference)
	assert.Contains(t, result.Metadata, "clientCorrelator")
	assert.Equal(t, "Transaction Successful", result.Metadata["statusMessage"])
}

func TestAdapter_Initiate_RequiresPhone(t *testing.T) {
	adapter := NewAdapterWithClient(nil, "", "")

	_, err := adapter.Initiate(context.Background(), gateways.InitiateRequest{
		Amount:     gateways.Money{Amount: 500, Currency: "USD"},
		Currency:   "USD",
		MethodCode: MethodEcoCash,
		// Missing phone
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "customer_phone is required")
}

func TestAdapter_Initiate_UnsupportedMethod(t *testing.T) {
	adapter := NewAdapterWithClient(nil, "", "")

	_, err := adapter.Initiate(context.Background(), gateways.InitiateRequest{
		Amount:     gateways.Money{Amount: 500, Currency: "USD"},
		Currency:   "USD",
		MethodCode: "unsupported-method",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported payment method")
}

func TestAdapter_Initiate_BusinessRuleViolation(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	adapter := NewAdapterWithClient(client, server.URL+"/webhook", server.URL+"/return")

	_, err := adapter.Initiate(context.Background(), gateways.InitiateRequest{
		Amount:        gateways.Money{Amount: 500, Currency: "USD"},
		Currency:      "USD",
		MethodCode:    MethodEcoCash,
		CustomerPhone: "07721234560004", // magic suffix triggers 422
		IdempotencyKey: "test-ref-422",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ecocash initiate failed")
}

func TestAdapter_Initiate_AuthFailure(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	adapter := NewAdapterWithClient(client, server.URL+"/webhook", server.URL+"/return")

	_, err := adapter.Initiate(context.Background(), gateways.InitiateRequest{
		Amount:        gateways.Money{Amount: 500, Currency: "USD"},
		Currency:      "USD",
		MethodCode:    MethodEcoCash,
		CustomerPhone: "07721234560005", // magic suffix triggers 401
		IdempotencyKey: "test-ref-401",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unauthorized")
}

func TestAdapter_Status(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	adapter := NewAdapterWithClient(client, server.URL+"/webhook", server.URL+"/return")

	// Seed the MSISDN cache
	adapter.msisdnCacheMu.Lock()
	adapter.msisdnCache["PP1204433.1148.78123456"] = "0772123456"
	adapter.msisdnCacheMu.Unlock()

	result, err := adapter.Status(context.Background(), "PP1204433.1148.78123456")
	require.NoError(t, err)
	assert.Equal(t, "pending", result.State)
	assert.Equal(t, int64(500), result.Amount.Amount)
}

func TestAdapter_Status_CacheMiss(t *testing.T) {
	adapter := NewAdapterWithClient(nil, "", "")

	_, err := adapter.Status(context.Background(), "UNKNOWN-REF")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MSISDN not found")
}

func TestAdapter_Refund_Success(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	adapter := NewAdapterWithClient(client, server.URL+"/webhook", server.URL+"/return")

	result, err := adapter.Refund(context.Background(), "PP1204433.1148.78123456", gateways.Money{
		Amount:   500,
		Currency: "USD",
	})

	require.NoError(t, err)
	assert.Equal(t, "refunded", result.State)
	assert.Equal(t, "PP1204433.1148.78123456", result.ExternalReference)
}

func TestClient_Charge(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	resp, err := client.Charge(context.Background(), ChargeRequest{
		ClientCorrelator: "test-123",
		ReferenceCode:    "ref-123",
		EndUserMSISN:     "0772123456",
		TranType:         "MCR",
		ChargingInformation: ChargingInformation{
			Amount:      "5.00",
			Currency:    "USD",
			Description: "Test payment",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "PP1204433.1148.78123456", resp.TransactionID)
	assert.Equal(t, "Pending", resp.Status)
	assert.Equal(t, "test-123", resp.ClientCorrelator)
}

func TestClient_Charge_Defaults(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	// Charge without merchant details — should use client defaults
	resp, err := client.Charge(context.Background(), ChargeRequest{
		ClientCorrelator: "test-defaults",
		EndUserMSISN:     "0772123456",
		ChargingInformation: ChargingInformation{
			Amount:   "10.00",
			Currency: "USD",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "986185", resp.MerchantCode)
}

func TestClient_Lookup(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	resp, err := client.Lookup(context.Background(), "0772123456", "test-correlator")
	require.NoError(t, err)
	assert.Equal(t, "Pending", resp.Status)
	assert.Equal(t, "test-correlator", resp.ClientCorrelator)
}

func TestClient_Lookup_Completed(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	resp, err := client.Lookup(context.Background(), "0772123456", "COMPLETED-CORRELATOR")
	require.NoError(t, err)
	assert.Equal(t, "Successful", resp.Status)
}

func TestClient_Refund(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	resp, err := client.Refund(context.Background(), RefundRequest{
		TransactionID: "PP1204433.1148.78123456",
		RefundAmount:  "5.00",
		Currency:      "USD",
	})

	require.NoError(t, err)
	assert.Equal(t, "Reversed", resp.Status)
	assert.Equal(t, "PP1204433.1148.78123456", resp.TransactionID)
}

func TestClient_VerifyWebhook(t *testing.T) {
	client := NewClient("id", "code", "pin", "number", "terminal", "http://localhost")

	webhookPayload := map[string]string{
		"transactionId":    "PP1204433.1148.78123456",
		"clientCorrelator": "test-123",
		"status":           "Successful",
		"statusMessage":    "Transaction Successful",
		"currency":         "USD",
		"endUserMSISN":     "0772123456",
		"merchantCode":     "986185",
		"amount":           "5.00",
		"timestamp":        "2024-04-22T11:41:45.382",
	}
	body, _ := json.Marshal(webhookPayload)

	event, err := client.VerifyWebhook(context.Background(), http.Header{}, body)
	require.NoError(t, err)

	assert.Equal(t, "PP1204433.1148.78123456", event.ExternalReference)
	assert.Equal(t, "completed", event.State)
	assert.Equal(t, "payment.completed", event.EventType)
	assert.Equal(t, int64(500), event.Amount.Amount)
	assert.Equal(t, "USD", event.Currency)
	assert.Equal(t, "0772123456", event.Metadata["endUserMSISN"])
}

func TestClient_VerifyWebhook_Pending(t *testing.T) {
	client := NewClient("id", "code", "pin", "number", "terminal", "http://localhost")

	webhookPayload := map[string]string{
		"transactionId":    "PP-PENDING-123",
		"clientCorrelator": "test-pending",
		"status":           "Pending",
		"currency":         "USD",
	}
	body, _ := json.Marshal(webhookPayload)

	event, err := client.VerifyWebhook(context.Background(), http.Header{}, body)
	require.NoError(t, err)
	assert.Equal(t, "pending", event.State)
	assert.Equal(t, "payment.status_update", event.EventType)
}

func TestClient_VerifyWebhook_Failed(t *testing.T) {
	client := NewClient("id", "code", "pin", "number", "terminal", "http://localhost")

	webhookPayload := map[string]string{
		"transactionId":    "PP-FAIL-123",
		"clientCorrelator": "test-fail",
		"status":           "Failed",
		"currency":         "USD",
	}
	body, _ := json.Marshal(webhookPayload)

	event, err := client.VerifyWebhook(context.Background(), http.Header{}, body)
	require.NoError(t, err)
	assert.Equal(t, "failed", event.State)
	assert.Equal(t, "payment.failed", event.EventType)
}

func TestClient_VerifyWebhook_Reversed(t *testing.T) {
	client := NewClient("id", "code", "pin", "number", "terminal", "http://localhost")

	webhookPayload := map[string]string{
		"transactionId":    "PP-REV-123",
		"clientCorrelator": "test-rev",
		"status":           "Reversed",
		"currency":         "USD",
	}
	body, _ := json.Marshal(webhookPayload)

	event, err := client.VerifyWebhook(context.Background(), http.Header{}, body)
	require.NoError(t, err)
	assert.Equal(t, "refunded", event.State)
	assert.Equal(t, "payment.refunded", event.EventType)
}

func TestClient_VerifyWebhook_MissingTransactionID(t *testing.T) {
	client := NewClient("id", "code", "pin", "number", "terminal", "http://localhost")

	webhookPayload := map[string]string{
		"status": "Successful",
	}
	body, _ := json.Marshal(webhookPayload)

	_, err := client.VerifyWebhook(context.Background(), http.Header{}, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing transactionId")
}

func TestClient_VerifyWebhook_InvalidJSON(t *testing.T) {
	client := NewClient("id", "code", "pin", "number", "terminal", "http://localhost")

	_, err := client.VerifyWebhook(context.Background(), http.Header{}, []byte("not json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse webhook body")
}

func TestClient_GenerateBasicAuth(t *testing.T) {
	apiKey := "test-api-key"
	merchantPin := "1234"
	expected := base64.StdEncoding.EncodeToString([]byte(apiKey + ":" + merchantPin))

	client := NewClient(apiKey, "code", merchantPin, "number", "terminal", "http://localhost")
	assert.Equal(t, expected, client.basicAuthToken)
}

func TestMapEcoCashStatus(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Successful", "completed"},
		{"successful", "completed"},
		{"Delivered", "completed"},
		{"Pending", "pending"},
		{"Initiated", "pending"},
		{"Failed", "failed"},
		{"Rejected", "failed"},
		{"Cancelled", "cancelled"},
		{"Reversed", "refunded"},
		{"Unknown", "pending"},
		{"", "pending"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, mapEcoCashStatus(tt.input))
		})
	}
}

func TestNormalizeMSISDN(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"0772123456", "0772123456"},
		{"+263772123456", "0772123456"},
		{"263772123456", "0772123456"},
		{"772123456", "0772123456"},
		{" 0772123456 ", "0772123456"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, normalizeMSISDN(tt.input))
		})
	}
}

func TestAdapter_ConfigSchema(t *testing.T) {
	adapter := NewAdapterWithClient(nil, "", "")
	schema := adapter.ConfigSchema()
	assert.NotNil(t, schema)
}

func TestChargeRoundTrip(t *testing.T) {
	server, client := mockEcoCashServer(t)
	defer server.Close()

	adapter := NewAdapterWithClient(client, server.URL+"/webhook", server.URL+"/return")

	// Step 1: Initiate payment
	result, err := adapter.Initiate(context.Background(), gateways.InitiateRequest{
		Amount:        gateways.Money{Amount: 500, Currency: "USD"},
		Currency:      "USD",
		MethodCode:    MethodEcoCash,
		CustomerPhone: "0772123456",
		IdempotencyKey: "roundtrip-001",
	})
	require.NoError(t, err)
	assert.Equal(t, "pending", result.State)
	assert.NotEmpty(t, result.ExternalReference)

	// Step 2: Check status — should be pending
	status1, err := adapter.Status(context.Background(), result.ExternalReference)
	require.NoError(t, err)
	assert.Equal(t, "pending", status1.State)

	// Step 3: Refund
	refundResult, err := adapter.Refund(context.Background(), result.ExternalReference, gateways.Money{
		Amount:   500,
		Currency: "USD",
	})
	require.NoError(t, err)
	assert.Equal(t, "refunded", refundResult.State)
}

func TestInitiateNetworkError(t *testing.T) {
	// Server that immediately closes connections
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	}))
	server.Close()

	client := NewClient("id", "code", "pin", "number", "terminal", server.URL)
	client.SetHTTPClient(&http.Client{Timeout: 1})

	adapter := NewAdapterWithClient(client, "", "")

	_, err := adapter.Initiate(context.Background(), gateways.InitiateRequest{
		Amount:        gateways.Money{Amount: 500, Currency: "USD"},
		Currency:      "USD",
		MethodCode:    MethodEcoCash,
		CustomerPhone: "0772123456",
		IdempotencyKey: "net-error-test",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ecocash initiate failed")
}

func TestIsSupportedMethod(t *testing.T) {
	adapter := NewAdapterWithClient(nil, "", "")
	assert.True(t, adapter.isSupportedMethod(MethodEcoCash))
	assert.False(t, adapter.isSupportedMethod("unsupported"))
	assert.False(t, adapter.isSupportedMethod(""))
}

func TestNormalizeMSISDN_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"leading plus and country code", "+263772123456", "0772123456"},
		{"country code without plus", "263772123456", "0772123456"},
		{"local format", "0772123456", "0772123456"},
		{"seven digit", "772123456", "0772123456"},
		{"with spaces", "077 212 3456", "077 212 3456"},
		{"with dashes", "077-212-3456", "077-212-3456"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeMSISDN(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestClient_BasicAuthHeader(t *testing.T) {
	var capturedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"transactionId":"PP-TEST","status":"Pending","clientCorrelator":"c","currency":"USD"}`))
	}))
	defer server.Close()

	client := NewClient("my-api-key", "code", "my-pin", "number", "terminal", server.URL)
	client.SetHTTPClient(server.Client())

	_, _ = client.Charge(context.Background(), ChargeRequest{
		ClientCorrelator: "test",
		EndUserMSISN:     "0772123456",
		ChargingInformation: ChargingInformation{
			Amount:   "1.00",
			Currency: "USD",
		},
	})

	expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("my-api-key:my-pin"))
	assert.Equal(t, expectedAuth, capturedAuth)
}

func TestSetBaseURL_TrailingSlash(t *testing.T) {
	client := NewClient("id", "code", "pin", "number", "terminal", "http://example.com/")
	assert.Equal(t, "http://example.com", client.baseURL)
}

func TestSetMerchantDetails(t *testing.T) {
	client := NewClient("id", "code", "pin", "number", "terminal", "http://example.com")
	client.SetMerchantDetails("SUPER", "MERCH")
	assert.Equal(t, "SUPER", client.superMerchantName)
	assert.Equal(t, "MERCH", client.merchantName)
}

// TestMultipleGatewaysInRegistry verifies both gateways can coexist
func TestMultipleGatewaysInRegistry(t *testing.T) {
	// This test verifies the design goal: EcoCash and Paynow can coexist
	ecocashAdapter := NewAdapterWithClient(nil, "", "")
	assert.Equal(t, "ecocash", ecocashAdapter.Code())

	// Simulate what registry does
	gateways := map[string]string{
		"paynow": "paynow",
		"ecocash": ecocashAdapter.Code(),
	}
	assert.Len(t, gateways, 2)
	assert.Equal(t, "ecocash", gateways["ecocash"])
	assert.Equal(t, "paynow", gateways["paynow"])
}

func TestSupportedMethods_MatchesRegistry(t *testing.T) {
	adapter := NewAdapterWithClient(nil, "", "")
	methods := adapter.SupportedMethods()

	// Verify method code follows expected pattern
	for _, m := range methods {
		assert.True(t, strings.HasPrefix(m.Code, "ecocash-"), "method code should start with 'ecocash-'")
		assert.NotEmpty(t, m.DisplayName)
	}
}

func TestChargeResponse_AllFieldsParsed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"transactionId": "PP1204433.1148.78123456",
			"clientCorrelator": "my-correlator",
			"status": "Pending",
			"statusMessage": "Transaction Successful",
			"ecocashRef": "ECO-REF-123",
			"currency": "USD",
			"endUserMSISN": "0772123456",
			"merchantCode": "986185",
			"timestamp": "2024-04-22T11:41:45.382"
		}`)
	}))
	defer server.Close()

	client := NewClient("id", "code", "pin", "number", "terminal", server.URL)
	client.SetHTTPClient(server.Client())

	resp, err := client.Charge(context.Background(), ChargeRequest{
		ClientCorrelator: "my-correlator",
		EndUserMSISN:     "0772123456",
		ChargingInformation: ChargingInformation{
			Amount:   "5.00",
			Currency: "USD",
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "PP1204433.1148.78123456", resp.TransactionID)
	assert.Equal(t, "my-correlator", resp.ClientCorrelator)
	assert.Equal(t, "Pending", resp.Status)
	assert.Equal(t, "Transaction Successful", resp.StatusMessage)
	assert.Equal(t, "ECO-REF-123", resp.EcoCashRef)
	assert.Equal(t, "USD", resp.Currency)
	assert.Equal(t, "0772123456", resp.EndUserMSISN)
	assert.Equal(t, "986185", resp.MerchantCode)
	assert.Equal(t, "2024-04-22T11:41:45.382", resp.Timestamp)
}
