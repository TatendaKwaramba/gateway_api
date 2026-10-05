package workers

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Captured-but-unfulfilled transactions must be re-driven by the sweep (crash
// or outage recovery), while fulfilled ones and kinds that never carry a
// voucher_pin are left alone — and a row is not re-picked before the cooldown.
func TestPollerRetriesUnfulfilledTransactions(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	var mu sync.Mutex
	var triggered []int64
	p := NewPoller(db, nil, func(id int64) {
		mu.Lock()
		triggered = append(triggered, id)
		mu.Unlock()
	})

	var gatewayID int64
	require.NoError(t, db.QueryRow(
		`SELECT id FROM payments_paymentgateway WHERE gateway_code = 'mock'`).Scan(&gatewayID))

	seq := 0
	seed := func(t *testing.T, kind string, pin interface{}) int64 {
		t.Helper()
		seq++
		res, err := db.Exec(`
			INSERT INTO payments_paymenttransaction
				(transaction_id, gateway_id, amount, currency, state, status,
				 fulfillment_kind, voucher_pin, completed_at, created_at, updated_at)
			VALUES (?, ?, 5.00, 'USD', 'completed', 'completed', ?, ?, NOW(), NOW(), NOW())`,
			fmt.Sprintf("tx-sweep-%d-%d", time.Now().UnixNano(), seq),
			gatewayID, kind, pin,
		)
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}

	unfulfilled := seed(t, "voucher", nil)
	fulfilled := seed(t, "voucher", "1234567890123456")
	topup := seed(t, "topup", nil) // no-op kind: never carries a pin

	p.run(context.Background())

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(triggered) >= 1
	}, 3*time.Second, 20*time.Millisecond, "sweep re-drives the unfulfilled transaction")

	mu.Lock()
	assert.Contains(t, triggered, unfulfilled)
	assert.NotContains(t, triggered, fulfilled, "already fulfilled is left alone")
	assert.NotContains(t, triggered, topup, "kinds without a pin marker are excluded")
	triggered = nil
	mu.Unlock()

	// Cooldown: the pick stamped last_polled_at, so an immediate second run
	// must not re-drive the same row.
	p.run(context.Background())
	time.Sleep(250 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, triggered, "cooldown prevents re-picking on the next tick")
}
