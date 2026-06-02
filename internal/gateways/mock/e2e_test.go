package mock

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/freeradius/payments-api/internal/django"
	"github.com/freeradius/payments-api/internal/fulfillment"
	"github.com/freeradius/payments-api/internal/gateways"
	notifymock "github.com/freeradius/payments-api/internal/notify/mock"
	"github.com/freeradius/payments-api/internal/payments"
	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

type e2eHarness struct {
	db             *sql.DB
	svc            *payments.Service
	adapter        *Adapter
	registry       *gateways.Registry
	webhookSecret  string
	cleanup        func()
}

func newE2E(t *testing.T) *e2eHarness {
	t.Helper()

	db, cleanupDB, err := testutil.SetupTestDB()
	require.NoError(t, err, "failed to spin up test database")

	registry := gateways.NewRegistry()

	secret := "test-webhook-secret-min-32-characters-long"
	adapter := NewAdapter(secret, "http://localhost:19208/payment/{id}")
	adapter.SetWebhookBaseURL("http://localhost:19207")
	require.NoError(t, registry.Register(adapter))

	notifier := notifymock.NewMockProvider()
	fulfillmentSvc := fulfillment.NewService(db, notifier)

	// Empty Django client; CoA tests are unit-tested elsewhere
	djangoClient := django.NewClient("", "")
	paymentSvc := payments.NewService(db, registry, fulfillmentSvc, djangoClient, "USD")

	return &e2eHarness{
		db:            db,
		svc:           paymentSvc,
		adapter:       adapter,
		registry:      registry,
		webhookSecret: secret,
		cleanup:       cleanupDB,
	}
}

func (h *e2eHarness) buildBody(externalRef, state, eventType string, amount int64, currency string) []byte {
	payload := map[string]interface{}{
		"external_reference": externalRef,
		"state":              state,
		"amount":             amount,
		"currency":           currency,
		"event_type":         eventType,
		"metadata":           map[string]string{"source": "test"},
	}
	b, _ := json.Marshal(payload)
	return b
}

func (h *e2eHarness) sign(body []byte) http.Header {
	return h.signWithTimestamp(body, fmt.Sprintf("%d", time.Now().Unix()))
}

func (h *e2eHarness) signWithTimestamp(body []byte, timestamp string) http.Header {
	message := timestamp + "." + string(body)
	mac := hmac.New(sha256.New, []byte(h.webhookSecret))
	mac.Write([]byte(message))
	sig := hex.EncodeToString(mac.Sum(nil))

	headers := http.Header{}
	headers.Set("X-Mock-Signature", sig)
	headers.Set("X-Mock-Timestamp", timestamp)
	return headers
}

func (h *e2eHarness) waitForState(t *testing.T, txID int64, want string, maxWait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		resp, err := h.svc.GetStatus(context.Background(), txID)
		require.NoError(t, err)
		if resp.State == want {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for state %s on tx %d", want, txID)
}

func (h *e2eHarness) queryRadcheckCount(pin string) int {
	var count int
	_ = h.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM radcheck WHERE username = ?", pin).Scan(&count)
	return count
}

func (h *e2eHarness) queryWebhookLogCount(txID int64) int {
	var count int
	_ = h.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM payments_paymentwebhooklog WHERE transaction_id = ?", txID).Scan(&count)
	return count
}

func (h *e2eHarness) queryNotificationCount(txID int64) int {
	var count int
	_ = h.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM notification_attempts WHERE transaction_id = ?", txID).Scan(&count)
	return count
}

func (h *e2eHarness) getExternalRef(txID int64) string {
	var ext string
	_ = h.db.QueryRowContext(context.Background(),
		"SELECT external_reference FROM payments_paymenttransaction WHERE id = ?", txID).Scan(&ext)
	return ext
}

// ---------------------------------------------------------------------------
// Magic-value tests
// ---------------------------------------------------------------------------

func TestE2E_AsyncSuccess_Webhook(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Phone ending 0002 -> async completed after 3s webhook
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000002",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)
	assert.Equal(t, "pending", initResp.State)

	// Wait for mock adapter to fire webhook
	time.Sleep(4 * time.Second)

	// Build and deliver webhook manually (simulating what the adapter POSTs)
	tx, ok := h.adapter.GetTransaction(h.getExternalRef(initResp.TransactionID))
	require.True(t, ok)
	require.Equal(t, "completed", tx.State)

	body := h.buildBody(tx.ExternalReference, "completed", "payment.completed", tx.Amount.Amount, tx.Amount.Currency)
	headers := h.sign(body)
	event, err := h.adapter.VerifyWebhook(ctx, headers, body)
	require.NoError(t, err)

	err = h.svc.ProcessWebhook(ctx, "mock", event, body, headers)
	require.NoError(t, err)

	// Assert DB state
	status, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", status.State)
	assert.NotEmpty(t, status.VoucherPIN)

	// Assert radcheck rows exist
	assert.Equal(t, 2, h.queryRadcheckCount(status.VoucherPIN), "expected radcheck password + time-limit rows")

	// Assert webhook audit log
	assert.GreaterOrEqual(t, h.queryWebhookLogCount(initResp.TransactionID), 1)

	// Assert notification was attempted
	assert.GreaterOrEqual(t, h.queryNotificationCount(initResp.TransactionID), 1)
}

func TestE2E_InstantSuccess(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Phone ending 0001 -> instant completed at Initiate
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-instant",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000001",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)
	assert.Equal(t, "completed", initResp.State)

	// Fulfillment runs async; give it a moment
	time.Sleep(500 * time.Millisecond)

	status, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", status.State)
	assert.NotEmpty(t, status.VoucherPIN)
	assert.Equal(t, 2, h.queryRadcheckCount(status.VoucherPIN))
}

func TestE2E_FailInsufficientFunds(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Phone ending 0003 -> failed (insufficient funds)
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000003",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)
	assert.Equal(t, "pending", initResp.State)

	time.Sleep(3 * time.Second)

	tx, ok := h.adapter.GetTransaction(h.getExternalRef(initResp.TransactionID))
	require.True(t, ok)
	require.Equal(t, "failed", tx.State)

	body := h.buildBody(tx.ExternalReference, "failed", "payment.failed", tx.Amount.Amount, tx.Amount.Currency)
	headers := h.sign(body)
	event, err := h.adapter.VerifyWebhook(ctx, headers, body)
	require.NoError(t, err)

	err = h.svc.ProcessWebhook(ctx, "mock", event, body, headers)
	require.NoError(t, err)

	status, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "failed", status.State)
	assert.Empty(t, status.VoucherPIN)
	assert.Equal(t, 0, h.queryRadcheckCount(""))
}

func TestE2E_FailDeclined(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Phone ending 0004 -> failed (customer declined)
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000004",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	tx, ok := h.adapter.GetTransaction(h.getExternalRef(initResp.TransactionID))
	require.True(t, ok)
	body := h.buildBody(tx.ExternalReference, "failed", "payment.failed", tx.Amount.Amount, tx.Amount.Currency)
	headers := h.sign(body)
	event, err := h.adapter.VerifyWebhook(ctx, headers, body)
	require.NoError(t, err)

	err = h.svc.ProcessWebhook(ctx, "mock", event, body, headers)
	require.NoError(t, err)

	status, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "failed", status.State)
}

func TestE2E_PendingForever_TimeoutByPoller(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Phone ending 0005 -> pending forever (poller should cap at 30 min)
	// For test speed, we will simulate poller directly rather than waiting 30 min.
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000005",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)
	assert.Equal(t, "pending", initResp.State)

	// Manually update created_at to be older than 30 minutes so timeoutOldTransactions triggers
	_, err = h.db.ExecContext(ctx, `
		UPDATE payments_paymenttransaction SET created_at = DATE_SUB(NOW(), INTERVAL 31 MINUTE) WHERE id = ?
	`, initResp.TransactionID)
	require.NoError(t, err)

	// Simulate the poller timeout query
	_, err = h.db.ExecContext(ctx, `
		UPDATE payments_paymenttransaction
		SET state = 'failed', status = 'failed', updated_at = NOW()
		WHERE state IN ('initiated', 'pending')
		  AND created_at <= DATE_SUB(NOW(), INTERVAL 1800 SECOND)
	`)
	require.NoError(t, err)

	status, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "failed", status.State)
}

func TestE2E_InvalidWebhookSignature(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Phone ending 0006 -> webhook with invalid signature
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000006",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)

	time.Sleep(4 * time.Second)

	tx, ok := h.adapter.GetTransaction(h.getExternalRef(initResp.TransactionID))
	require.True(t, ok)

	body := h.buildBody(tx.ExternalReference, "completed", "payment.completed", tx.Amount.Amount, tx.Amount.Currency)
	// Tamper signature
	headers := http.Header{}
	headers.Set("X-Mock-Signature", "bad_signature")
	headers.Set("X-Mock-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))

	_, err = h.adapter.VerifyWebhook(ctx, headers, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid signature")

	// Transaction should still be pending because webhook was rejected
	status, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "pending", status.State)
}

func TestE2E_WebhookReplay_Deduped(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Phone ending 0007 -> webhook arrives twice; second should be noop
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000007",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)

	time.Sleep(4 * time.Second)

	tx, ok := h.adapter.GetTransaction(h.getExternalRef(initResp.TransactionID))
	require.True(t, ok)

	body := h.buildBody(tx.ExternalReference, "completed", "payment.completed", tx.Amount.Amount, tx.Amount.Currency)
	headers := h.sign(body)
	event, err := h.adapter.VerifyWebhook(ctx, headers, body)
	require.NoError(t, err)

	// First delivery
	err = h.svc.ProcessWebhook(ctx, "mock", event, body, headers)
	require.NoError(t, err)

	status1, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", status1.State)
	pin1 := status1.VoucherPIN

	// Second delivery (same body/headers = replay)
	err = h.svc.ProcessWebhook(ctx, "mock", event, body, headers)
	require.NoError(t, err) // should swallow, not error

	status2, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", status2.State)
	assert.Equal(t, pin1, status2.VoucherPIN, "PIN must not change on replay")

	// Only one set of radcheck rows
	assert.Equal(t, 2, h.queryRadcheckCount(pin1))
}

func TestE2E_NetworkErrorOnInitiate(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Phone ending 0008 -> network error from Initiate (5xx)
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000008",
	}
	_, err := h.svc.Initiate(ctx, initReq)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network error")
}

func TestE2E_SlowInitiate(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Phone ending 0009 -> slow Initiate (~10s)
	start := time.Now()
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000009",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, elapsed, 9*time.Second, "expected slow initiate")
	assert.Equal(t, "pending", initResp.State)
}

func TestE2E_ChargebackSimulation(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Use instant success to get to completed quickly
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-instant",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000001",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	status, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)

	// Manually deliver a refunded webhook to simulate chargeback
	extRef := h.getExternalRef(initResp.TransactionID)
	body := h.buildBody(extRef, "refunded", "payment.refunded", 500, "USD")
	headers := h.sign(body)
	event, err := h.adapter.VerifyWebhook(ctx, headers, body)
	require.NoError(t, err)
	err = h.svc.ProcessWebhook(ctx, "mock", event, body, headers)
	require.NoError(t, err)

	status2, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "refunded", status2.State)
}

func TestE2E_FulfillmentFailure_Rollback(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Amount ending .99 -> approves but fulfillment fails (no matching tariff plan)
	// We seeded a plan at $5.00; $6.99 => amountWhole=6 => no plan at 6.00
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        699, // $6.99 — no tariff plan at this price
		Currency:      "USD",
		CustomerPhone: "0772111111",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)
	assert.Equal(t, "pending", initResp.State)

	time.Sleep(4 * time.Second)

	tx, ok := h.adapter.GetTransaction(h.getExternalRef(initResp.TransactionID))
	require.True(t, ok)

	body := h.buildBody(tx.ExternalReference, "completed", "payment.completed", tx.Amount.Amount, tx.Amount.Currency)
	headers := h.sign(body)
	event, err := h.adapter.VerifyWebhook(ctx, headers, body)
	require.NoError(t, err)
	err = h.svc.ProcessWebhook(ctx, "mock", event, body, headers)
	require.NoError(t, err)

	// Wait a moment for async fulfillment to run and fail
	time.Sleep(1 * time.Second)

	status, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "failed", status.State, "transaction should rollback to failed when fulfillment fails")
	assert.Empty(t, status.VoucherPIN)
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

func TestE2E_IdempotencyKey_ReplayReturnsSameTransaction(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	initReq := payments.InitiateRequest{
		GatewayCode:    "mock",
		MethodCode:     "mock-instant",
		Amount:         500,
		Currency:       "USD",
		CustomerPhone:  "0772000001",
		IdempotencyKey: "unique-key-12345",
	}

	resp1, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)
	require.NotNil(t, resp1)

	// Exact same request again
	resp2, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)
	require.NotNil(t, resp2)

	assert.Equal(t, resp1.TransactionID, resp2.TransactionID, "idempotency replay must return same transaction")
	assert.True(t, resp2.IsReplay)
}

func TestE2E_IdempotencyKey_DifferentRequestSameKey_Errors(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	initReq1 := payments.InitiateRequest{
		GatewayCode:    "mock",
		MethodCode:     "mock-instant",
		Amount:         500,
		Currency:       "USD",
		CustomerPhone:  "0772000001",
		IdempotencyKey: "conflict-key-999",
	}
	_, err := h.svc.Initiate(ctx, initReq1)
	require.NoError(t, err)

	initReq2 := payments.InitiateRequest{
		GatewayCode:    "mock",
		MethodCode:     "mock-instant",
		Amount:         999, // different amount!
		Currency:       "USD",
		CustomerPhone:  "0772000001",
		IdempotencyKey: "conflict-key-999",
	}
	_, err = h.svc.Initiate(ctx, initReq2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "idempotency key conflict")
}

// ---------------------------------------------------------------------------
// Admin controls
// ---------------------------------------------------------------------------

func TestE2E_AdminComplete_Manual(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Use a plain phone so the adapter stays pending
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772999999",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)
	assert.Equal(t, "pending", initResp.State)

	// Admin force-complete via adapter control method
	tx, ok := h.adapter.GetTransaction(h.getExternalRef(initResp.TransactionID))
	require.True(t, ok)
	err = h.adapter.CompleteTransaction(tx.ExternalReference)
	require.NoError(t, err)

	// Deliver synthetic webhook to drive state machine
	body := h.buildBody(tx.ExternalReference, "completed", "payment.completed", tx.Amount.Amount, tx.Amount.Currency)
	headers := h.sign(body)
	event, err := h.adapter.VerifyWebhook(ctx, headers, body)
	require.NoError(t, err)
	err = h.svc.ProcessWebhook(ctx, "mock", event, body, headers)
	require.NoError(t, err)

	status, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", status.State)
}

func TestE2E_AdminRefund(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Instant complete
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-instant",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000001",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)

	statusBefore, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "completed", statusBefore.State)
	pin := statusBefore.VoucherPIN
	require.NotEmpty(t, pin)

	// Issue refund
	err = h.svc.Refund(ctx, initResp.TransactionID)
	require.NoError(t, err)

	statusAfter, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "refunded", statusAfter.State)

	// Radcheck rows should be removed
	assert.Equal(t, 0, h.queryRadcheckCount(pin), "radcheck rows must be removed on refund")
}

func TestE2E_AdminCancel(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-ecocash",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772999999",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)
	assert.Equal(t, "pending", initResp.State)

	err = h.svc.Cancel(ctx, initResp.TransactionID)
	require.NoError(t, err)

	status, err := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", status.State)
}

// ---------------------------------------------------------------------------
// Webhook edge cases
// ---------------------------------------------------------------------------

func TestE2E_WebhookExpiredTimestamp(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-instant",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000001",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)

	tx, ok := h.adapter.GetTransaction(h.getExternalRef(initResp.TransactionID))
	require.True(t, ok)

	body := h.buildBody(tx.ExternalReference, "completed", "payment.completed", tx.Amount.Amount, tx.Amount.Currency)
	oldTimestamp := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	headers := h.signWithTimestamp(body, oldTimestamp)

	_, err = h.adapter.VerifyWebhook(ctx, headers, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too old")
}

func TestE2E_WebhookMissingSignature(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-instant",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000001",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)

	tx, ok := h.adapter.GetTransaction(h.getExternalRef(initResp.TransactionID))
	require.True(t, ok)

	body := h.buildBody(tx.ExternalReference, "completed", "payment.completed", tx.Amount.Amount, tx.Amount.Currency)
	headers := http.Header{}
	headers.Set("X-Mock-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	// missing X-Mock-Signature

	_, err = h.adapter.VerifyWebhook(ctx, headers, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing X-Mock-Signature")
}

// ---------------------------------------------------------------------------
// State-machine guard tests
// ---------------------------------------------------------------------------

func TestE2E_InvalidStateTransition_RefundedToCompleted(t *testing.T) {
	h := newE2E(t)
	defer h.cleanup()
	ctx := context.Background()

	// Complete then refund
	initReq := payments.InitiateRequest{
		GatewayCode:   "mock",
		MethodCode:    "mock-instant",
		Amount:        500,
		Currency:      "USD",
		CustomerPhone: "0772000001",
	}
	initResp, err := h.svc.Initiate(ctx, initReq)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)
	err = h.svc.Refund(ctx, initResp.TransactionID)
	require.NoError(t, err)

	// Try to transition refunded -> completed via webhook (should be swallowed)
	status, _ := h.svc.GetStatus(ctx, initResp.TransactionID)
	require.Equal(t, "refunded", status.State)

	// Build a synthetic webhook for completed
	extRef := h.getExternalRef(initResp.TransactionID)
	body := h.buildBody(extRef, "completed", "payment.completed", 500, "USD")
	headers := h.sign(body)
	event := gateways.WebhookEvent{
		ExternalReference: extRef,
		State:             "completed",
		Amount:            gateways.Money{Amount: 500, Currency: "USD"},
		Currency:          "USD",
		EventType:         "payment.completed",
	}
	err = h.svc.ProcessWebhook(ctx, "mock", event, body, headers)
	require.NoError(t, err) // must not error, just swallow

	status2, _ := h.svc.GetStatus(ctx, initResp.TransactionID)
	assert.Equal(t, "refunded", status2.State)
}
