package fulfillment

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
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

	switch {
	case req.PlanID > 0:
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
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("fulfillment: tariff plan %d not found", req.PlanID)
			}
			return nil, fmt.Errorf("fulfillment: failed to query tariff plan: %w", err)
		}
	case req.DurationDays > 0:
		// Dynamic hotspot pricing — no static plan matches the computed price.
		// Use the first available active plan for speed/session defaults, then
		// override seconds below with duration_days.
		err := s.db.QueryRowContext(ctx, `
			SELECT id, seconds, download_speed, upload_speed, max_sessions, price
			FROM services_tariffplan
			WHERE is_active = TRUE
			LIMIT 1
		`).Scan(
			&tariffPlan.ID,
			&tariffPlan.Seconds,
			&tariffPlan.DownloadSpeed,
			&tariffPlan.UploadSpeed,
			&tariffPlan.MaxSessions,
			&tariffPlan.Price,
		)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// No plans at all — use sensible defaults for a hotspot voucher.
				tariffPlan = struct {
					ID            int64
					Seconds       int
					DownloadSpeed int
					UploadSpeed   int
					MaxSessions   int
					Price         int64
				}{ID: 0, Seconds: 86400, DownloadSpeed: 3000, UploadSpeed: 1500, MaxSessions: 2, Price: req.Amount}
			} else {
				return nil, fmt.Errorf("fulfillment: failed to query tariff plan: %w", err)
			}
		}
	default:
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
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("fulfillment: no tariff plan found for price %d (minor units)", req.Amount)
			}
			return nil, fmt.Errorf("fulfillment: failed to query tariff plan: %w", err)
		}
	}

	// When duration_days is provided (dynamic hotspot pricing), override the
	// plan's seconds with the requested duration.
	if req.DurationDays > 0 {
		tariffPlan.Seconds = req.DurationDays * 86400
	}

	pin, err := generatePIN()
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to generate PIN: %w", err)
	}

	orgID := s.resolveOrganizationID(ctx, req)

	contactCustomerID, err := s.EnsureBuyerCustomer(ctx, req, orgID)
	if err != nil {
		return nil, err
	}
	// Voucher owner: the checkout contact wins (typed SMS/PIN recipient),
	// then the identity-stamped owner; both empty = anonymous voucher.
	voucherCustomerID := interface{}(nil)
	if contactCustomerID > 0 {
		voucherCustomerID = contactCustomerID
	} else if req.OwnerCustomerID > 0 {
		voucherCustomerID = req.OwnerCustomerID
	}
	// Transaction owner: the identity customer first (M-CIM — purchases of a
	// logged-in buyer stay in their history), else the contact customer.
	txCustomerID := interface{}(nil)
	if req.OwnerCustomerID > 0 {
		txCustomerID = req.OwnerCustomerID
	} else if contactCustomerID > 0 {
		txCustomerID = contactCustomerID
	}

	timeLimit := time.Now().Add(time.Duration(tariffPlan.Seconds) * time.Second).Add(2 * time.Hour)
	timeLimitStr := timeLimit.Format("2006-01-02 15:04:05.000000")

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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
		{"Reply-Message", "Cached by Amani Ecosystems"},
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

	var tariffPlanID interface{} = tariffPlan.ID
	if tariffPlan.ID == 0 {
		tariffPlanID = nil
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO vouchers_voucher (
			radcheck_id, tariff_plan_id, customer_id, voucher_amount, voucher_serial_number,
			voucher_pin, voucher_expired_date, voucher_response_message, voucher_status,
			payment_transaction_id, created_by_id, updated_by_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, 1, 1, NOW(), NOW())
	`, radcheckID, tariffPlanID, voucherCustomerID, tariffPlan.Price, pin, pin, timeLimit, "Payment voucher", req.TransactionID)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to insert voucher: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE payments_paymenttransaction
		SET voucher_pin = ?, customer_id = COALESCE(?, customer_id), completed_at = NOW(), updated_at = NOW()
		WHERE id = ?
	`, pin, txCustomerID, req.TransactionID)
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
		s.sendPushForPhone(ctx, req.TransactionID, req.CustomerPhone, "Voucher ready", msgBody)
	}

	return &FulfillResult{
		Success:    true,
		VoucherPIN: pin,
	}, nil
}

//nolint:unparam // ctx is part of the fulfillment dispatcher signature.
func (s *Service) fulfillTopup(ctx context.Context, req FulfillRequest) (*FulfillResult, error) {
	return &FulfillResult{Success: true}, nil
}

func (s *Service) sendNotification(ctx context.Context, transactionID int64, to, body string) error {
	attemptID, err := s.insertNotificationAttempt(ctx, transactionID, to, "sms", body)
	if err != nil {
		return err
	}

	err = s.notifier.SendSMS(ctx, to, body)
	status := "sent"
	if err != nil {
		status = "failed"
	}

	s.completeNotificationAttempt(ctx, attemptID, s.notifier.Name(), status, err)
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

func (s *Service) resolveOrganizationID(ctx context.Context, req FulfillRequest) sql.NullInt64 {
	if req.OrganizationID > 0 {
		return sql.NullInt64{Int64: req.OrganizationID, Valid: true}
	}
	if req.NasIPAddress != "" {
		var orgID sql.NullInt64
		err := s.db.QueryRowContext(ctx, `
			SELECT organization_id FROM authentication_nasdevice
			WHERE nas_ip_address = ? AND is_active = TRUE LIMIT 1
		`, req.NasIPAddress).Scan(&orgID)
		if err == nil && orgID.Valid {
			return orgID
		}
	}
	if req.NasIdentifier != "" {
		var orgID sql.NullInt64
		err := s.db.QueryRowContext(ctx, `
			SELECT organization_id FROM authentication_nasdevice
			WHERE nas_identifier = ? AND is_active = TRUE LIMIT 1
		`, req.NasIdentifier).Scan(&orgID)
		if err == nil && orgID.Valid {
			return orgID
		}
	}
	return sql.NullInt64{}
}
