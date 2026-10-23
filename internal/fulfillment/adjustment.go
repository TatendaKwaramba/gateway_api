package fulfillment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/freeradius/payments-api/internal/metrics"
)

// resolveBuyerCustomerID is the find-only counterpart of EnsureBuyerCustomer:
// it applies the same identitylink tier ranking (otp > google > purchase >
// other, then contact-column fallback) but never mints rows, so an adjustment
// can only ever attach to the customer who already owns the subscription.
// Returns 0 when no buyer can be resolved.
func (s *Service) resolveBuyerCustomerID(ctx context.Context, req FulfillRequest) (int64, error) {
	kind, value := contactKind(req)
	if kind == "" {
		return 0, nil
	}

	const tierRank = `
		CASE verified_via
			WHEN 'otp' THEN 1
			WHEN 'google' THEN 2
			WHEN 'purchase' THEN 3
			ELSE 4
		END`
	var customerID int64
	query := fmt.Sprintf(`
		SELECT customer_id FROM authentication_identitylink
		WHERE kind = ? AND value = ?
		ORDER BY %s, verified_at DESC, id DESC
		LIMIT 1
	`, tierRank)
	args := []interface{}{kind, value}
	if kind == "phone" && len(value) >= 9 {
		query = fmt.Sprintf(`
		SELECT customer_id FROM authentication_identitylink
		WHERE kind = 'phone' AND (value = ? OR RIGHT(value, 9) = ?)
		ORDER BY %s, value = ? DESC, verified_at DESC, id DESC
		LIMIT 1
	`, tierRank)
		args = []interface{}{value, phoneSuffix9(value), value}
	}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&customerID)
	if err == nil {
		return customerID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("fulfillment: identitylink lookup: %w", err)
	}

	column := "customer_phone"
	compare := req.CustomerPhone
	if kind == "email" {
		column = "customer_email"
		compare = value
	}
	err = s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT id FROM authentication_customer WHERE %s = ? LIMIT 1
	`, column), compare).Scan(&customerID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("fulfillment: customer contact lookup: %w", err)
	}
	return customerID, nil
}

// subscriptionAdjustmentCore swaps the plan on an existing active
// subscription. Shared by the paid path (transaction-scoped fulfillment,
// transactionID > 0 for the audit link) and the free apply endpoint
// (transactionID = 0, paidAmount = 0 and the caller has already verified
// the recomputed net is 0).
//
// Guards, all server-side:
//   - subscription exists and is active
//   - buyer contact resolves to the subscription's owner (no create)
//   - old and new plan currencies match
//   - paidAmount >= recomputed proration net (underpay/tamper guard)
//   - plan actually changes (idempotent no-op otherwise)
//
// End date and next billing date are untouched: the customer already owns
// the current period, the payment only covers the difference.
//
// RADIUS artifacts (radreply/radusergroup/VLAN attrs) are rewritten for the
// new plan so the new speeds apply at next login; the PPPoE password in
// radcheck is left untouched.
func (s *Service) subscriptionAdjustmentCore(ctx context.Context, subID, newPlanID int64, paidAmount int64, req FulfillRequest, transactionID int64) (*FulfillResult, error) {
	if subID <= 0 {
		return nil, fmt.Errorf("fulfillment: adjustment requires subscription_id")
	}
	if newPlanID <= 0 {
		return nil, fmt.Errorf("fulfillment: adjustment requires new plan_id")
	}

	buyerID, err := s.resolveBuyerCustomerID(ctx, req)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("fulfillment: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var subCustomerID, oldPlanID int64
	var status string
	var startDate, endDate time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT customer_id, plan_id, status, start_date, end_date
		FROM subscriptions_subscription
		WHERE id = ?
		FOR UPDATE
	`, subID).Scan(&subCustomerID, &oldPlanID, &status, &startDate, &endDate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("fulfillment: subscription %d not found", subID)
	}
	if err != nil {
		return nil, fmt.Errorf("fulfillment: query subscription: %w", err)
	}
	if status != "active" {
		return nil, fmt.Errorf("fulfillment: subscription is %s, only active subscriptions can be adjusted", status)
	}
	if buyerID != 0 && buyerID != subCustomerID {
		return nil, fmt.Errorf("fulfillment: buyer contact does not own subscription %d", subID)
	}
	if buyerID == 0 {
		return nil, fmt.Errorf("fulfillment: adjustment requires a contact matching the subscription owner")
	}

	var plan struct {
		ID                int64
		Name              string
		BillingPeriodDays int
		DownloadSpeed     int
		UploadSpeed       int
		PriceMinor        int64
		Currency          string
		DefaultPoolID     sql.NullInt64
		VlanID            sql.NullInt64
	}
	err = tx.QueryRowContext(ctx, `
		SELECT id, name, billing_period_days, download_speed, upload_speed,
		       price_minor, COALESCE(currency, ''), default_framed_ip_pool_id, vlan_id
		FROM services_subscriptionplan
		WHERE id = ? AND is_active = TRUE
	`, newPlanID).Scan(
		&plan.ID, &plan.Name, &plan.BillingPeriodDays, &plan.DownloadSpeed,
		&plan.UploadSpeed, &plan.PriceMinor, &plan.Currency,
		&plan.DefaultPoolID, &plan.VlanID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("fulfillment: new subscription plan %d not found or inactive", newPlanID)
	}
	if err != nil {
		return nil, fmt.Errorf("fulfillment: query new plan: %w", err)
	}

	var oldPriceMinor int64
	var oldCurrency string
	var oldName string
	err = tx.QueryRowContext(ctx, `
		SELECT name, price_minor, COALESCE(currency, '')
		FROM services_subscriptionplan WHERE id = ?
	`, oldPlanID).Scan(&oldName, &oldPriceMinor, &oldCurrency)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: query old plan: %w", err)
	}

	if plan.Currency != "" && oldCurrency != "" && plan.Currency != oldCurrency {
		return nil, fmt.Errorf("fulfillment: plan currency mismatch: old %s vs new %s", oldCurrency, plan.Currency)
	}

	billingDays := plan.BillingPeriodDays
	if billingDays <= 0 {
		billingDays = int(endDate.Sub(startDate).Hours() / 24)
	}
	daysRemaining := int(time.Until(endDate).Hours() / 24)
	credit, charge, net := ComputeProration(oldPriceMinor, plan.PriceMinor, billingDays, daysRemaining)
	netRounded := int64(math.Round(net))

	if paidAmount > 0 && paidAmount < netRounded {
		return nil, fmt.Errorf(
			"fulfillment: paid amount %d below recomputed adjustment net %d",
			paidAmount, netRounded,
		)
	}
	if paidAmount == 0 && transactionID == 0 && netRounded > 0 {
		// Free-apply path defense in depth: the caller already checked, but
		// never apply a nonzero adjustment without payment.
		return nil, fmt.Errorf("fulfillment: adjustment requires payment of %d", netRounded)
	}

	if oldPlanID == newPlanID {
		return nil, fmt.Errorf("fulfillment: subscription already on plan %d", newPlanID)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE subscriptions_subscription
		SET plan_id = ?, updated_at = NOW()
		WHERE id = ?
	`, newPlanID, subID)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: update subscription plan: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return nil, fmt.Errorf("fulfillment: subscription %d vanished during adjustment", subID)
	}

	// Audit row (Django subscriptions.SubscriptionAdjustment schema).
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO subscriptions_subscriptionadjustment (
			subscription_id, old_plan_id, new_plan_id,
			proration_credit, proration_charge, net_amount, status,
			payment_transaction_id, reason, created_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, 'completed', ?, 'customer self-service plan change', NOW(), NOW())
	`, subID, oldPlanID, newPlanID, credit, charge, net, nullableInt(transactionID)); err != nil {
		return nil, fmt.Errorf("fulfillment: insert adjustment audit row: %w", err)
	}

	// Rewrite RADIUS artifacts for the new plan (username keys off the
	// subscription's customer record — same convention fulfillSubscription
	// uses: authentication_customer.customer_id holds the login username).
	var customerUsername string
	err = tx.QueryRowContext(ctx, `
		SELECT customer_id FROM authentication_customer WHERE id = ?
	`, subCustomerID).Scan(&customerUsername)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: lookup customer username: %w", err)
	}

	dlSpeed := fmt.Sprintf("%dk", plan.DownloadSpeed)
	ulSpeed := fmt.Sprintf("%dk", plan.UploadSpeed)
	rateLimit := dlSpeed + "/" + ulSpeed

	_, err = tx.ExecContext(ctx, "DELETE FROM radreply WHERE username = ?", customerUsername)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: clear radreply: %w", err)
	}
	radreplyAttrs := []struct{ attr, value string }{
		{"Reply-Message", "Cached by Betta Technologies"},
		{"Mikrotik-Rate-Limit", rateLimit},
		{"Mikrotik-Recv-Limit", "1000000000000"},
		{"Mikrotik-Xmit-Limit", "1000000000000"},
		{"Acct-Interim-Interval", "300"},
	}
	for _, a := range radreplyAttrs {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO radreply (username, attribute, op, value) VALUES (?, ?, '=', ?)",
			customerUsername, a.attr, a.value,
		)
		if err != nil {
			return nil, fmt.Errorf("fulfillment: insert radreply %s: %w", a.attr, err)
		}
	}
	if plan.DefaultPoolID.Valid {
		var poolName string
		err = tx.QueryRowContext(ctx,
			"SELECT name FROM ipam_ippool WHERE id = ?", plan.DefaultPoolID.Int64,
		).Scan(&poolName)
		if err == nil && poolName != "" {
			_, _ = tx.ExecContext(ctx,
				"INSERT INTO radreply (username, attribute, op, value) VALUES (?, 'Framed-Pool', '=', ?)",
				customerUsername, poolName,
			)
		}
	}
	if plan.VlanID.Valid {
		vlanIDStr := fmt.Sprintf("%d", plan.VlanID.Int64)
		for _, a := range []struct{ attr, value string }{
			{"Tunnel-Type", "VLAN"},
			{"Tunnel-Medium-Type", "IEEE-802"},
			{"Tunnel-Private-Group-ID", vlanIDStr},
		} {
			_, err = tx.ExecContext(ctx,
				"INSERT INTO radreply (username, attribute, op, value) VALUES (?, ?, '=', ?)",
				customerUsername, a.attr, a.value,
			)
			if err != nil {
				return nil, fmt.Errorf("fulfillment: insert radreply %s: %w", a.attr, err)
			}
		}
	}

	_, err = tx.ExecContext(ctx,
		"DELETE FROM radusergroup WHERE username = ?", customerUsername)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: clear radusergroup: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO radusergroup (username, groupname, priority) VALUES (?, ?, 1)",
		customerUsername, fmt.Sprintf("plan-%d", plan.ID))
	if err != nil {
		return nil, fmt.Errorf("fulfillment: insert radusergroup: %w", err)
	}

	voucherPIN := fmt.Sprintf("adj:%d:%d", subID, newPlanID)
	if transactionID > 0 {
		_, err = tx.ExecContext(ctx, `
			UPDATE payments_paymenttransaction
			SET voucher_pin = ?, updated_at = NOW()
			WHERE id = ?
		`, voucherPIN, transactionID)
		if err != nil {
			return nil, fmt.Errorf("fulfillment: update transaction: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("fulfillment: commit: %w", err)
	}

	slog.Info("fulfillment: subscription adjusted",
		slog.Int64("transaction_id", transactionID),
		slog.Int64("subscription_id", subID),
		slog.Int64("old_plan_id", oldPlanID),
		slog.Int64("new_plan_id", newPlanID),
		slog.Float64("credit", credit),
		slog.Float64("charge", charge),
		slog.Float64("net", net),
		slog.String("customer_username", customerUsername),
	)

	metrics.RecordFulfillmentSuccess("subscription_adjustment")

	if req.CustomerPhone != "" {
		msgBody := fmt.Sprintf(
			"Your plan changed to %s. Effective immediately — your billing period end date is unchanged.",
			plan.Name,
		)
		if notifyErr := s.sendNotification(ctx, transactionID, req.CustomerPhone, msgBody); notifyErr != nil {
			slog.Error("fulfillment: adjustment notification failed",
				slog.Int64("transaction_id", transactionID),
				slog.Any("error", notifyErr),
			)
		}
		s.sendPushForPhone(ctx, transactionID, req.CustomerPhone, "Plan changed", msgBody)
	}

	return &FulfillResult{Success: true, VoucherPIN: voucherPIN}, nil
}

// fulfillSubscriptionAdjustment is the paid path: the transaction completed
// and the state machine called Fulfill with kind subscription_adjustment.
func (s *Service) fulfillSubscriptionAdjustment(ctx context.Context, req FulfillRequest) (*FulfillResult, error) {
	return s.subscriptionAdjustmentCore(ctx, req.SubscriptionID, req.PlanID, req.Amount, req, req.TransactionID)
}

// AdjustSubscription is the free path (payments service calls it directly):
// paidAmount = 0 and the caller has verified the recomputed net is 0.
func (s *Service) AdjustSubscription(ctx context.Context, subID, newPlanID int64, req FulfillRequest) (*FulfillResult, error) {
	return s.subscriptionAdjustmentCore(ctx, subID, newPlanID, 0, req, 0)
}

// nullableInt maps 0 to SQL NULL for optional FK columns.
func nullableInt(v int64) interface{} {
	if v <= 0 {
		return nil
	}
	return v
}
