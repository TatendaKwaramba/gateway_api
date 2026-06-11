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

// fulfillVoucher generates a PIN, writes radcheck rows, vouchers_voucher, updates transaction.
func (s *Service) fulfillVoucher(ctx context.Context, req FulfillRequest) (*FulfillResult, error) {
	var tariffPlan struct {
		ID            int64
		Seconds       int
		DownloadSpeed int
		UploadSpeed   int
		MaxSessions   int
		Price         int64
	}

	if req.PlanID > 0 {
		err := s.db.QueryRowContext(ctx, `
			SELECT id, seconds, download_speed, upload_speed, max_sessions, price
			FROM services_tariffplan
			WHERE id = ? AND is_active = TRUE
		`, req.PlanID).Scan(
			&tariffPlan.ID,
			&tariffPlan.Seconds,
			&tariffPlan.DownloadSpeed,
			&tariffPlan.UploadSpeed,
			&tariffPlan.MaxSessions,
			&tariffPlan.Price,
		)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil, fmt.Errorf("fulfillment: tariff plan %d not found", req.PlanID)
			}
			return nil, fmt.Errorf("fulfillment: failed to query tariff plan: %w", err)
		}
	} else {
		err := s.db.QueryRowContext(ctx, `
			SELECT id, seconds, download_speed, upload_speed, max_sessions, price
			FROM services_tariffplan
			WHERE price = ? AND is_active = TRUE
			LIMIT 1
		`, req.Amount).Scan(
			&tariffPlan.ID,
			&tariffPlan.Seconds,
			&tariffPlan.DownloadSpeed,
			&tariffPlan.UploadSpeed,
			&tariffPlan.MaxSessions,
			&tariffPlan.Price,
		)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil, fmt.Errorf("fulfillment: no tariff plan found for price %d (minor units)", req.Amount)
			}
			return nil, fmt.Errorf("fulfillment: failed to query tariff plan: %w", err)
		}
	}

	pin, err := generatePIN()
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to generate PIN: %w", err)
	}

	orgID, err := s.resolveOrganizationID(ctx, req)
	if err != nil {
		return nil, err
	}

	customerID, err := s.ensureVoucherCustomer(ctx, req, pin, orgID)
	if err != nil {
		return nil, err
	}

	timeLimit := time.Now().Add(time.Duration(tariffPlan.Seconds) * time.Second).Add(2 * time.Hour)
	timeLimitStr := timeLimit.Format("2006-01-02 15:04:05.000000")

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO radcheck (username, attribute, op, value)
		VALUES (?, 'Cleartext-Password', ':=', ?)
	`, pin, pin)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to insert radcheck password: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO radcheck (username, attribute, op, value)
		VALUES (?, 'Time-Limit', ':=', ?)
	`, pin, timeLimitStr)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to insert radcheck time-limit: %w", err)
	}

	// Write radreply entries for SQL authorize fallback.
	// Keeps radreply fresh so FreeRADIUS can authorize from SQL when API is down.
	dlSpeed := fmt.Sprintf("%dk", tariffPlan.DownloadSpeed)
	ulSpeed := fmt.Sprintf("%dk", tariffPlan.UploadSpeed)
	rateLimit := dlSpeed + "/" + ulSpeed

	_, err = tx.ExecContext(ctx, "DELETE FROM radreply WHERE username = ?", pin)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: clear radreply: %w", err)
	}

	radreplyAttrs := []struct{ attr, value string }{
		{"Reply-Message", "Cached by Flash API"},
		{"Mikrotik-Rate-Limit", rateLimit},
		{"Mikrotik-Recv-Limit", "1000000000000"},
		{"Mikrotik-Xmit-Limit", "1000000000000"},
		{"Acct-Interim-Interval", "300"},
		{"Session-Timeout", fmt.Sprintf("%d", tariffPlan.Seconds)},
		{"Simultaneous-Use", fmt.Sprintf("%d", tariffPlan.MaxSessions)},
	}
	for _, a := range radreplyAttrs {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO radreply (username, attribute, op, value) VALUES (?, ?, '=', ?)",
			pin, a.attr, a.value,
		)
		if err != nil {
			return nil, fmt.Errorf("fulfillment: insert radreply %s: %w", a.attr, err)
		}
	}

	var radcheckID int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM radcheck WHERE username = ? AND attribute = 'Time-Limit'
		ORDER BY id DESC LIMIT 1
	`, pin).Scan(&radcheckID)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to lookup radcheck id: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO vouchers_voucher (
			radcheck_id, tariff_plan_id, voucher_amount, voucher_serial_number,
			voucher_pin, voucher_expired_date, voucher_response_message, voucher_status,
			payment_transaction_id, created_by_id, updated_by_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, 1, 1, NOW(), NOW())
	`, radcheckID, tariffPlan.ID, tariffPlan.Price, pin, pin, timeLimit, "Payment voucher", req.TransactionID)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to insert voucher: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE payments_paymenttransaction
		SET voucher_pin = ?, customer_id = ?, completed_at = NOW(), updated_at = NOW()
		WHERE id = ?
	`, pin, customerID, req.TransactionID)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to update transaction: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("fulfillment: failed to commit transaction: %w", err)
	}

	slog.Info("fulfillment: voucher created",
		slog.Int64("transaction_id", req.TransactionID),
		slog.String("voucher_pin", pin),
		slog.Int64("tariff_plan_id", tariffPlan.ID),
		slog.Int("tariff_seconds", tariffPlan.Seconds),
	)

	metrics.RecordFulfillmentSuccess("voucher")

	if req.CustomerPhone != "" {
		msgBody := fmt.Sprintf(
			"Your internet voucher PIN is %s. Valid for %s. Enjoy browsing!",
			pin,
			formatDuration(tariffPlan.Seconds),
		)
		if notifyErr := s.sendNotification(ctx, req.TransactionID, req.CustomerPhone, msgBody); notifyErr != nil {
			slog.Error("fulfillment: notification failed",
				slog.Int64("transaction_id", req.TransactionID),
				slog.Any("error", notifyErr),
			)
			metrics.RecordFulfillmentFailure("voucher", "notification_failed")
		}
	}

	return &FulfillResult{
		Success:    true,
		VoucherPIN: pin,
	}, nil
}

func (s *Service) fulfillTopup(ctx context.Context, req FulfillRequest) (*FulfillResult, error) {
	return &FulfillResult{Success: true}, nil
}

func (s *Service) sendNotification(ctx context.Context, transactionID int64, to, body string) error {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_attempts (
			transaction_id, recipient, channel, body, status, retry_count, attempted_at
		) VALUES (?, ?, 'sms', ?, 'pending', 0, NOW())
	`, transactionID, to, body)
	if err != nil {
		return fmt.Errorf("failed to log notification attempt: %w", err)
	}
	attemptID, _ := result.LastInsertId()

	err = s.notifier.SendSMS(ctx, to, body)
	status := "sent"
	if err != nil {
		status = "failed"
	}

	_, _ = s.db.ExecContext(ctx, `
		UPDATE notification_attempts
		SET status = ?, provider = ?, error = ?, completed_at = NOW()
		WHERE id = ?
	`, status, s.notifier.Name(), errToString(err), attemptID)

	return err
}

func generatePIN() (string, error) {
	const digits = "0123456789"
	pin := make([]byte, 16)
	for i := range pin {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(digits))))
		if err != nil {
			return "", err
		}
		pin[i] = digits[n.Int64()]
	}
	return string(pin), nil
}

func formatDuration(seconds int) string {
	d := time.Duration(seconds) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", seconds)
	}
	if d < time.Hour {
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func errToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Service) resolveOrganizationID(ctx context.Context, req FulfillRequest) (sql.NullInt64, error) {
	if req.OrganizationID > 0 {
		return sql.NullInt64{Int64: req.OrganizationID, Valid: true}, nil
	}
	if req.NasIPAddress != "" {
		var orgID sql.NullInt64
		err := s.db.QueryRowContext(ctx, `
			SELECT organization_id FROM authentication_nasdevice
			WHERE nas_ip_address = ? AND is_active = TRUE LIMIT 1
		`, req.NasIPAddress).Scan(&orgID)
		if err == nil && orgID.Valid {
			return orgID, nil
		}
	}
	if req.NasIdentifier != "" {
		var orgID sql.NullInt64
		err := s.db.QueryRowContext(ctx, `
			SELECT organization_id FROM authentication_nasdevice
			WHERE nas_identifier = ? AND is_active = TRUE LIMIT 1
		`, req.NasIdentifier).Scan(&orgID)
		if err == nil && orgID.Valid {
			return orgID, nil
		}
	}
	return sql.NullInt64{}, nil
}

func (s *Service) ensureVoucherCustomer(
	ctx context.Context,
	req FulfillRequest,
	pin string,
	orgID sql.NullInt64,
) (int64, error) {
	email := fmt.Sprintf("%s@voucher.local", pin)
	var customerID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM authentication_customer WHERE customer_email = ? LIMIT 1
	`, email).Scan(&customerID)
	if err == nil {
		if orgID.Valid {
			_, _ = s.db.ExecContext(ctx, `
				UPDATE authentication_customer SET organization_id = ? WHERE id = ? AND organization_id IS NULL
			`, orgID.Int64, customerID)
		}
		return customerID, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("fulfillment: lookup voucher customer: %w", err)
	}

	var orgVal interface{}
	if orgID.Valid {
		orgVal = orgID.Int64
	}

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
			customer_address, customer_city, organization_id, referred_by_id, is_active, created_at, updated_at
		) VALUES (?, 'individual', ?, ?, ?, 'Payment voucher', 'N/A', ?, ?, TRUE, NOW(), NOW())
	`, fmt.Sprintf("voucher-%d", req.TransactionID), pin, email, req.CustomerPhone, orgVal, referredByVal)
	if err != nil {
		return 0, fmt.Errorf("fulfillment: create voucher customer: %w", err)
	}
	return res.LastInsertId()
}
