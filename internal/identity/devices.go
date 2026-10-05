package identity

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// UpsertPushDevice registers or refreshes a mobile push device binding.
// The expo_token is the natural key: re-registration updates the identity
// binding and re-enables the device.
func (s *Service) UpsertPushDevice(ctx context.Context, ident Identity, expoToken, platform string) error {
	if s.db == nil {
		return fmt.Errorf("identity: no database configured")
	}

	phone := ident.Phone
	if phone != "" {
		phone = NormalizePhone(phone)
	}
	email := strings.ToLower(strings.TrimSpace(ident.Email))

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO payments_pushdevice (
			expo_token, platform, identity_phone, identity_email, enabled, last_seen_at, created_at, updated_at
		) VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), TRUE, NOW(), NOW(), NOW())
		ON DUPLICATE KEY UPDATE
			platform = VALUES(platform),
			identity_phone = VALUES(identity_phone),
			identity_email = VALUES(identity_email),
			enabled = TRUE,
			last_seen_at = NOW(),
			updated_at = NOW()
	`, expoToken, platform, phone, email)
	if err != nil {
		return fmt.Errorf("identity: upsert push device: %w", err)
	}
	return nil
}

// DisablePushDevice marks a device token as no longer valid (best-effort)
func (s *Service) DisablePushDevice(ctx context.Context, expoToken string) {
	if s.db == nil {
		return
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE payments_pushdevice SET enabled = FALSE, updated_at = NOW() WHERE expo_token = ?
	`, expoToken); err != nil {
		slog.Warn("identity: failed to disable push device",
			slog.Any("error", err),
		)
	}
}

// EnabledTokensForPhone returns active push tokens bound to a phone number.
// Matching is exact or last-9-digit suffix (bridges local vs E.164 formats,
// e.g. 0771234567 vs +263771234567).
func (s *Service) EnabledTokensForPhone(ctx context.Context, phone string) ([]string, error) {
	if s.db == nil {
		return nil, fmt.Errorf("identity: no database configured")
	}
	normalized := NormalizePhone(phone)
	if normalized == "" {
		return nil, nil
	}
	suffix := normalized
	if len(suffix) > 9 {
		suffix = suffix[len(suffix)-9:]
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT expo_token FROM payments_pushdevice
		WHERE enabled = TRUE AND identity_phone IS NOT NULL
		  AND (identity_phone = ? OR RIGHT(identity_phone, 9) = ?)
	`, normalized, suffix)
	if err != nil {
		return nil, fmt.Errorf("identity: query push devices: %w", err)
	}
	defer rows.Close()

	var tokens []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, fmt.Errorf("identity: scan push device: %w", err)
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// CountEnabledDevices returns how many active push devices are bound to the
// identity (phone exact/last-9 or email). Used by GET /api/v1/identity/me.
func (s *Service) CountEnabledDevices(ctx context.Context, ident Identity) int {
	if s.db == nil {
		return 0
	}
	if ident.Phone == "" && ident.Email == "" {
		return 0
	}
	var count int
	argOrder := []string{}
	suffix := ""
	if ident.Phone != "" {
		normalized := NormalizePhone(ident.Phone)
		if len(normalized) > 9 {
			suffix = normalized[len(normalized)-9:]
		} else {
			suffix = normalized
		}
	}
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM payments_pushdevice
		WHERE enabled = TRUE AND (
			(? != '' AND (identity_phone = ? OR RIGHT(identity_phone, 9) = ?))
			OR (? != '' AND identity_email = ?)
		)
	`, ident.Phone, ident.Phone, suffix, strings.ToLower(ident.Email), strings.ToLower(ident.Email)).Scan(&count)
	if err != nil {
		return 0
	}
	_ = argOrder
	return count
}
