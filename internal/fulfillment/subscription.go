package fulfillment

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/freeradius/payments-api/internal/metrics"
)

// generateSubscriberPassword creates a 16-char alphanumeric password for PPPoE.
// Unlike voucher PINs (numeric only), subscriber passwords include letters for
// MS-CHAPv2 compatibility and stronger security.
func generateSubscriberPassword() (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 16)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		b[i] = chars[n.Int64()]
	}
	return string(b), nil
}

func (s *Service) fulfillSubscription(ctx context.Context, req FulfillRequest) (*FulfillResult, error) {
	if req.PlanID <= 0 {
		return nil, fmt.Errorf("fulfillment: subscription requires plan_id")
	}

	var plan struct {
		ID                int64
		Name              string
		BillingPeriodDays int
		DownloadSpeed     int
		UploadSpeed       int
		PriceMinor        int64
		DefaultPoolID     sql.NullInt64
		VlanID            sql.NullInt64
	}

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, billing_period_days, download_speed, upload_speed,
		       price_minor, default_framed_ip_pool_id, vlan_id
		FROM services_subscriptionplan
		WHERE id = ? AND is_active = TRUE
	`, req.PlanID).Scan(
		&plan.ID,
		&plan.Name,
		&plan.BillingPeriodDays,
		&plan.DownloadSpeed,
		&plan.UploadSpeed,
		&plan.PriceMinor,
		&plan.DefaultPoolID,
		&plan.VlanID,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("fulfillment: subscription plan %d not found", req.PlanID)
		}
		return nil, fmt.Errorf("fulfillment: query subscription plan: %w", err)
	}

	customerID, err := s.resolveOrCreateCustomer(ctx, req)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	periodDays := plan.BillingPeriodDays
	if periodDays <= 0 {
		periodDays = 30
	}
	endDate := now.AddDate(0, 0, periodDays)
	nextBilling := endDate

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("fulfillment: begin tx: %w", err)
	}
	defer tx.Rollback()

	var poolID interface{}
	if plan.DefaultPoolID.Valid {
		poolID = plan.DefaultPoolID.Int64
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions_subscription (
			customer_id, plan_id, status, start_date, end_date, auto_renew,
			next_billing_date, framed_ip_pool_id, created_at, updated_at
		) VALUES (?, ?, 'active', ?, ?, TRUE, ?, ?, NOW(), NOW())
	`, customerID, plan.ID, now, endDate, nextBilling, poolID)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: insert subscription: %w", err)
	}

	subscriptionID, _ := res.LastInsertId()

	var customerUsername string
	err = tx.QueryRowContext(ctx, `
		SELECT customer_id FROM authentication_customer WHERE id = ?
	`, customerID).Scan(&customerUsername)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: lookup customer_id: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		DELETE FROM radcheck WHERE username = ? AND attribute = 'Cleartext-Password'
	`, customerUsername)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: clear radcheck password: %w", err)
	}

	subPassword, err := generateSubscriberPassword()
	if err != nil {
		return nil, fmt.Errorf("fulfillment: generate subscriber password: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO radcheck (username, attribute, op, value)
		VALUES (?, 'Cleartext-Password', ':=', ?)
	`, customerUsername, subPassword)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: upsert radcheck password: %w", err)
	}

	// Store password in customer record so Beta App can display it
	_, err = tx.ExecContext(ctx, `
		UPDATE authentication_customer SET customer_password = ? WHERE customer_id = ?
	`, subPassword, customerUsername)
	if err != nil {
		slog.Warn("fulfillment: could not store subscriber password on customer record",
			"customer_id", customerUsername, "error", err)
	}

	// Write radreply entries for SQL authorize fallback.
	// Keeps radreply fresh so FreeRADIUS can authorize from SQL when API is down.
	dlSpeed := fmt.Sprintf("%dk", plan.DownloadSpeed)
	ulSpeed := fmt.Sprintf("%dk", plan.UploadSpeed)
	rateLimit := dlSpeed + "/" + ulSpeed

	_, err = tx.ExecContext(ctx, "DELETE FROM radreply WHERE username = ?", customerUsername)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: clear radreply: %w", err)
	}

	radreplyAttrs := []struct{ attr, value string }{
		{"Reply-Message", "Cached by Flash API"},
		{"Mikrotik-Rate-Limit", rateLimit},
		{"Mikrotik-Recv-Limit", "1000000000000"},
		{"Mikrotik-Xmit-Limit", "1000000000000"},
		{"Acct-Interim-Interval", "300"},
		{"Session-Timeout", fmt.Sprintf("%d", periodDays*86400)},
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

	// Add Framed-Pool if pool is configured
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

	// Write radusergroup for plan-based speed tiers
	groupName := fmt.Sprintf("plan-%d", plan.ID)
	_, err = tx.ExecContext(ctx,
		"DELETE FROM radusergroup WHERE username = ?", customerUsername)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: clear radusergroup: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO radusergroup (username, groupname, priority) VALUES (?, ?, 1)",
		customerUsername, groupName)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: insert radusergroup: %w", err)
	}

	// Write VLAN Tunnel-* attributes if plan has a VLAN
	if plan.VlanID.Valid {
		vlanIDStr := fmt.Sprintf("%d", plan.VlanID.Int64)
		vlanAttrs := []struct{ attr, value string }{
			{"Tunnel-Type", "VLAN"},
			{"Tunnel-Medium-Type", "IEEE-802"},
			{"Tunnel-Private-Group-ID", vlanIDStr},
		}
		for _, a := range vlanAttrs {
			_, err = tx.ExecContext(ctx,
				"INSERT INTO radreply (username, attribute, op, value) VALUES (?, ?, '=', ?)",
				customerUsername, a.attr, a.value,
			)
			if err != nil {
				return nil, fmt.Errorf("fulfillment: insert radreply %s: %w", a.attr, err)
			}
		}
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE payments_paymenttransaction
		SET completed_at = NOW(), updated_at = NOW(),
		    voucher_pin = CONCAT('sub:', ?)
		WHERE id = ?
	`, subscriptionID, req.TransactionID)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: update transaction: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("fulfillment: commit: %w", err)
	}

	slog.Info("fulfillment: subscription created",
		slog.Int64("transaction_id", req.TransactionID),
		slog.Int64("subscription_id", subscriptionID),
		slog.Int64("plan_id", plan.ID),
		slog.String("customer_username", customerUsername),
	)

	metrics.RecordFulfillmentSuccess("subscription")

	if req.CustomerPhone != "" {
		msgBody := fmt.Sprintf(
			"Your %s subscription is active until %s. Username: %s",
			plan.Name,
			endDate.Format("2006-01-02"),
			customerUsername,
		)
		if notifyErr := s.sendNotification(ctx, req.TransactionID, req.CustomerPhone, msgBody); notifyErr != nil {
			slog.Error("fulfillment: subscription notification failed",
				slog.Int64("transaction_id", req.TransactionID),
				slog.Any("error", notifyErr),
			)
		}
	}

	return &FulfillResult{Success: true}, nil
}

func (s *Service) resolveOrCreateCustomer(ctx context.Context, req FulfillRequest) (int64, error) {
	if req.CustomerPhone == "" && req.CustomerEmail == "" {
		return 0, fmt.Errorf("fulfillment: customer phone or email required for subscription")
	}

	var customerID int64
	var lookupErr error

	orgID, err := s.resolveOrganizationID(ctx, req)
	if err != nil {
		return 0, err
	}
	var orgVal interface{}
	if orgID.Valid {
		orgVal = orgID.Int64
	}

	if req.CustomerEmail != "" {
		lookupErr = s.db.QueryRowContext(ctx, `
			SELECT id FROM authentication_customer WHERE customer_email = ? LIMIT 1
		`, req.CustomerEmail).Scan(&customerID)
	}
	if lookupErr == sql.ErrNoRows && req.CustomerPhone != "" {
		lookupErr = s.db.QueryRowContext(ctx, `
			SELECT id FROM authentication_customer WHERE customer_phone = ? LIMIT 1
		`, req.CustomerPhone).Scan(&customerID)
	}
	if lookupErr == nil {
		if orgID.Valid {
			_, _ = s.db.ExecContext(ctx, `
				UPDATE authentication_customer SET organization_id = ? WHERE id = ? AND organization_id IS NULL
			`, orgID.Int64, customerID)
		}
		return customerID, nil
	}
	if lookupErr != sql.ErrNoRows {
		return 0, fmt.Errorf("fulfillment: lookup customer: %w", lookupErr)
	}

	username := req.CustomerPhone
	if username == "" {
		username = req.CustomerEmail
	}
	customerCode := fmt.Sprintf("sub-%d", req.TransactionID)

	// Resolve referral agent by referral link code (REF-XXXXXXXX from cookie)
	var referredByVal interface{}
	if req.ReferralCode != "" {
		var agentID int64
		if err := s.db.QueryRowContext(ctx, `
			SELECT ap.id FROM sales_agentprofile ap
			JOIN sales_referrallink rl ON rl.agent_id = ap.id
			WHERE rl.code = ? AND rl.is_active = TRUE AND ap.status = 'active'
			LIMIT 1
		`, req.ReferralCode).Scan(&agentID); err == nil {
			referredByVal = agentID
		}
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO authentication_customer (
			customer_id, customer_type, customer_name, customer_email, customer_phone,
			customer_address, customer_city, customer_password, organization_id, referred_by_id,
			radius_username, access_type, is_active, created_at, updated_at
		) VALUES (?, 'subscriber', ?, ?, ?, 'Online checkout', 'N/A', ?, ?, ?, ?, 'subscription', TRUE, NOW(), NOW())
	`, customerCode, username, req.CustomerEmail, req.CustomerPhone, username, orgVal, referredByVal, customerCode)
	if err != nil {
		return 0, fmt.Errorf("fulfillment: create customer: %w", err)
	}
	return res.LastInsertId()
}
