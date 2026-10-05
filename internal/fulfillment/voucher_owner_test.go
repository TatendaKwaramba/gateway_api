package fulfillment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	notifymock "github.com/freeradius/payments-api/internal/notify/mock"
	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Voucher fulfillment ownership (M-CIM): the checkout contact wins the voucher,
// the identity-stamped owner keeps the transaction, and an anonymous purchase
// stays NULL (never 0 — that violates vouchers_voucher.fk_voucher_customer).
func TestVoucherFulfillmentOwnership(t *testing.T) {
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

	newTransaction := func(t *testing.T, customerID *int64, phone, email string) int64 {
		t.Helper()
		res, err := db.Exec(`
			INSERT INTO payments_paymenttransaction
				(transaction_id, gateway_id, amount, currency, state, status,
				 customer_id, customer_phone, customer_email, fulfillment_kind, created_at, updated_at)
			VALUES (?, ?, 5.00, 'USD', 'completed', 'completed', ?, ?, ?, 'voucher', NOW(), NOW())
		`, fmt.Sprintf("tx-%d", time.Now().UnixNano()), gatewayID, customerID, phone, email)
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}

	ownerSeq := 0
	owner := func(t *testing.T) int64 {
		t.Helper()
		ownerSeq++
		code := fmt.Sprintf("owner-sub-%d", ownerSeq)
		res, err := db.Exec(`
			INSERT INTO authentication_customer
				(customer_id, customer_type, customer_name, customer_phone, radius_username, is_active)
			VALUES (?, 'subscriber', 'Owner', '263770000001', ?, TRUE)
		`, code, code)
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}

	voucherCustomerID := func(t *testing.T, txID int64) sql.NullInt64 {
		t.Helper()
		var id sql.NullInt64
		err := db.QueryRow(
			`SELECT customer_id FROM vouchers_voucher WHERE payment_transaction_id = ?`, txID,
		).Scan(&id)
		require.False(t, errors.Is(err, sql.ErrNoRows), "voucher row written for tx %d", txID)
		require.NoError(t, err)
		return id
	}

	t.Run("anonymous purchase keeps customer_id NULL on both rows", func(t *testing.T) {
		txID := newTransaction(t, nil, "", "")
		res, err := svc.Fulfill(ctx, FulfillRequest{
			TransactionID: txID, Amount: 500, Currency: "USD",
			DurationDays: 1, FulfillmentKind: "voucher",
		})
		require.NoError(t, err)
		require.NotEmpty(t, res.VoucherPIN)

		assert.False(t, voucherCustomerID(t, txID).Valid, "anonymous voucher has no owner")

		var txCustomer sql.NullInt64
		require.NoError(t, db.QueryRow(
			`SELECT customer_id FROM payments_paymenttransaction WHERE id = ?`, txID).Scan(&txCustomer))
		assert.False(t, txCustomer.Valid, "anonymous transaction keeps customer_id NULL")
	})

	t.Run("identity-stamped purchase without a contact lands on the owner", func(t *testing.T) {
		ownerID := owner(t)
		txID := newTransaction(t, &ownerID, "", "")

		res, err := svc.Fulfill(ctx, FulfillRequest{
			TransactionID: txID, Amount: 500, Currency: "USD",
			DurationDays: 1, FulfillmentKind: "voucher",
			OwnerCustomerID: ownerID,
		})
		require.NoError(t, err)
		require.NotEmpty(t, res.VoucherPIN)

		voucherOwner := voucherCustomerID(t, txID)
		require.True(t, voucherOwner.Valid, "voucher attaches to the identity owner")
		assert.Equal(t, ownerID, voucherOwner.Int64)

		var txCustomer sql.NullInt64
		require.NoError(t, db.QueryRow(
			`SELECT customer_id FROM payments_paymenttransaction WHERE id = ?`, txID).Scan(&txCustomer))
		require.True(t, txCustomer.Valid)
		assert.Equal(t, ownerID, txCustomer.Int64)
	})

	t.Run("typed contact wins the voucher while the owner keeps the transaction", func(t *testing.T) {
		ownerID := owner(t)
		txID := newTransaction(t, &ownerID, "", "")

		res, err := svc.Fulfill(ctx, FulfillRequest{
			TransactionID: txID, Amount: 500, Currency: "USD",
			DurationDays: 1, FulfillmentKind: "voucher",
			CustomerPhone:   "+263771234999",
			OwnerCustomerID: ownerID,
		})
		require.NoError(t, err)
		require.NotEmpty(t, res.VoucherPIN)

		var buyerID int64
		require.NoError(t, db.QueryRow(`
			SELECT id FROM authentication_customer WHERE customer_phone = '+263771234999'
		`).Scan(&buyerID))

		voucherOwner := voucherCustomerID(t, txID)
		require.True(t, voucherOwner.Valid)
		assert.Equal(t, buyerID, voucherOwner.Int64, "typed contact receives the voucher")

		var txCustomer sql.NullInt64
		require.NoError(t, db.QueryRow(
			`SELECT customer_id FROM payments_paymenttransaction WHERE id = ?`, txID).Scan(&txCustomer))
		require.True(t, txCustomer.Valid)
		assert.Equal(t, ownerID, txCustomer.Int64, "purchase stays in the buyer's history")
	})
}
