package payments

import (
	"context"
	"database/sql"
	"strconv"
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

// TestResolveTransactionID covers the reference forms the mobile app sends to
// GET /payments/{transaction_id}/status: the numeric database id and the public
// UUID (transaction_id_str), plus the malformed / unknown rejection paths.
func TestResolveTransactionID(t *testing.T) {
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
	id := seedTransaction(t, db, uuidStr, time.Now())

	t.Run("numeric id", func(t *testing.T) {
		got, err := svc.ResolveTransactionID(ctx, strconv.FormatInt(id, 10))
		require.NoError(t, err)
		require.Equal(t, id, got)
	})

	t.Run("public uuid", func(t *testing.T) {
		got, err := svc.ResolveTransactionID(ctx, uuidStr)
		require.NoError(t, err)
		require.Equal(t, id, got)
	})

	t.Run("malformed reference", func(t *testing.T) {
		_, err := svc.ResolveTransactionID(ctx, "not-a-uuid")
		require.ErrorIs(t, err, ErrInvalidTransactionRef)
	})

	t.Run("unknown uuid", func(t *testing.T) {
		_, err := svc.ResolveTransactionID(ctx, "00000000-0000-4000-8000-000000000000")
		require.ErrorIs(t, err, ErrTransactionNotFound)
	})
}

func seedTransaction(t *testing.T, db *sql.DB, uuidStr string, createdAt time.Time) int64 {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO payments_paymenttransaction
			(transaction_id, gateway_id, amount, currency, state, status,
			 idempotency_key, created_at, updated_at)
		VALUES (?, 1, 12.00, 'USD', 'completed', 'completed', ?, ?, ?)`,
		uuidStr, "idem-"+uuidStr, createdAt, createdAt,
	)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}
