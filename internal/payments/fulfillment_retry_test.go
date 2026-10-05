package payments

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/freeradius/payments-api/internal/fulfillment"
	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingFulfillment fails every Fulfill call and counts the attempts.
type failingFulfillment struct {
	calls int
}

func (f *failingFulfillment) Fulfill(ctx context.Context, req fulfillment.FulfillRequest) (*fulfillment.FulfillResult, error) {
	f.calls++
	return nil, errors.New("fulfillment: simulated outage")
}

func (f *failingFulfillment) Rollback(ctx context.Context, voucherPIN string) error {
	return nil
}

// A fulfillment failure happens after the money is captured: the transaction
// must stay `completed` (voucher_pin NULL — retryable, visible to support and
// refund flows) instead of flipping to the terminal `failed` state, and the
// attempt must be retried before giving up.
func TestTriggerFulfillmentFailureKeepsCompleted(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	oldDelay := fulfillmentRetryDelay
	fulfillmentRetryDelay = 5 * time.Millisecond
	defer func() { fulfillmentRetryDelay = oldDelay }()

	var gatewayID int64
	require.NoError(t, db.QueryRow(
		`SELECT id FROM payments_paymentgateway WHERE gateway_code = 'mock'`).Scan(&gatewayID))

	res, err := db.Exec(`
		INSERT INTO payments_paymenttransaction
			(transaction_id, gateway_id, amount, currency, state, status,
			 fulfillment_kind, created_at, updated_at)
		VALUES (?, ?, 5.00, 'USD', 'completed', 'completed', 'voucher', NOW(), NOW())`,
		fmt.Sprintf("tx-retry-%d", time.Now().UnixNano()), gatewayID,
	)
	require.NoError(t, err)
	txID, err := res.LastInsertId()
	require.NoError(t, err)

	stub := &failingFulfillment{}
	svc := NewService(db, nil, stub, nil, "USD")

	svc.triggerFulfillment(context.Background(), txID)

	require.Equal(t, fulfillmentMaxAttempts, stub.calls, "transient failures are retried before giving up")

	var state, voucherPin string
	require.NoError(t, db.QueryRow(`
		SELECT state, COALESCE(voucher_pin, '')
		FROM payments_paymenttransaction
		WHERE id = ?
	`, txID).Scan(&state, &voucherPin))
	assert.Equal(t, "completed", state, "captured payment must not become terminal failed")
	assert.Empty(t, voucherPin, "no voucher issued")
}
