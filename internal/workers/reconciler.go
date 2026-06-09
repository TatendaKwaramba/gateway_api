// Package workers contains background reliability workers
package workers

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

const (
	// reconcileInterval is how often the reconciliation worker runs
	reconcileInterval = 24 * time.Hour // Daily
)

// Reconciler compares local transactions against gateway settlement files
type Reconciler struct {
	db       *sql.DB
	ticker   *time.Ticker
	stopChan chan struct{}
}

// NewReconciler creates a new reconciliation worker
func NewReconciler(db *sql.DB) *Reconciler {
	return &Reconciler{
		db:       db,
		stopChan: make(chan struct{}),
	}
}

// Start begins the reconciliation loop in a goroutine
func (r *Reconciler) Start() {
	r.ticker = time.NewTicker(reconcileInterval)
	go r.loop()
	slog.Info("reconciliation worker started", "interval", reconcileInterval)
}

// Stop signals the worker to stop
func (r *Reconciler) Stop() {
	close(r.stopChan)
	if r.ticker != nil {
		r.ticker.Stop()
	}
	slog.Info("reconciliation worker stopped")
}

func (r *Reconciler) loop() {
	for {
		select {
		case <-r.ticker.C:
			r.reconcileOpenPeriods()
		case <-r.stopChan:
			return
		}
	}
}

// reconcileOpenPeriods checks all open settlement periods and auto-closes
// periods where the end_date has passed.
func (r *Reconciler) reconcileOpenPeriods() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Find open periods where end_date has passed
	rows, err := r.db.QueryContext(ctx, `
		SELECT sp.id, sp.organization_id, sp.start_date, sp.end_date
		FROM payments_settlementperiod sp
		WHERE sp.status = 'open' AND sp.end_date < CURDATE()
	`)
	if err != nil {
		slog.Error("reconciliation: failed to query open periods", "error", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int64
		var orgID int64
		var startDate, endDate string
		if err := rows.Scan(&id, &orgID, &startDate, &endDate); err != nil {
			slog.Error("reconciliation: failed to scan period", "error", err)
			continue
		}

		// Recompute totals from transactions
		var gross, fees, net float64
		var txCount int
		err := r.db.QueryRowContext(ctx, `
			SELECT
				COALESCE(SUM(pt.amount), 0),
				COALESCE(SUM(pt.fee_amount), 0),
				COALESCE(SUM(pt.net_amount), 0),
				COUNT(*)
			FROM payments_paymenttransaction pt
			WHERE pt.organization_id = ?
			  AND pt.state = 'completed'
			  AND DATE(pt.created_at) BETWEEN ? AND ?
		`, orgID, startDate, endDate).Scan(&gross, &fees, &net, &txCount)
		if err != nil {
			slog.Error("reconciliation: failed to compute totals",
				"period_id", id, "error", err)
			continue
		}

		// Update the settlement period with computed totals and close it
		_, err = r.db.ExecContext(ctx, `
			UPDATE payments_settlementperiod
			SET gross_revenue = ?, fee_amount = ?, net_revenue = ?,
			    transaction_count = ?, status = 'closed', updated_at = NOW()
			WHERE id = ?
		`, gross, fees, net, txCount, id)
		if err != nil {
			slog.Error("reconciliation: failed to close period",
				"period_id", id, "error", err)
			continue
		}

		count++
		slog.Info("reconciliation: closed settlement period",
			"period_id", id,
			"org_id", orgID,
			"period", startDate+"–"+endDate,
			"gross", gross,
			"fees", fees,
			"net", net,
			"transactions", txCount,
		)
	}

	if count > 0 {
		slog.Info("reconciliation: completed", "periods_closed", count)
	}
}

// RunManualReconciliation runs a one-off reconciliation for a specific period
func (r *Reconciler) RunManualReconciliation(ctx context.Context, gatewayID int64, orgID *int64, startDate, endDate string) (map[string]interface{}, error) {
	// Compute local totals
	var localTotal float64
	var txCount int
	query := `
		SELECT COALESCE(SUM(pt.amount), 0), COUNT(*)
		FROM payments_paymenttransaction pt
		WHERE pt.gateway_id = ?
		  AND pt.state = 'completed'
		  AND DATE(pt.created_at) BETWEEN ? AND ?
	`
	params := []interface{}{gatewayID, startDate, endDate}
	if orgID != nil {
		query += ` AND pt.organization_id = ?`
		params = append(params, *orgID)
	}

	err := r.db.QueryRowContext(ctx, query, params...).Scan(&localTotal, &txCount)
	if err != nil {
		return nil, err
	}

	result := map[string]interface{}{
		"gateway_id":        gatewayID,
		"period_start":      startDate,
		"period_end":        endDate,
		"local_total":       localTotal,
		"transaction_count": txCount,
	}

	if orgID != nil {
		result["organization_id"] = *orgID
	}

	return result, nil
}
