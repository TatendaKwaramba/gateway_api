package workers

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/freeradius/payments-api/internal/metrics"
	"github.com/freeradius/payments-api/internal/notify"
)

const (
	// retryInterval is how often the notification retry worker runs
	retryInterval = 60 * time.Second

	// maxNotificationRetries is the maximum number of retry attempts
	maxNotificationRetries = 5

	// notificationRetryBaseDelay is the base delay for exponential backoff
	notificationRetryBaseDelay = 5 * time.Minute
)

// NotificationRetry drains failed notification attempts and retries them
type NotificationRetry struct {
	db       *sql.DB
	notifier notify.Provider
	ticker   *time.Ticker
	stopChan chan struct{}
}

// NewNotificationRetry creates a new notification retry worker
func NewNotificationRetry(db *sql.DB, notifier notify.Provider) *NotificationRetry {
	return &NotificationRetry{
		db:       db,
		notifier: notifier,
		stopChan: make(chan struct{}),
	}
}

// Start begins the retry loop in a goroutine
func (nr *NotificationRetry) Start() {
	nr.ticker = time.NewTicker(retryInterval)
	go nr.loop()
	slog.Info("notification retry worker started", slog.Duration("interval", retryInterval))
}

// Stop halts the retry loop
func (nr *NotificationRetry) Stop() {
	if nr.ticker != nil {
		nr.ticker.Stop()
	}
	close(nr.stopChan)
}

func (nr *NotificationRetry) loop() {
	for {
		select {
		case <-nr.ticker.C:
			nr.run(context.Background())
		case <-nr.stopChan:
			return
		}
	}
}

func (nr *NotificationRetry) run(ctx context.Context) {
	// Find failed notifications that are due for retry
	rows, err := nr.db.QueryContext(ctx, `
		SELECT id, transaction_id, recipient, body, retry_count
		FROM notification_attempts
		WHERE status = 'failed'
		  AND retry_count < ?
		  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		ORDER BY attempted_at ASC
		LIMIT 100
	`, maxNotificationRetries)
	if err != nil {
		slog.Error("notification retry: failed to query", slog.Any("error", err))
		return
	}
	defer rows.Close()

	var retried int
	for rows.Next() {
		var id int64
		var transactionID int64
		var recipient, body string
		var retryCount int
		if err := rows.Scan(&id, &transactionID, &recipient, &body, &retryCount); err != nil {
			slog.Error("notification retry: scan error", slog.Any("error", err))
			continue
		}

		err := nr.notifier.SendSMS(ctx, recipient, body)
		status := "sent"
		if err != nil {
			status = "failed"
		}

		// Update record
		nextRetry := time.Now().Add(time.Duration(retryCount+1) * notificationRetryBaseDelay)
		_, updateErr := nr.db.ExecContext(ctx, `
			UPDATE notification_attempts
			SET status = ?, provider = ?, error = ?, retry_count = retry_count + 1,
			    next_retry_at = ?, completed_at = CASE WHEN ? = 'sent' THEN NOW() ELSE NULL END,
			    updated_at = NOW()
			WHERE id = ?
		`, status, nr.notifier.Name(), errToStr(err), nextRetry, status, id)
		if updateErr != nil {
			slog.Error("notification retry: failed to update record",
				slog.Int64("id", id),
				slog.Any("error", updateErr),
			)
		}

		metrics.RecordNotificationAttempt(nr.notifier.Name(), status)
		retried++
	}

	if retried > 0 {
		slog.Info("notification retry: processed", slog.Int("count", retried))
	}
}

func errToStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
