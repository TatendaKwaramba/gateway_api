package identity

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Identity revocation registry (DDL owned by Django migration
// payments/0008). A row invalidates tokens issued BEFORE revoked_at for the
// contact point: the customer must re-verify (OTP / Google SSO) to mint a
// fresh token, so self-service recovery still works.

// IdentityRevoked reports whether any revocation row invalidates tokens
// issued at issuedAt for the identity's contact points. Legacy tokens with
// unknown iat are treated as issued before every revocation (fail-closed).
func (s *Service) IdentityRevoked(ctx context.Context, ident Identity, issuedAt int64) (bool, error) {
	if s.db == nil || (ident.Phone == "" && ident.Email == "") {
		return false, nil
	}

	clauses := []string{}
	args := []interface{}{}
	suffix := ""
	if ident.Phone != "" {
		clauses = append(clauses, "phone = ?")
		args = append(args, strings.TrimSpace(ident.Phone))
	}
	if ident.Email != "" {
		clauses = append(clauses, "email = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(ident.Email)))
	}
	if len(args) == 0 {
		return false, nil
	}

	// Legacy M4 tokens carry no iat: ANY revocation of the contact point
	// invalidates them (fail-closed). New tokens window by issued-at.
	if issuedAt > 0 {
		args = append(args, issuedAt)
		suffix = " AND revoked_at > FROM_UNIXTIME(?)"
	}

	// #nosec G201 -- clauses/suffix are constant fragments; contact points bound via args.
	query := fmt.Sprintf(
		"SELECT COUNT(*) FROM payments_identity_revocation WHERE (%s)%s",
		strings.Join(clauses, " OR "), suffix,
	)

	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return false, fmt.Errorf("identity: revocation query: %w", err)
	}
	return count > 0, nil
}

// RevokeIdentity records a revocation. Takes effect for tokens issued before
// now; empty contact points is a caller error.
func (s *Service) RevokeIdentity(ctx context.Context, phone, email, reason, revokedBy string) error {
	phone = strings.TrimSpace(phone)
	email = strings.ToLower(strings.TrimSpace(email))
	if phone == "" && email == "" {
		return fmt.Errorf("identity: phone or email required for revocation")
	}
	var phoneVal, emailVal interface{}
	if phone != "" {
		phoneVal = phone
	}
	if email != "" {
		emailVal = email
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO payments_identity_revocation (phone, email, reason, revoked_by, revoked_at)
		VALUES (?, ?, ?, ?, NOW(3))
	`, phoneVal, emailVal, strings.TrimSpace(reason), strings.TrimSpace(revokedBy)); err != nil {
		return fmt.Errorf("identity: revoke insert: %w", err)
	}
	slog.Info("identity: contact point revoked",
		slog.String(pointPhone, maskRecipient(phone)),
		slog.String(pointEmail, maskRecipient(email)),
		slog.String("reason", strings.TrimSpace(reason)),
	)
	return nil
}
