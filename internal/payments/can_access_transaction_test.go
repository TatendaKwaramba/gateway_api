package payments

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/freeradius/payments-api/internal/django"
	"github.com/freeradius/payments-api/internal/fulfillment"
	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/freeradius/payments-api/internal/gateways/mock"
	notifymock "github.com/freeradius/payments-api/internal/notify/mock"
	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/require"
)

// TestCanAccessTransaction covers the owner gate on the payment status/poll
// endpoints: a numeric transaction id may only be read by the identity that
// owns it, matched with the same rules as the payment history list (owner
// customer id, phone last-9 bridge, case-insensitive email).
func TestCanAccessTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}

	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err, "failed to spin up test database")
	defer cleanup()

	registry := gateways.NewRegistry()
	adapter := mock.NewAdapter("test-webhook-secret-min-32-characters-long", "")
	require.NoError(t, registry.Register(adapter))
	svc := NewService(
		db,
		registry,
		fulfillment.NewService(db, notifymock.NewMockProvider()),
		django.NewClient("", ""),
		"USD",
	)

	ctx := context.Background()
	const uuidStr = "81813b50-6793-452f-9ee1-f8f8b5172972"
	id := seedOwnedTransaction(t, db, uuidStr, time.Now(), int64(4242),
		"0771234567", "Buyer@Example.com")

	owner := []int64{4242}

	t.Run("owner customer id", func(t *testing.T) {
		ok, err := svc.CanAccessTransaction(ctx, id, "", "", owner)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("phone last-9 bridge (E.164 token vs local number)", func(t *testing.T) {
		ok, err := svc.CanAccessTransaction(ctx, id, "+263 77 123 4567", "", nil)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("email case-insensitive", func(t *testing.T) {
		ok, err := svc.CanAccessTransaction(ctx, id, "", "  buyer@example.COM ", nil)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("identitylink customer ids", func(t *testing.T) {
		ok, err := svc.CanAccessTransaction(ctx, id, "someone.else@example.org", "", []int64{99, 4242})
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("unrelated identity", func(t *testing.T) {
		ok, err := svc.CanAccessTransaction(ctx, id, "0779999999", "other@example.org", []int64{7})
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("no identity contacts", func(t *testing.T) {
		ok, err := svc.CanAccessTransaction(ctx, id, "", "", nil)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("unknown transaction", func(t *testing.T) {
		ok, err := svc.CanAccessTransaction(ctx, id+9999, "", "", owner)
		require.NoError(t, err)
		require.False(t, ok)
	})
}

// seedOwnedTransaction inserts a transaction that carries a customer owner and
// buyer contacts, returning the new database id.
func seedOwnedTransaction(
	t *testing.T,
	db *sql.DB,
	uuidStr string,
	createdAt time.Time,
	customerID int64,
	phone, email string,
) int64 {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO payments_paymenttransaction
			(transaction_id, gateway_id, amount, currency, state, status,
			 idempotency_key, customer_id, customer_phone, customer_email,
			 created_at, updated_at)
		VALUES (?, 1, 12.00, 'USD', 'completed', 'completed', ?, ?, ?, ?, ?, ?)`,
		uuidStr, "idem-"+uuidStr, customerID, phone, email, createdAt, createdAt,
	)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}
