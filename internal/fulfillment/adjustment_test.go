package fulfillment

import (
	"context"
	"fmt"
	"testing"
	"time"

	notifymock "github.com/freeradius/payments-api/internal/notify/mock"
	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subscriptionAdjustmentCore: the paid subscription_adjustment fulfillment
// path and the free AdjustSubscription entry share it — buyer verification,
// server-side proration recompute (underpay/tamper guard), plan swap with
// unchanged end date, and the adjustment audit row.
func TestSubscriptionAdjustment(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	svc := NewService(db, notifymock.NewMockProvider())
	ctx := context.Background()

	// seed creates a fresh buyer + old/new plans + an active subscription
	// 7 days into a 30-day period. Phones vary per subtest so contact-based
	// buyer resolution never collides across cases.
	seed := func(t *testing.T, tag string) (buyerID, oldPlanID, newPlanID, subID int64) {
		t.Helper()
		phone := fmt.Sprintf("07712345%s", tag)
		res, err := db.Exec(`
			INSERT INTO authentication_customer (customer_id, customer_name, customer_phone)
			VALUES (?, 'CoA Buyer', ?)
		`, "coa-cust-"+tag, phone)
		require.NoError(t, err)
		buyerID, _ = res.LastInsertId()

		res, err = db.Exec(`
			INSERT INTO services_subscriptionplan
				(name, billing_period_days, download_speed, upload_speed, price_minor, currency, is_active)
			VALUES ('Old Plan', 30, 5000, 2000, 30000, 'USD', 1)
		`)
		require.NoError(t, err)
		oldPlanID, _ = res.LastInsertId()

		res, err = db.Exec(`
			INSERT INTO services_subscriptionplan
				(name, billing_period_days, download_speed, upload_speed, price_minor, currency, is_active)
			VALUES ('New Plan', 30, 10000, 5000, 60000, 'USD', 1)
		`)
		require.NoError(t, err)
		newPlanID, _ = res.LastInsertId()

		start := time.Now().AddDate(0, 0, -7)
		end := start.AddDate(0, 0, 30)
		res, err = db.Exec(`
			INSERT INTO subscriptions_subscription
				(customer_id, plan_id, status, start_date, end_date, auto_renew, next_billing_date)
			VALUES (?, ?, 'active', ?, ?, 1, ?)
		`, buyerID, oldPlanID, start, end, end)
		require.NoError(t, err)
		subID, _ = res.LastInsertId()
		return buyerID, oldPlanID, newPlanID, subID
	}

	stateOf := func(t *testing.T, subID int64) (planID int64, status string) {
		t.Helper()
		require.NoError(t, db.QueryRow(`
			SELECT plan_id, status FROM subscriptions_subscription WHERE id = ?
		`, subID).Scan(&planID, &status))
		return planID, status
	}

	adjustReq := func(phone string) FulfillRequest {
		return FulfillRequest{
			CustomerPhone:   phone,
			FulfillmentKind: "subscription_adjustment",
		}
	}

	t.Run("free adjustment swaps the plan and writes the audit row", func(t *testing.T) {
		buyerID, _, newPlanID, subID := seed(t, "01")
		// Identitylink ownership (verified_via otp) bridges E.164 input.
		_, err := db.Exec(`
			INSERT INTO authentication_identitylink (customer_id, kind, value, verified_at, verified_via)
			VALUES (?, 'phone', '263771234501', NOW(), 'otp')
		`, buyerID)
		require.NoError(t, err)

		// 7 days used of 30: credit = 30000*23/30 = 23000, charge = 60000*23/30 = 46000,
		// net = 23000 — not free; the free path must refuse it.
		_, err = svc.AdjustSubscription(ctx, subID, newPlanID, adjustReq("+263771234501"))
		require.Error(t, err, "nonzero net must not apply for free")

		// Downgrade to a plan with identical pricing makes net 0.
		res, err := db.Exec(`
			INSERT INTO services_subscriptionplan
				(name, billing_period_days, download_speed, upload_speed, price_minor, currency, is_active)
			VALUES ('Same-Price Plan', 30, 8000, 4000, 30000, 'USD', 1)
		`)
		require.NoError(t, err)
		samePlanID, _ := res.LastInsertId()

		result, err := svc.AdjustSubscription(ctx, subID, samePlanID, adjustReq("0771234501"))
		require.NoError(t, err)
		require.True(t, result.Success)

		planID, _ := stateOf(t, subID)
		assert.Equal(t, samePlanID, planID)
		var adjStatus string
		var adjNet float64
		require.NoError(t, db.QueryRow(`
			SELECT status, net_amount FROM subscriptions_subscriptionadjustment
			WHERE subscription_id = ? ORDER BY id DESC LIMIT 1
		`, subID).Scan(&adjStatus, &adjNet))
		assert.Equal(t, "completed", adjStatus)
		assert.InDelta(t, 0, adjNet, 0.01)
	})

	t.Run("paid adjustment enforces the recomputed net", func(t *testing.T) {
		_, oldPlanID, newPlanID, subID := seed(t, "02")

		res, err := db.Exec(`
			INSERT INTO payments_paymenttransaction
				(transaction_id, gateway_id, amount, currency, state, status, fulfillment_kind)
			VALUES ('adj-txn-500', 1, 230.00, 'USD', 'completed', 'completed', 'subscription_adjustment')
		`)
		require.NoError(t, err)
		txnID, err := res.LastInsertId()
		require.NoError(t, err)
		require.NotZero(t, txnID, "seeded transaction must exist")

		// net recomputes to 22000+ (7/30 days, 30000 -> 60000): pay a token
		// amount and fail.
		_, err = svc.fulfillSubscriptionAdjustment(ctx, FulfillRequest{
			TransactionID:   txnID,
			Amount:          100,
			PlanID:          newPlanID,
			SubscriptionID:  subID,
			CustomerPhone:   "0771234502",
			FulfillmentKind: "subscription_adjustment",
		})
		require.Error(t, err, "underpaid adjustment must be rejected")
		planID, _ := stateOf(t, subID)
		assert.Equal(t, oldPlanID, planID, "plan must not change on rejected adjustment")

		// Pay the recomputed net: swap succeeds and the transaction links.
		var result *FulfillResult
		result, err = svc.fulfillSubscriptionAdjustment(ctx, FulfillRequest{
			TransactionID:   txnID,
			Amount:          23000,
			PlanID:          newPlanID,
			SubscriptionID:  subID,
			CustomerPhone:   "0771234502",
			FulfillmentKind: "subscription_adjustment",
		})
		require.NoError(t, err)
		require.True(t, result.Success)

		planID, status := stateOf(t, subID)
		assert.Equal(t, newPlanID, planID)
		assert.Equal(t, "active", status, "end date unchanged — subscription stays active")

		var txnPIN string
		require.NoError(t, db.QueryRow(`
			SELECT voucher_pin FROM payments_paymenttransaction WHERE id = ?
		`, txnID).Scan(&txnPIN))
		assert.Contains(t, txnPIN, "adj:")

		// radreply carries the new plan's speeds for the next login.
		var rate string
		require.NoError(t, db.QueryRow(`
			SELECT value FROM radreply WHERE username = ? AND attribute = 'Mikrotik-Rate-Limit'
		`, "coa-cust-02").Scan(&rate))
		assert.Equal(t, "10000k/5000k", rate)
	})

	t.Run("stranger contact cannot adjust someone else's subscription", func(t *testing.T) {
		_, _, newPlanID, subID := seed(t, "03")
		_, err := svc.AdjustSubscription(ctx, subID, newPlanID, adjustReq("0779999999"))
		require.Error(t, err)
		_, status := stateOf(t, subID)
		assert.Equal(t, "active", status)
	})

	t.Run("non-active subscriptions cannot be adjusted", func(t *testing.T) {
		_, _, newPlanID, subID := seed(t, "04")
		_, err := db.Exec(`UPDATE subscriptions_subscription SET status = 'suspended' WHERE id = ?`, subID)
		require.NoError(t, err)
		_, err = svc.AdjustSubscription(ctx, subID, newPlanID, adjustReq("0771234504"))
		require.ErrorContains(t, err, "only active")
	})
}
