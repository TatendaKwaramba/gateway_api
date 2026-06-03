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

// fulfillVoucher generates a PIN, writes radcheck rows, updates transaction, and notifies.
func (s *Service) fulfillVoucher(ctx context.Context, req FulfillRequest) (*FulfillResult, error) {
	// Find tariff plan by amount (convert cents to whole units for matching)
	amountWhole := req.Amount / 100
	if amountWhole < 1 {
		amountWhole = 1
	}

	var tariffPlan struct {
		Seconds       int
		DownloadSpeed int
		UploadSpeed   int
		MaxSessions   int
	}

	err := s.db.QueryRowContext(ctx, `
		SELECT seconds, download_speed, upload_speed, max_sessions
		FROM services_tariffplan
		WHERE price = ? AND is_active = TRUE
		LIMIT 1
	`, amountWhole).Scan(
		&tariffPlan.Seconds,
		&tariffPlan.DownloadSpeed,
		&tariffPlan.UploadSpeed,
		&tariffPlan.MaxSessions,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("fulfillment: no tariff plan found for price %d", amountWhole)
		}
		return nil, fmt.Errorf("fulfillment: failed to query tariff plan: %w", err)
	}

	// Generate cryptographically random 16-digit PIN
	pin, err := generatePIN()
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to generate PIN: %w", err)
	}

	// Calculate time limit expiration
	timeLimit := time.Now().Add(time.Duration(tariffPlan.Seconds) * time.Second).Add(2 * time.Hour)
	timeLimitStr := timeLimit.Format("2006-01-02 15:04:05.000000")

	// Perform fulfillment in a database transaction
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Write radcheck password entry
	_, err = tx.ExecContext(ctx, `
		INSERT INTO radcheck (username, attribute, op, value)
		VALUES (?, 'Cleartext-Password', ':=', ?)
	`, pin, pin)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to insert radcheck password: %w", err)
	}

	// Write radcheck time-limit entry
	_, err = tx.ExecContext(ctx, `
		INSERT INTO radcheck (username, attribute, op, value)
		VALUES (?, 'Time-Limit', ':=', ?)
	`, pin, timeLimitStr)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to insert radcheck time-limit: %w", err)
	}

	// Update transaction with voucher PIN and completed_at
	_, err = tx.ExecContext(ctx, `
		UPDATE payments_paymenttransaction
		SET voucher_pin = ?, completed_at = NOW(), updated_at = NOW()
		WHERE id = ?
	`, pin, req.TransactionID)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to update transaction: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("fulfillment: failed to commit transaction: %w", err)
	}

	slog.Info("fulfillment: voucher created",
		slog.Int64("transaction_id", req.TransactionID),
		slog.String("voucher_pin", pin),
		slog.Int("tariff_seconds", tariffPlan.Seconds),
	)

	// Record metric
	metrics.RecordFulfillmentSuccess("voucher")

	// Send notification
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
			// Do NOT fail fulfillment because notification failed; retry worker will pick it up
		}
	} else {
		slog.Info("fulfillment: no customer phone, skipping notification",
			slog.Int64("transaction_id", req.TransactionID),
		)
	}

	return &FulfillResult{
		Success:    true,
		VoucherPIN: pin,
	}, nil
}

// fulfillSubscription activates a subscription row (placeholder for Phase 4+)
func (s *Service) fulfillSubscription(ctx context.Context, req FulfillRequest) (*FulfillResult, error) {
	// TODO: Implement subscription activation in Phase 4
	return &FulfillResult{Success: true}, nil
}

// fulfillTopup applies a top-up to an existing subscription (placeholder for Phase 4+)
func (s *Service) fulfillTopup(ctx context.Context, req FulfillRequest) (*FulfillResult, error) {
	// TODO: Implement top-up in Phase 4
	return &FulfillResult{Success: true}, nil
}

// sendNotification sends an SMS and logs the attempt to the audit table.
func (s *Service) sendNotification(ctx context.Context, transactionID int64, to, body string) error {
	// Insert notification attempt record
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_attempts (
			transaction_id, recipient, channel, body, status, retry_count, attempted_at
		) VALUES (?, ?, 'sms', ?, 'pending', 0, NOW())
	`, transactionID, to, body)
	if err != nil {
		return fmt.Errorf("failed to log notification attempt: %w", err)
	}
	attemptID, _ := result.LastInsertId()

	// Send via provider
	err = s.notifier.SendSMS(ctx, to, body)
	status := "sent"
	if err != nil {
		status = "failed"
	}

	// Update attempt record
	_, _ = s.db.ExecContext(ctx, `
		UPDATE notification_attempts
		SET status = ?, provider = ?, error = ?, completed_at = NOW()
		WHERE id = ?
	`, status, s.notifier.Name(), errToString(err), attemptID)

	return err
}

// generatePIN creates a cryptographically secure 16-digit PIN.
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

// formatDuration converts seconds to a human-readable string.
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
