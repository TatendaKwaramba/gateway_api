// Package fulfillment handles post-payment fulfillment (vouchers, subscriptions)
package fulfillment

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/freeradius/payments-api/internal/metrics"
	"github.com/freeradius/payments-api/internal/notify"
)

// Service handles fulfillment of completed payments
type Service struct {
	db       *sql.DB
	notifier notify.Provider
}

// NewService creates a new fulfillment service
func NewService(db *sql.DB, notifier notify.Provider) *Service {
	return &Service{
		db:       db,
		notifier: notifier,
	}
}

// FulfillRequest contains the data needed to fulfill a transaction
type FulfillRequest struct {
	TransactionID    int64
	TransactionIDStr string
	Amount           int64  // minor currency units
	Currency         string
	PlanID           int64
	CustomerPhone    string
	CustomerEmail    string
	FulfillmentKind  string
}

// FulfillResult contains the result of fulfillment
type FulfillResult struct {
	Success    bool
	VoucherPIN string
	Error      string
}

// Fulfill processes a completed transaction based on its fulfillment_kind.
// It is idempotent: if the transaction already has a voucher_pin, it returns that.
func (s *Service) Fulfill(ctx context.Context, req FulfillRequest) (*FulfillResult, error) {
	start := time.Now()
	defer func() {
		metrics.RecordFulfillmentDuration(time.Since(start).Seconds())
	}()

	// Check if already fulfilled
	var existingPIN sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT voucher_pin FROM payments_paymenttransaction WHERE id = ?
	`, req.TransactionID).Scan(&existingPIN)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: failed to query transaction: %w", err)
	}
	if existingPIN.Valid && existingPIN.String != "" {
		slog.Info("fulfillment: transaction already fulfilled",
			slog.Int64("transaction_id", req.TransactionID),
		)
		return &FulfillResult{Success: true, VoucherPIN: existingPIN.String}, nil
	}

	switch req.FulfillmentKind {
	case "voucher":
		return s.fulfillVoucher(ctx, req)
	case "subscription":
		return s.fulfillSubscription(ctx, req)
	case "topup":
		return s.fulfillTopup(ctx, req)
	default:
		return nil, fmt.Errorf("fulfillment: unknown fulfillment_kind: %s", req.FulfillmentKind)
	}
}

// Rollback removes partial radcheck rows on fulfillment failure.
func (s *Service) Rollback(ctx context.Context, voucherPIN string) error {
	if voucherPIN == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM radcheck WHERE username = ?
	`, voucherPIN)
	if err != nil {
		slog.Error("fulfillment rollback: failed to delete radcheck",
			slog.String("voucher_pin", voucherPIN),
			slog.Any("error", err),
		)
		return err
	}
	slog.Info("fulfillment rollback: removed radcheck rows", slog.String("voucher_pin", voucherPIN))
	return nil
}
