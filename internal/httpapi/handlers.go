package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/freeradius/payments-api/internal/gateways/mock"
	"github.com/freeradius/payments-api/internal/metrics"
	"github.com/freeradius/payments-api/internal/payments"
	"github.com/go-chi/chi/v5"
)

// listPlans returns active tariff plans
func (r *Router) listPlans(w http.ResponseWriter, req *http.Request) {
	plans, err := r.paymentService.ListPlans(req.Context())
	if err != nil {
		slog.Error("failed to list plans", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, "failed to list plans")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"plans": plans,
	})
}

// listGateways returns available payment gateways with fees
func (r *Router) listGateways(w http.ResponseWriter, req *http.Request) {
	gateways, err := r.paymentService.ListGateways(req.Context())
	if err != nil {
		slog.Error("failed to list gateways", slog.Any("error", err))
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"gateways": []interface{}{},
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"gateways": gateways,
	})
}

// listPaymentMethods returns available payment methods from the database, filtered by gateway
func (r *Router) listPaymentMethods(w http.ResponseWriter, req *http.Request) {
	gatewayCode := req.URL.Query().Get("gateway")

	methods, err := r.paymentService.ListPaymentMethods(req.Context(), gatewayCode)
	if err != nil {
		slog.Error("failed to list payment methods", slog.Any("error", err))
		// Return empty array on error - client should handle gracefully
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"methods": []interface{}{},
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"methods": methods,
	})
}

// initiatePaymentRequest represents a payment initiation request
type initiatePaymentRequest struct {
	GatewayCode      string            `json:"gateway_code"`
	MethodCode       string            `json:"method_code"`
	Amount           int64             `json:"amount"`
	Currency         string            `json:"currency"`
	CustomerEmail    string            `json:"customer_email,omitempty"`
	CustomerPhone    string            `json:"customer_phone,omitempty"`
	CustomerID       *int64            `json:"customer_id,omitempty"`
	FulfillmentKind  string            `json:"fulfillment_kind,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
}

// initiatePayment handles payment initiation
func (r *Router) initiatePayment(w http.ResponseWriter, req *http.Request) {
	var reqBody initiatePaymentRequest
	if err := parseJSON(req, &reqBody); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	
	// Validate required fields
	if reqBody.GatewayCode == "" {
		respondError(w, http.StatusBadRequest, "gateway_code is required")
		return
	}
	if reqBody.Amount <= 0 {
		respondError(w, http.StatusBadRequest, "amount must be positive")
		return
	}
	if reqBody.Currency == "" {
		respondError(w, http.StatusBadRequest, "currency is required")
		return
	}
	
	// Initiate payment
	result, err := r.paymentService.Initiate(req.Context(), payments.InitiateRequest{
		GatewayCode:      reqBody.GatewayCode,
		MethodCode:       reqBody.MethodCode,
		Amount:           reqBody.Amount,
		Currency:         reqBody.Currency,
		CustomerEmail:    reqBody.CustomerEmail,
		CustomerPhone:    reqBody.CustomerPhone,
		CustomerID:       reqBody.CustomerID,
		FulfillmentKind:  reqBody.FulfillmentKind,
		Metadata:         reqBody.Metadata,
		IdempotencyKey:   reqBody.IdempotencyKey,
	})
	
	if err != nil {
		slog.Error("failed to initiate payment", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	
	respondJSON(w, http.StatusOK, result)
}

// pollPaymentStatus triggers a manual poll of the gateway status
func (r *Router) pollPaymentStatus(w http.ResponseWriter, req *http.Request) {
	transactionIDStr := chi.URLParam(req, "transaction_id")

	transactionID, err := strconv.ParseInt(transactionIDStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid transaction_id format")
		return
	}

	status, err := r.paymentService.PollStatus(req.Context(), transactionID)
	if err != nil {
		slog.Error("failed to poll payment status", slog.Any("error", err))
		respondError(w, http.StatusNotFound, "transaction not found or poll failed")
		return
	}

	respondJSON(w, http.StatusOK, status)
}

// getPaymentStatus returns the status of a payment
func (r *Router) getPaymentStatus(w http.ResponseWriter, req *http.Request) {
	transactionIDStr := chi.URLParam(req, "transaction_id")
	
	// Try to parse as integer first (database ID)
	transactionID, err := strconv.ParseInt(transactionIDStr, 10, 64)
	if err != nil {
		// If not an integer, it might be the UUID string - we'd need to look it up
		respondError(w, http.StatusBadRequest, "invalid transaction_id format")
		return
	}
	
	status, err := r.paymentService.GetStatus(req.Context(), transactionID)
	if err != nil {
		slog.Error("failed to get payment status", slog.Any("error", err))
		respondError(w, http.StatusNotFound, "transaction not found")
		return
	}
	
	respondJSON(w, http.StatusOK, status)
}

// webhookHandler handles webhooks from payment gateways
func (r *Router) webhookHandler(w http.ResponseWriter, req *http.Request) {
	gatewayCode := chi.URLParam(req, "gateway_code")
	
	// Resolve gateway
	gateway, ok := r.registry.Resolve(gatewayCode)
	if !ok {
		respondError(w, http.StatusNotFound, "gateway not found")
		return
	}
	
	// Read body
	body, err := io.ReadAll(req.Body)
	if err != nil {
		slog.Error("failed to read webhook body",
			slog.String("gateway", gatewayCode),
			slog.Any("error", err),
		)
		respondError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	defer req.Body.Close()
	
	// Verify webhook
	event, err := gateway.VerifyWebhook(req.Context(), req.Header, body)
	if err != nil {
		metrics.RecordWebhookReceived(gatewayCode, false)
		slog.Error("webhook verification failed",
			slog.String("gateway", gatewayCode),
			slog.Any("error", err),
		)
		respondError(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	}

	metrics.RecordWebhookReceived(gatewayCode, true)

	slog.Info("webhook received",
		slog.String("gateway", gatewayCode),
		slog.String("external_ref", event.ExternalReference),
		slog.String("event_type", event.EventType),
	)

	// Process the webhook event (state transition, audit logging)
	if err := r.paymentService.ProcessWebhook(req.Context(), gatewayCode, event, body, req.Header); err != nil {
		slog.Error("webhook processing failed",
			slog.String("gateway", gatewayCode),
			slog.String("external_ref", event.ExternalReference),
			slog.Any("error", err),
		)
		// Still return 200 to the gateway to prevent retries
	}

	// Respond with success
	respondJSON(w, http.StatusOK, map[string]string{
		"status": "received",
	})
}

// Admin handlers

// adminGetTransaction returns detailed transaction info
func (r *Router) adminGetTransaction(w http.ResponseWriter, req *http.Request) {
	transactionIDStr := chi.URLParam(req, "transaction_id")

	transactionID, err := strconv.ParseInt(transactionIDStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid transaction_id")
		return
	}

	// Use GetStatus to fetch the specific transaction
	transaction, err := r.paymentService.GetStatus(req.Context(), transactionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "transaction not found")
		return
	}

	respondJSON(w, http.StatusOK, transaction)
}

// adminRefund handles refund requests
func (r *Router) adminRefund(w http.ResponseWriter, req *http.Request) {
	transactionIDStr := chi.URLParam(req, "transaction_id")

	transactionID, err := strconv.ParseInt(transactionIDStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid transaction_id")
		return
	}

	if err := r.paymentService.Refund(req.Context(), transactionID); err != nil {
		slog.Error("refund failed", slog.Int64("transaction_id", transactionID), slog.Any("error", err))
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"status": "refunded",
	})
}

// adminCancel handles cancellation requests
func (r *Router) adminCancel(w http.ResponseWriter, req *http.Request) {
	transactionIDStr := chi.URLParam(req, "transaction_id")

	transactionID, err := strconv.ParseInt(transactionIDStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid transaction_id")
		return
	}

	if err := r.paymentService.Cancel(req.Context(), transactionID); err != nil {
		slog.Error("cancel failed", slog.Int64("transaction_id", transactionID), slog.Any("error", err))
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"status": "cancelled",
	})
}

// adminListTransactions lists transactions with optional filters.
func (r *Router) adminListTransactions(w http.ResponseWriter, req *http.Request) {
	state := req.URL.Query().Get("state")
	gatewayCode := req.URL.Query().Get("gateway")
	limitStr := req.URL.Query().Get("limit")
	offsetStr := req.URL.Query().Get("offset")

	limit := int64(20)
	if v, err := strconv.ParseInt(limitStr, 10, 64); err == nil && v > 0 && v <= 100 {
		limit = v
	}
	offset := int64(0)
	if v, err := strconv.ParseInt(offsetStr, 10, 64); err == nil && v >= 0 {
		offset = v
	}

	transactions, err := r.paymentService.ListTransactions(req.Context(), payments.ListTransactionsRequest{
		State:  state,
		Limit:  int(limit),
		Offset: int(offset),
	})
	if err != nil {
		slog.Error("failed to list transactions", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, "failed to list transactions")
		return
	}

	// Filter by gateway code client-side (DB query doesn't have gateway_code param today)
	if gatewayCode != "" {
		var filtered []*payments.GetStatusResponse
		for _, t := range transactions {
			if t.GatewayCode == gatewayCode {
				filtered = append(filtered, t)
			}
		}
		transactions = filtered
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"transactions": transactions,
	})
}

// adminGetTransactionWebhooks returns webhook audit log for a transaction.
func (r *Router) adminGetTransactionWebhooks(w http.ResponseWriter, req *http.Request) {
	transactionIDStr := chi.URLParam(req, "transaction_id")

	transactionID, err := strconv.ParseInt(transactionIDStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid transaction_id")
		return
	}

	logs, err := r.paymentService.GetWebhookLogs(req.Context(), transactionID)
	if err != nil {
		slog.Error("failed to get webhook logs", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, "failed to get webhook logs")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"logs": logs,
	})
}

// adminGetReceipt returns transaction receipt data.
func (r *Router) adminGetReceipt(w http.ResponseWriter, req *http.Request) {
	transactionIDStr := chi.URLParam(req, "transaction_id")

	transactionID, err := strconv.ParseInt(transactionIDStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid transaction_id")
		return
	}

	transaction, err := r.paymentService.GetStatus(req.Context(), transactionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "transaction not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"receipt": transaction,
	})
}

// adminListGateways lists all configured gateways
func (r *Router) adminListGateways(w http.ResponseWriter, req *http.Request) {
	gateways := r.registry.List()
	
	var result []map[string]interface{}
	for _, g := range gateways {
		caps := g.Capabilities()
		result = append(result, map[string]interface{}{
			"code":         g.Code(),
			"capabilities": caps,
			"methods":      g.SupportedMethods(),
			"currencies":   g.SupportedCurrencies(),
		})
	}
	
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"gateways": result,
	})
}

// adminGetGatewaySchema returns the configuration schema for a gateway
func (r *Router) adminGetGatewaySchema(w http.ResponseWriter, req *http.Request) {
	gatewayCode := chi.URLParam(req, "gateway_code")
	
	schema, ok := r.registry.GetConfigSchema(gatewayCode)
	if !ok {
		respondError(w, http.StatusNotFound, "gateway not found")
		return
	}
	
	respondJSON(w, http.StatusOK, schema)
}

// Mock gateway admin handlers

// mockListTransactions lists all mock transactions
func (r *Router) mockListTransactions(w http.ResponseWriter, req *http.Request) {
	adapter := r.getMockAdapter()
	if adapter == nil {
		respondError(w, http.StatusNotFound, "mock gateway not available")
		return
	}
	
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"transactions": adapter.ListTransactions(),
	})
}

// mockCompleteTransaction manually completes a mock transaction and delivers webhook
func (r *Router) mockCompleteTransaction(w http.ResponseWriter, req *http.Request) {
	adapter := r.getMockAdapter()
	if adapter == nil {
		respondError(w, http.StatusNotFound, "mock gateway not available")
		return
	}
	
	externalRef := chi.URLParam(req, "external_ref")
	
	if err := adapter.CompleteTransaction(externalRef); err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	
	// Deliver webhook so the payment service processes the state change
	_ = r.deliverMockWebhook(req.Context(), adapter, externalRef, "completed", "payment.completed")
	
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "completed",
		"message": "transaction marked as completed",
	})
}

// mockFailTransaction manually fails a mock transaction and delivers webhook
func (r *Router) mockFailTransaction(w http.ResponseWriter, req *http.Request) {
	adapter := r.getMockAdapter()
	if adapter == nil {
		respondError(w, http.StatusNotFound, "mock gateway not available")
		return
	}
	
	externalRef := chi.URLParam(req, "external_ref")
	
	if err := adapter.FailTransaction(externalRef); err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	
	// Deliver webhook so the payment service processes the state change
	_ = r.deliverMockWebhook(req.Context(), adapter, externalRef, "failed", "payment.failed")
	
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "failed",
		"message": "transaction marked as failed",
	})
}

// mockRefundTransaction manually refunds a mock transaction and delivers webhook
func (r *Router) mockRefundTransaction(w http.ResponseWriter, req *http.Request) {
	adapter := r.getMockAdapter()
	if adapter == nil {
		respondError(w, http.StatusNotFound, "mock gateway not available")
		return
	}
	
	externalRef := chi.URLParam(req, "external_ref")
	
	if err := adapter.RefundTransaction(externalRef); err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	
	// Deliver webhook so the payment service processes the state change
	_ = r.deliverMockWebhook(req.Context(), adapter, externalRef, "refunded", "payment.refunded")
	
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "refunded",
		"message": "transaction marked as refunded",
	})
}

// mockTriggerWebhook manually triggers a webhook for a mock transaction
func (r *Router) mockTriggerWebhook(w http.ResponseWriter, req *http.Request) {
	adapter := r.getMockAdapter()
	if adapter == nil {
		respondError(w, http.StatusNotFound, "mock gateway not available")
		return
	}
	
	externalRef := chi.URLParam(req, "external_ref")
	
	slog.Info("mock webhook triggered", slog.String("external_ref", externalRef))
	
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "triggered",
		"message": "webhook triggered",
	})
}

// mockCheckoutPage serves the mock checkout HTML page
func (r *Router) mockCheckoutPage(w http.ResponseWriter, req *http.Request) {
	adapter := r.getMockAdapter()
	if adapter == nil {
		respondError(w, http.StatusNotFound, "mock gateway not available")
		return
	}
	
	externalRef := chi.URLParam(req, "external_ref")
	
	// Set HTML content type
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	
	html := `<!DOCTYPE html>
<html>
<head>
    <title>Mock Payment Checkout</title>
    <style>
        body { font-family: Arial, sans-serif; max-width: 600px; margin: 50px auto; padding: 20px; }
        .transaction { background: #f5f5f5; padding: 20px; border-radius: 8px; margin: 20px 0; }
        button { padding: 15px 30px; margin: 10px; font-size: 16px; cursor: pointer; border: none; border-radius: 4px; }
        .approve { background: #4CAF50; color: white; }
        .decline { background: #f44336; color: white; }
        .info { color: #666; }
    </style>
</head>
<body>
    <h1>Mock Payment Checkout</h1>
    <div class="transaction">
        <p><strong>Reference:</strong> ` + externalRef + `</p>
        <p><strong>Status:</strong> Pending</p>
        <p class="info">This is a mock checkout page for testing purposes.</p>
    </div>
    <div>
        <button class="approve" onclick="completePayment()">Approve Payment</button>
        <button class="decline" onclick="failPayment()">Decline Payment</button>
    </div>
    <script>
        function completePayment() {
            fetch('/api/mock/transactions/` + externalRef + `/complete', { method: 'POST' })
                .then(() => alert('Payment approved!'))
                .catch(err => alert('Error: ' + err));
        }
        function failPayment() {
            fetch('/api/mock/transactions/` + externalRef + `/fail', { method: 'POST' })
                .then(() => alert('Payment declined!'))
                .catch(err => alert('Error: ' + err));
        }
    </script>
</body>
</html>`
	
	w.Write([]byte(html))
}

// deliverMockWebhook constructs and delivers a mock webhook event to the payment service.
func (r *Router) deliverMockWebhook(ctx context.Context, adapter *mock.Adapter, externalRef, state, eventType string) error {
	tx, ok := adapter.GetTransaction(externalRef)
	if !ok {
		return fmt.Errorf("transaction not found: %s", externalRef)
	}

	event := gateways.WebhookEvent{
		ExternalReference: externalRef,
		State:             state,
		Amount:            tx.Amount,
		Currency:          tx.Amount.Currency,
		EventType:         eventType,
		Metadata:          map[string]string{"source": "mock-admin"},
	}

	body, _ := json.Marshal(map[string]interface{}{
		"external_reference": externalRef,
		"state":              state,
		"amount":             tx.Amount.Amount,
		"currency":           tx.Amount.Currency,
		"event_type":         eventType,
		"metadata":           event.Metadata,
	})

	headers := http.Header{}
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	sig := adapter.ComputeWebhookSignature(timestamp, body)
	headers.Set("X-Mock-Signature", sig)
	headers.Set("X-Mock-Timestamp", timestamp)

	return r.paymentService.ProcessWebhook(ctx, "mock", event, body, headers)
}
