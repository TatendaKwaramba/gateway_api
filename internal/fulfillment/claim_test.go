package fulfillment

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	notifymock "github.com/freeradius/payments-api/internal/notify/mock"
	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Webhook, poller and admin triggers can fire concurrently for one captured
// payment. The per-transaction claim must serialize them so exactly one
// voucher is issued — never two.
func TestFulfillConcurrentIssuesSingleVoucher(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	svc := NewService(db, notifymock.NewMockProvider())
	ctx := context.Background()

	var gatewayID int64
	require.NoError(t, db.QueryRow(
		`SELECT id FROM payments_paymentgateway WHERE gateway_code = 'mock'`).Scan(&gatewayID))

	txRef := fmt.Sprintf("tx-conc-%d", time.Now().UnixNano())
	res, err := db.Exec(`
		INSERT INTO payments_paymenttransaction
			(transaction_id, gateway_id, amount, currency, state, status,
			 fulfillment_kind, created_at, updated_at)
		VALUES (?, ?, 5.00, 'USD', 'completed', 'completed', 'voucher', NOW(), NOW())`,
		txRef, gatewayID,
	)
	require.NoError(t, err)
	txID, err := res.LastInsertId()
	require.NoError(t, err)

	req := FulfillRequest{
		TransactionID:   txID,
		Amount:          500,
		Currency:        "USD",
		DurationDays:    1,
		FulfillmentKind: "voucher",
	}

	const callers = 4
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Fulfill(ctx, req)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "every caller succeeds — fulfillment is idempotent")
	}

	var vouchers int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM vouchers_voucher WHERE payment_transaction_id = ?`, txID,
	).Scan(&vouchers))
	assert.Equal(t, 1, vouchers, "exactly one voucher for one captured payment")

	var pins int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(DISTINCT username) FROM radcheck WHERE username IN (
			SELECT voucher_pin FROM vouchers_voucher WHERE payment_transaction_id = ?
		)`, txID,
	).Scan(&pins))
	assert.Equal(t, 1, pins, "no orphaned radcheck rows from a losing caller")
}
