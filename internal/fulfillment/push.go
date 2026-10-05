package fulfillment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/freeradius/payments-api/internal/notify"
)

// PushNotificationRequest is a best-effort server-initiated push (e.g. FUP
// threshold warnings emitted by flash-api). Not tied to a transaction.
type PushNotificationRequest struct {
	Phone string
	Title string
	Body  string
	Data  map[string]string
}

// PushNotification delivers a server-initiated push to every active device
// bound to the customer phone. Best-effort: failures are audited in
// notification_attempts (channel='push', transaction_id=0) and logged, never
// fatal. No-op when push or identity support is not configured.
func (s *Service) PushNotification(ctx context.Context, req PushNotificationRequest) error {
	req.Phone = strings.TrimSpace(req.Phone)
	req.Title = strings.TrimSpace(req.Title)
	req.Body = strings.TrimSpace(req.Body)
	if req.Phone == "" || req.Title == "" || req.Body == "" {
		return errors.New("fulfillment: phone, title and body are required")
	}
	if s.push == nil || s.identity == nil {
		return errors.New("fulfillment: push not configured")
	}

	tokens, err := s.identity.EnabledTokensForPhone(ctx, req.Phone)
	if err != nil {
		return fmt.Errorf("fulfillment: push device lookup failed: %w", err)
	}
	if len(tokens) == 0 {
		return fmt.Errorf("fulfillment: no registered push devices for %s", req.Phone)
	}

	var lastErr error
	for _, token := range tokens {
		attemptID, insertErr := s.insertNotificationAttempt(ctx, 0, token, "push", req.Body)
		if insertErr != nil {
			slog.Error("fulfillment: push attempt audit failed",
				slog.Any("error", insertErr),
			)
			continue
		}

		sendErr := s.push.SendPush(ctx, token, req.Title, req.Body, req.Data)

		status := "sent"
		if sendErr != nil {
			status = "failed"
			if errors.Is(sendErr, notify.ErrDeviceNotRegistered) {
				s.identity.DisablePushDevice(ctx, token)
			}
		}
		s.completeNotificationAttempt(ctx, attemptID, s.push.Name(), status, sendErr)

		if sendErr != nil {
			slog.Error("fulfillment: push delivery failed",
				slog.Any("error", sendErr),
			)
			lastErr = sendErr
		}
	}
	return lastErr
}

// sendPushForPhone delivers a mobile push notification to every active device
// bound to the customer phone. Best-effort: failures are audited in
// notification_attempts (channel='push') and logged, never fatal to
// fulfillment. No-op when push or identity support is not configured.
func (s *Service) sendPushForPhone(ctx context.Context, transactionID int64, phone, title, body string) {
	if s.push == nil || s.identity == nil || phone == "" {
		return
	}

	tokens, err := s.identity.EnabledTokensForPhone(ctx, phone)
	if err != nil {
		slog.Error("fulfillment: push device lookup failed",
			slog.Int64("transaction_id", transactionID),
			slog.Any("error", err),
		)
		return
	}

	for _, token := range tokens {
		attemptID, insertErr := s.insertNotificationAttempt(ctx, transactionID, token, "push", body)
		if insertErr != nil {
			slog.Error("fulfillment: push attempt audit failed",
				slog.Int64("transaction_id", transactionID),
				slog.Any("error", insertErr),
			)
			continue
		}

		sendErr := s.push.SendPush(ctx, token, title, body, map[string]string{
			"transaction_id": fmt.Sprintf("%d", transactionID),
			"kind":           "fulfillment",
		})

		status := "sent"
		if sendErr != nil {
			status = "failed"
			if errors.Is(sendErr, notify.ErrDeviceNotRegistered) {
				s.identity.DisablePushDevice(ctx, token)
			}
		}
		s.completeNotificationAttempt(ctx, attemptID, s.push.Name(), status, sendErr)

		if sendErr != nil {
			slog.Error("fulfillment: push delivery failed",
				slog.Int64("transaction_id", transactionID),
				slog.Any("error", sendErr),
			)
		}
	}
}

// insertNotificationAttempt writes a pending notification_attempts audit row
// and returns its ID
func (s *Service) insertNotificationAttempt(ctx context.Context, transactionID int64, recipient, channel, body string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_attempts (
			transaction_id, recipient, channel, body, status, retry_count, attempted_at
		) VALUES (?, ?, ?, ?, 'pending', 0, NOW())
	`, transactionID, recipient, channel, body)
	if err != nil {
		return 0, fmt.Errorf("failed to log notification attempt: %w", err)
	}
	return res.LastInsertId()
}

// completeNotificationAttempt marks an audit row sent/failed
func (s *Service) completeNotificationAttempt(ctx context.Context, attemptID int64, provider, status string, err error) {
	_, _ = s.db.ExecContext(ctx, `
		UPDATE notification_attempts
		SET status = ?, provider = ?, error = ?, completed_at = NOW()
		WHERE id = ?
	`, status, provider, errToString(err), attemptID)
}
