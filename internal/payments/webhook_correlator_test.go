package payments

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/freeradius/payments-api/internal/testutil"
)

// TestProcessWebhookCorrelatorFallback: EcoCash webhooks can reference a
// transaction id that differs from the charge-response id (sandbox observed:
// charge id 2032334847, webhook id 9738, same payment). The service must fall
// back to the echoed clientCorrelator persisted in gateway_response, and
// still audit unmatched webhooks with a NULL transaction reference.
func TestProcessWebhookCorrelatorFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}

	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err, "spin up test DB")
	defer cleanup()

	registry := gateways.NewRegistry()
	svc := NewService(db, registry, stubFulfillment{}, nil, "USD")
	ctx := context.Background()

	seedTxn := func(t *testing.T, ref, correlator string) int64 {
		t.Helper()
		res, err := db.Exec(`
			INSERT INTO payments_paymenttransaction
				(transaction_id, gateway_id, external_reference, amount, currency, state, status,
				 fulfillment_kind, idempotency_key, gateway_response, customer_phone,
				 created_at, updated_at)
			VALUES (?, 1, ?, ?, 'USD', 'initiated', 'initiated', 'voucher', ?, ?, ?, NOW(), NOW())
		`, "txn-"+ref, ref, "5.00", "idem-"+ref, correlator, "+263779070941")
		require.NoError(t, err, "seed transaction")
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}

	stateOf := func(t *testing.T, id int64) string {
		t.Helper()
		var state string
		require.NoError(t, db.QueryRow(`SELECT state FROM payments_paymenttransaction WHERE id = ?`, id).Scan(&state))
		return state
	}

	byCorrelatorID := seedTxn(t, "charge-id-1", `{"clientCorrelator": "corr-echo-1"}`)
	byPhoneID := seedTxn(t, "charge-id-2", `{}`)

	t.Run("matched by echoed clientCorrelator", func(t *testing.T) {
		require.NoError(t, svc.ProcessWebhook(ctx, "mock", gateways.WebhookEvent{
			ExternalReference: "9738",
			EventType:         EventTypePaymentCompleted,
			State:             "completed",
			Amount:            gateways.Money{Amount: 500, Currency: "USD"},
			Metadata:          map[string]string{"clientCorrelator": "corr-echo-1"},
		}, []byte(`{}`), http.Header{}))
		assert.Equal(t, "completed", stateOf(t, byCorrelatorID))
	})

	t.Run("matched by single pending MSISDN fallback", func(t *testing.T) {
		require.NoError(t, svc.ProcessWebhook(ctx, "mock", gateways.WebhookEvent{
			ExternalReference: "9739",
			EventType:         EventTypePaymentCompleted,
			State:             "completed",
			Amount:            gateways.Money{Amount: 500, Currency: "USD"},
			Metadata:          map[string]string{"endUserId": "0779070941"},
		}, []byte(`{}`), http.Header{}))
		assert.Equal(t, "completed", stateOf(t, byPhoneID))
	})

	t.Run("unmatched webhook is audited with NULL transaction", func(t *testing.T) {
		err := svc.ProcessWebhook(ctx, "mock", gateways.WebhookEvent{
			ExternalReference: "0000",
			EventType:         EventTypePaymentCompleted,
			State:             "completed",
			Amount:            gateways.Money{Amount: 500, Currency: "USD"},
			Metadata:          map[string]string{},
		}, []byte(`{"id":0000}`), http.Header{})
		require.Error(t, err)

		var txnRef interface{}
		require.NoError(t, db.QueryRow(`
			SELECT transaction_id FROM payments_paymentwebhooklog
			WHERE external_reference = '0000'
		`).Scan(&txnRef))
		assert.Nil(t, txnRef, "unmatched webhook must audit with NULL transaction_id")
	})
}
