// Package workers contains background reliability workers
package workers

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/freeradius/payments-api/internal/metrics"
	"github.com/freeradius/payments-api/internal/payments"
)

const (
	// pollInterval is how often the poller scans for pending transactions
	pollInterval = 30 * time.Second

	// maxPollAge is the maximum age a transaction can be in pending state before being marked failed
	maxPollAge = 30 * time.Minute

	// fulfillmentRetryWindow is how long a captured-but-unfulfilled
	// transaction stays eligible for automatic re-fulfillment.
	fulfillmentRetryWindow = time.Hour

	// fulfillmentRetryCooldown is the minimum gap between sweep picks for the
	// same transaction (last_polled_at doubles as the marker — the gateway
	// poller never touches completed rows).
	fulfillmentRetryCooldown = 2 * time.Minute

	// fulfillmentBatch caps how many stuck transactions one tick re-drives.
	fulfillmentBatch = 10
)

// FulfillmentFunc is called when a transaction transitions to completed
type FulfillmentFunc func(transactionID int64)

// Poller scans pending transactions and queries gateway status
type Poller struct {
	db              *sql.DB
	registry        *gateways.Registry
	sm              *payments.StateMachine
	fulfillmentFunc FulfillmentFunc
	ticker          *time.Ticker
	stopChan        chan struct{}
}

// NewPoller creates a new poller worker
func NewPoller(db *sql.DB, registry *gateways.Registry, fulfillmentFunc FulfillmentFunc) *Poller {
	return &Poller{
		db:              db,
		registry:        registry,
		sm:              payments.NewStateMachine(),
		fulfillmentFunc: fulfillmentFunc,
		stopChan:        make(chan struct{}),
	}
}

// Start begins the polling loop in a goroutine
func (p *Poller) Start() {
	p.ticker = time.NewTicker(pollInterval)
	go p.loop()
	slog.Info("poller worker started", slog.Duration("interval", pollInterval))
}

// Stop halts the polling loop
func (p *Poller) Stop() {
	if p.ticker != nil {
		p.ticker.Stop()
	}
	close(p.stopChan)
}

func (p *Poller) loop() {
	for {
		select {
		case <-p.ticker.C:
			p.run(context.Background())
		case <-p.stopChan:
			return
		}
	}
}

func (p *Poller) run(ctx context.Context) {
	// Query transactions that are pending and haven't been polled in the last 30s,
	// or are older than maxPollAge
	rows, err := p.db.QueryContext(ctx, `
		SELECT t.id, t.external_reference, g.gateway_code, t.state
		FROM payments_paymenttransaction t
		JOIN payments_paymentgateway g ON g.id = t.gateway_id
		WHERE t.state IN ('initiated', 'pending')
		  AND (t.last_polled_at IS NULL OR t.last_polled_at < DATE_SUB(NOW(), INTERVAL 30 SECOND))
		  AND t.created_at > DATE_SUB(NOW(), INTERVAL ? SECOND)
	`, int64(maxPollAge.Seconds()))
	if err != nil {
		slog.Error("poller: failed to query transactions", slog.Any("error", err))
		return
	}
	defer rows.Close()

	var checked int
	for rows.Next() {
		var transactionID int64
		var externalRef sql.NullString
		var gatewayCode, currentState string
		if err := rows.Scan(&transactionID, &externalRef, &gatewayCode, &currentState); err != nil {
			slog.Error("poller: failed to scan row", slog.Any("error", err))
			continue
		}
		externalRefStr := ""
		if externalRef.Valid {
			externalRefStr = externalRef.String
		}
		checked++
		p.processTransaction(ctx, transactionID, externalRefStr, gatewayCode, currentState)
	}

	// Re-drive fulfillment for captured-but-unfulfilled transactions
	// (restart recovery: the in-process retries in payments.Service do not
	// survive a process crash or an outage longer than a few seconds).
	p.retryUnfulfilled(ctx)

	// Also mark any pending transactions older than maxPollAge as failed
	p.timeoutOldTransactions(ctx)

	metrics.RecordPollerRun(checked)
}

// retryUnfulfilled re-triggers fulfillment for recent completed transactions
// that never got their fulfillment marker (voucher_pin NULL). Safe to call
// repeatedly: Fulfill is idempotent and claim-locked per transaction.
// `fulfillment_kind` is restricted to kinds that actually mark completion
// that way — `topup` is a no-op and would otherwise be re-driven forever.
func (p *Poller) retryUnfulfilled(ctx context.Context) {
	if p.fulfillmentFunc == nil {
		return
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT id
		FROM payments_paymenttransaction
		WHERE state = 'completed'
		  AND voucher_pin IS NULL
		  AND fulfillment_kind IN ('voucher', 'subscription')
		  AND completed_at > DATE_SUB(NOW(), INTERVAL ? SECOND)
		  AND (last_polled_at IS NULL
		       OR last_polled_at < DATE_SUB(NOW(), INTERVAL ? SECOND))
		ORDER BY completed_at
		LIMIT ?
	`,
		int64(fulfillmentRetryWindow.Seconds()),
		int64(fulfillmentRetryCooldown.Seconds()),
		fulfillmentBatch,
	)
	if err != nil {
		slog.Error("poller: failed to query unfulfilled transactions", slog.Any("error", err))
		return
	}

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			slog.Error("poller: failed to scan unfulfilled transaction", slog.Any("error", err))
			continue
		}
		ids = append(ids, id)
	}
	_ = rows.Close()

	for _, id := range ids {
		// Stamp the cooldown before triggering so a failing fulfillment is
		// not re-picked on the very next tick.
		_, _ = p.db.ExecContext(ctx, `
			UPDATE payments_paymenttransaction SET last_polled_at = NOW() WHERE id = ?
		`, id)
		slog.Info("poller: retrying fulfillment for unfulfilled transaction",
			slog.Int64("transaction_id", id),
		)
		p.triggerFulfillment(id)
	}
}

func (p *Poller) processTransaction(ctx context.Context, transactionID int64, externalRef, gatewayCode, currentState string) {
	// Update last_polled_at
	_, _ = p.db.ExecContext(ctx, `
		UPDATE payments_paymenttransaction SET last_polled_at = NOW() WHERE id = ?
	`, transactionID)

	// Resolve gateway
	gateway, ok := p.registry.Resolve(gatewayCode)
	if !ok {
		slog.Warn("poller: gateway not found",
			slog.Int64("transaction_id", transactionID),
			slog.String("gateway", gatewayCode),
		)
		return
	}

	// Query gateway status
	statusResult, err := gateway.Status(ctx, externalRef)
	if err != nil {
		slog.Error("poller: gateway status check failed",
			slog.Int64("transaction_id", transactionID),
			slog.String("gateway", gatewayCode),
			slog.Any("error", err),
		)
		return
	}

	// Transition if state changed
	fromState := payments.State(currentState)
	toState := payments.State(statusResult.State)
	if toState.Valid() && toState != fromState {
		changed, err := p.sm.Transition(ctx, p.db, transactionID, fromState, toState)
		if err != nil {
			slog.Warn("poller: state transition failed",
				slog.Int64("transaction_id", transactionID),
				slog.String("from", fromState.String()),
				slog.String("to", toState.String()),
				slog.Any("error", err),
			)
			return
		}
		if changed {
			slog.Info("poller: state transitioned",
				slog.Int64("transaction_id", transactionID),
				slog.String("from", fromState.String()),
				slog.String("to", toState.String()),
			)
			if toState == payments.StateCompleted {
				metrics.RecordPaymentCompleted()
				// Trigger fulfillment for transactions that completed via polling
				p.triggerFulfillment(transactionID)
			}
			if toState == payments.StateFailed {
				metrics.RecordPaymentFailed("poller_timeout")
			}
		}
	}
}

func (p *Poller) triggerFulfillment(transactionID int64) {
	if p.fulfillmentFunc != nil {
		go p.fulfillmentFunc(transactionID)
	}
}

func (p *Poller) timeoutOldTransactions(ctx context.Context) {
	result, err := p.db.ExecContext(ctx, `
		UPDATE payments_paymenttransaction
		SET state = 'failed', status = 'failed', updated_at = NOW()
		WHERE state IN ('initiated', 'pending')
		  AND created_at <= DATE_SUB(NOW(), INTERVAL ? SECOND)
	`, int64(maxPollAge.Seconds()))
	if err != nil {
		slog.Error("poller: failed to timeout old transactions", slog.Any("error", err))
		return
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected > 0 {
		slog.Info("poller: timed out old transactions",
			slog.Int64("count", rowsAffected),
		)
		for i := int64(0); i < rowsAffected; i++ {
			metrics.RecordPaymentFailed("max_poll_age_exceeded")
		}
	}
}
