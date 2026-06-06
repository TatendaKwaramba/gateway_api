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
		SET voucher_pin = ?, completed_at = NOW(), updated_at = NOW()
		WHERE id = ?
	`, pin, req.TransactionID)
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
