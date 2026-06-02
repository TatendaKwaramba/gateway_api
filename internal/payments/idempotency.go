package payments

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// IdempotencyStore handles idempotency key storage and replay detection
type IdempotencyStore struct {
	db *sql.DB
}

// NewIdempotencyStore creates a new idempotency store
func NewIdempotencyStore(db *sql.DB) *IdempotencyStore {
	return &IdempotencyStore{db: db}
}

// CheckAndStore checks if an idempotency key exists and stores it if not.
// Returns the existing transaction ID if found, or 0 if not found.
func (s *IdempotencyStore) CheckAndStore(ctx context.Context, key string, requestHash string) (int64, bool, error) {
	if key == "" {
		return 0, false, nil // No idempotency key provided, proceed normally
	}

	// Try to insert the key. MySQL does not support RETURNING on INSERT ... ON DUPLICATE KEY UPDATE.
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO payments_idempotency_key (` + "`key`" + `, request_hash, created_at)
		VALUES (?, ?, NOW())
		ON DUPLICATE KEY UPDATE
			` + "`key` = `key`" + `
	`, key, requestHash)

	if err != nil {
		return 0, false, fmt.Errorf("idempotency insert failed: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 1 {
		// New key inserted - not a replay
		return 0, false, nil
	}

	// Duplicate key - look up the existing transaction to verify hash matches
	var existingID int64
	var storedHash string
	err = s.db.QueryRowContext(ctx, `
		SELECT t.id, k.request_hash
		FROM payments_idempotency_key k
		LEFT JOIN payments_paymenttransaction t ON t.idempotency_key = k.`+"`key`"+`
		WHERE k.`+"`key`"+` = ?
	`, key).Scan(&existingID, &storedHash)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("idempotency key not found after duplicate detection")
		}
		return 0, false, fmt.Errorf("idempotency lookup failed: %w", err)
	}

	if storedHash != requestHash {
		return 0, false, fmt.Errorf("idempotency key conflict: same key with different request")
	}

	slog.Info("idempotency key replay detected",
		slog.String("key", key),
		slog.Int64("transaction_id", existingID),
	)
	return existingID, true, nil
}

// GetExistingTransaction returns an existing transaction for an idempotency key
func (s *IdempotencyStore) GetExistingTransaction(ctx context.Context, key string) (int64, error) {
	if key == "" {
		return 0, errors.New("empty idempotency key")
	}
	
	var transactionID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id 
		FROM payments_paymenttransaction 
		WHERE idempotency_key = ?
	`, key).Scan(&transactionID)
	
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("no transaction found for idempotency key: %s", key)
		}
		return 0, fmt.Errorf("failed to query idempotency key: %w", err)
	}
	
	return transactionID, nil
}

// ComputeRequestHash creates a hash of the request for idempotency comparison
func ComputeRequestHash(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// WebhookReplayStore handles webhook replay detection
type WebhookReplayStore struct {
	db *sql.DB
}

// NewWebhookReplayStore creates a new webhook replay store
func NewWebhookReplayStore(db *sql.DB) *WebhookReplayStore {
	return &WebhookReplayStore{db: db}
}

// WebhookEventKey uniquely identifies a webhook event for replay detection
type WebhookEventKey struct {
	GatewayCode       string
	ExternalReference string
	EventType         string
}

// String returns a string representation for hashing
func (k WebhookEventKey) String() string {
	return fmt.Sprintf("%s:%s:%s", k.GatewayCode, k.ExternalReference, k.EventType)
}

// CheckAndLog checks if a webhook event is a replay and logs it
// Returns true if this is a replay (already processed), false otherwise
func (s *WebhookReplayStore) CheckAndLog(ctx context.Context, key WebhookEventKey, rawBody []byte, headers map[string][]string) (bool, error) {
	eventKey := key.String()
	eventHash := ComputeRequestHash(rawBody)
	
	// Check if we've seen this exact event before
	var existingID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM payments_webhook_replay_log
		WHERE event_key = ? AND event_hash = ?
	`, eventKey, eventHash).Scan(&existingID)
	
	if err == nil {
		slog.Info("webhook replay detected",
			slog.String("event_key", eventKey),
			slog.Int64("existing_id", existingID),
		)
		return true, nil
	}
	
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("failed to check webhook replay: %w", err)
	}
	
	// Not a replay - log it
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO payments_webhook_replay_log (event_key, event_hash, received_at)
		VALUES (?, ?, NOW())
	`, eventKey, eventHash)
	
	if err != nil {
		return false, fmt.Errorf("failed to log webhook event: %w", err)
	}
	
	return false, nil
}

// CleanupOldKeys removes idempotency keys older than the retention period
func (s *IdempotencyStore) CleanupOldKeys(ctx context.Context, retention time.Duration) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM payments_idempotency_key
		WHERE created_at < DATE_SUB(NOW(), INTERVAL ? SECOND)
	`, int64(retention.Seconds()))
	
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup idempotency keys: %w", err)
	}
	
	rows, _ := result.RowsAffected()
	return rows, nil
}

// CleanupOldWebhooks removes old webhook replay logs
func (s *WebhookReplayStore) CleanupOldWebhooks(ctx context.Context, retention time.Duration) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM payments_webhook_replay_log
		WHERE received_at < DATE_SUB(NOW(), INTERVAL ? SECOND)
	`, int64(retention.Seconds()))
	
	if err != nil {
		return 0, fmt.Errorf("failed to cleanup webhook logs: %w", err)
	}
	
	rows, _ := result.RowsAffected()
	return rows, nil
}
