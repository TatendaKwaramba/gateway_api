// Package fulfillment handles post-payment fulfillment (vouchers, subscriptions)
package fulfillment

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/freeradius/payments-api/internal/identity"
	"github.com/freeradius/payments-api/internal/metrics"
	"github.com/freeradius/payments-api/internal/notify"
)

// Service handles fulfillment of completed payments
type Service struct {
	db       *sql.DB
	notifier notify.Provider
	push     notify.PushSender
	identity *identity.Service
}

// NewService creates a new fulfillment service
func NewService(db *sql.DB, notifier notify.Provider) *Service {
	return &Service{
		db:       db,
		notifier: notifier,
	}
}

// SetPushSender attaches optional mobile push support. Requires an identity
// service (device registry); push is skipped when either is absent.
func (s *Service) SetPushSender(push notify.PushSender, identityService *identity.Service) {
	s.push = push
	s.identity = identityService
}

// FulfillRequest contains the data needed to fulfill a transaction
type FulfillRequest struct {
	TransactionID    int64
	TransactionIDStr string
	Amount           int64 // minor currency units
	Currency         string
	PlanID           int64
	SubscriptionID   int64
	DurationDays     int
	CustomerPhone    string
	CustomerEmail    string
	FulfillmentKind  string
	OrganizationID   int64
	NasIPAddress     string
	NasIdentifier    string
	ReferralCode     string
	// OwnerCustomerID is the identity-stamped owner captured at initiate
	// (M-CIM). Used when the checkout captured no contact, so a logged-in
	// purchase never ends up on an ownerless row.
	OwnerCustomerID int64
}

// FulfillResult contains the result of fulfillment
type FulfillResult struct {
	Success    bool
	VoucherPIN string
	Error      string
}

// Fulfill processes a completed transaction based on its fulfillment_kind.
// It is idempotent: if the transaction already has a voucher_pin, it returns that.
//
// A per-transaction MySQL advisory lock serializes concurrent triggers
// (webhook, poller and admin re-trigger can all fire for one row). Without it
// two callers can both pass the "already fulfilled" check and issue two
// vouchers for a single captured payment.
func (s *Service) Fulfill(ctx context.Context, req FulfillRequest) (*FulfillResult, error) {
	start := time.Now()
	defer func() {
		metrics.RecordFulfillmentDuration(time.Since(start).Seconds())
	}()

	release, err := s.claim(ctx, req.TransactionID)
	if err != nil {
		return nil, err
	}
	defer release()

	// Check if already fulfilled (under the claim — a concurrent trigger
	// cannot be mid-write while we read).
	var existingPIN sql.NullString
	err = s.db.QueryRowContext(ctx, `
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
	case "subscription_adjustment":
		return s.fulfillSubscriptionAdjustment(ctx, req)
	case "topup":
		return s.fulfillTopup(ctx, req)
	default:
		return nil, fmt.Errorf("fulfillment: unknown fulfillment_kind: %s", req.FulfillmentKind)
	}
}

// claim acquires the per-transaction fulfillment lock and returns the release
// function. GET_LOCK is scoped to a single MySQL connection, so the lock and
// its release run on one dedicated *sql.Conn.
const (
	claimLockWaitSeconds = 15
	claimLockPrefix      = "payments:fulfill:"
)

func (s *Service) claim(ctx context.Context, transactionID int64) (func(), error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("fulfillment: claim connection: %w", err)
	}
	name := fmt.Sprintf("%s%d", claimLockPrefix, transactionID)

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, claimLockWaitSeconds).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("fulfillment: claim lock: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		_ = conn.Close()
		return nil, fmt.Errorf("fulfillment: transaction %d is already being fulfilled", transactionID)
	}

	return func() {
		// Release even when the caller's context is already cancelled.
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK(?)", name); err != nil {
			slog.Error("fulfillment: failed to release claim",
				slog.Int64("transaction_id", transactionID),
				slog.Any("error", err),
			)
		}
		_ = conn.Close()
	}, nil
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
