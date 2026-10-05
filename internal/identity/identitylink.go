package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Identitylink maintenance (M-CIM): every successful identity verification
// (OTP, Google, password login) registers the verified contact into
// authentication_identitylink. Sign-up creates the customer owner row when no
// customer carries the contact yet — the user's expectation: "sign up creates
// a customer record" — while purchases still create/claim rows through the
// fulfillment buyer path (retro-claim).

// EnsureSelfCustomer resolves the customer owned by a verified identity,
// creating a minimal owner row when none exists yet (M-CIM sign-up).
// Returns the already-claimed customer id when a link/legacy match wins.
func (s *Service) EnsureSelfCustomer(ctx context.Context, ident Identity) (int64, error) {
	if s.db == nil {
		return 0, ErrPasswordNoOwner
	}
	if ids := s.LinkedCustomerIDs(ctx, ident); len(ids) > 0 {
		return ids[0], nil
	}

	digits := NormalizePhoneDigits(ident.Phone)
	email := strings.ToLower(strings.TrimSpace(ident.Email))
	kind := pointPhone
	if digits == "" {
		if email == "" {
			return 0, ErrPasswordNoOwner
		}
		kind = pointEmail
	}
	customerCode := selfCustomerCode(kind, email, digits)
	customerName := digits
	if customerName == "" {
		customerName = email
	}
	customerEmail := email
	if customerEmail == "" {
		customerEmail = digits + "@app.identity"
	}
	customerPhone := digits

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO authentication_customer (
			customer_id, customer_type, customer_name, customer_email, customer_phone,
			customer_address, customer_city, organization_id, radius_username,
			access_type, is_active, created_at, updated_at
		) VALUES (?, 'subscriber', ?, ?, ?, 'App sign-up', 'N/A', ?, ?, 'hotspot', TRUE, NOW(), NOW())
	`, customerCode, customerName, customerEmail, customerPhone,
		s.defaultOrgID(ctx), customerCode)
	if err != nil {
		// Race/reuse: another request may have inserted the same derived code
		// (same verified contact) — adopt that row instead of failing.
		var existing int64
		if lookupErr := s.db.QueryRowContext(ctx,
			"SELECT id FROM authentication_customer WHERE customer_id = ?", customerCode,
		).Scan(&existing); lookupErr == nil && existing > 0 {
			return existing, nil
		}
		return 0, fmt.Errorf("identity: self-signup customer create: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("identity: self-signup customer id: %w", err)
	}
	slog.Info("identity: sign-up customer created",
		slog.Int64("customer_id", id),
		slog.String("customer_id_code", customerCode),
		slog.String("kind", kind),
	)
	return id, nil
}

// selfCustomerCode derives the unique (customer_id, radius_username) key:
// phone identities use the digit run, email identities a sanitized local part
// plus a hash of the full address (both stay unique per contact).
func selfCustomerCode(kind, email, digits string) string {
	if kind == pointPhone {
		return "app-" + digits
	}
	sum := sha256.Sum256([]byte(email))
	return fmt.Sprintf("app-%s-%x", emailLocalPart(email), sum[:3])
}

// emailLocalPart keeps the address local part short and identifier-safe.
func emailLocalPart(email string) string {
	local := email
	if i := strings.LastIndex(email, "@"); i > 0 {
		local = email[:i]
	}
	var b strings.Builder
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= 20 {
			break
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == "" {
		return "user"
	}
	return out
}

// defaultOrgID picks the organization for a sign-up customer: the configured
// DEFAULT_ORG_ID when set, else the deployment's single organization when
// unambiguous, else NULL (profile shows no org name; multi-tenant attribution
// arrives with the customer's first purchase).
func (s *Service) defaultOrgID(ctx context.Context) interface{} {
	if s.signupOrgID > 0 {
		return s.signupOrgID
	}
	var count int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM authentication_organization",
	).Scan(&count); err != nil || count != 1 {
		return nil
	}
	var orgID int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT id FROM authentication_organization ORDER BY id ASC LIMIT 1",
	).Scan(&orgID); err != nil {
		return nil
	}
	return orgID
}

// LinkedCustomerIDs resolves a verified identity to its owning customer IDs
// (identitylink exact + last-9 digits bridge; falls back to raw columns).
func (s *Service) LinkedCustomerIDs(ctx context.Context, ident Identity) []int64 {
	if s.db == nil || ident.IsEmpty() {
		return nil
	}

	var phoneDigits string
	if ident.Phone != "" {
		phoneDigits = NormalizePhoneDigits(ident.Phone)
	}
	email := strings.ToLower(strings.TrimSpace(ident.Email))
	suffix := ""
	if len(phoneDigits) >= 9 {
		suffix = phoneDigits[len(phoneDigits)-9:]
	}

	ids := make([]int64, 0, 2)
	seen := make(map[int64]bool)
	appendID := func(id int64) {
		if !seen[id] {
			ids = append(ids, id)
		}
		seen[id] = true
	}

	// Identitylink route (phone digits bridging + email exact).
	query := ""
	args := []interface{}{}
	if phoneDigits != "" {
		query += " OR (kind = 'phone' AND (value = ? OR value = ? OR RIGHT(value, 9) = ?))"
		args = append(args, phoneDigits, "+"+phoneDigits, suffix)
	}
	if email != "" {
		query += " OR (kind = 'email' AND value = ?)"
		args = append(args, email)
	}
	if query != "" {
		// #nosec G202 -- query is built from constant fragments; contact values bound via ? args.
		full := "SELECT DISTINCT customer_id FROM authentication_identitylink WHERE " + strings.TrimPrefix(query, " OR ")
		rows, err := s.db.QueryContext(ctx, full, args...)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				if scanErr := rows.Scan(&id); scanErr == nil {
					appendID(id)
				}
			}
		}
	}

	// Legacy column row (pre-identitylink customers).
	if email != "" {
		var id int64
		if err := s.db.QueryRowContext(ctx,
			"SELECT id FROM authentication_customer WHERE LOWER(customer_email) = ? LIMIT 1", email,
		).Scan(&id); err == nil {
			appendID(id)
		}
	} else if phoneDigits != "" && len(phoneDigits) >= 9 {
		var id int64
		if err := s.db.QueryRowContext(ctx, `
			SELECT id FROM authentication_customer
			WHERE RIGHT(REGEXP_REPLACE(customer_phone, '[^0-9]', ''), 9) = ?
			LIMIT 1
		`, suffix).Scan(&id); err == nil {
			appendID(id)
		}
	}
	return ids
}

// RegisterVerifiedContact records a successful identity verification
// (OTP / Google / password login): the identity's contacts are attached to its
// customer — creating the sign-up customer row when the contact has never
// bought before. via is the trust tier (otp/google) recorded on the link row.
func (s *Service) RegisterVerifiedContact(ctx context.Context, ident Identity, via string) {
	if s.db == nil || ident.IsEmpty() {
		return
	}
	if via == "" {
		via = ViaOtp
	}
	customerID, err := s.EnsureSelfCustomer(ctx, ident)
	if err != nil {
		slog.Debug("identity: identitylink register skipped",
			slog.Any("error", err),
		)
		return
	}
	if ident.Phone != "" {
		s.linkContact(ctx, pointPhone, ident.Phone, customerID, via)
	}
	if ident.Email != "" {
		s.linkContact(ctx, pointEmail, ident.Email, customerID, via)
	}
}

// linkContact attaches one contact to a customer at the given trust tier.
// Never steals a claim owned by another customer (supersede lives in
// AddContact) and never downgrades an existing tier on the same customer.
func (s *Service) linkContact(ctx context.Context, kind, value string, customerID int64, via string) {
	value = normalizeContact(kind, value)
	if value == "" {
		return
	}
	var (
		ownerID  int64
		ownerVia string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT customer_id, verified_via FROM authentication_identitylink
		WHERE kind = ? AND value = ?
	`, kind, value).Scan(&ownerID, &ownerVia)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO authentication_identitylink
				(customer_id, kind, value, verified_at, verified_via, created_at, updated_at)
			VALUES (?, ?, ?, NOW(), ?, NOW(), NOW())
		`, customerID, kind, value, via); err != nil {
			slog.Debug("identity: identitylink register skipped", slog.Any("error", err))
			return
		}
		slog.Info("identity: contact linked to customer (M-CIM)",
			slog.String("kind", kind),
			slog.String("value", maskContact(value)),
			slog.Int64("customer_id", customerID),
			slog.String("verified_via", via),
		)
	case err != nil:
		slog.Debug("identity: identitylink lookup failed", slog.Any("error", err))
	case ownerID == customerID && tierRank(ownerVia) < tierRank(via):
		if _, err := s.db.ExecContext(ctx, `
			UPDATE authentication_identitylink
			SET verified_via = ?, verified_at = NOW(), updated_at = NOW()
			WHERE customer_id = ? AND kind = ? AND value = ?
		`, via, customerID, kind, value); err == nil {
			slog.Info("identity: contact tier upgraded (M-CIM)",
				slog.String("kind", kind),
				slog.String("value", maskContact(value)),
				slog.String("verified_via", via),
			)
		}
	default:
		// Claimed by another customer (or already at least as trusted).
	}
}

func maskContact(value string) string {
	if len(value) <= 4 {
		return "***"
	}
	return value[:2] + "***" + value[len(value)-2:]
}

// LinkRecord is a linked-verified-contact view for the me/profile API.
type LinkRecord struct {
	Kind        string
	Value       string
	VerifiedVia string
	VerifiedAt  time.Time
}

// IdentityLinksFor returns the verified contacts attached to the resolved
// customer(s) of the identity.
func (s *Service) IdentityLinksFor(ctx context.Context, ident Identity) []LinkRecord {
	customerIDs := s.LinkedCustomerIDs(ctx, ident)
	if s.db == nil || len(customerIDs) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(customerIDs)), ",")
	args := []interface{}{}
	for _, id := range customerIDs {
		args = append(args, id)
	}
	// #nosec G202 -- constant SELECT; only the placeholder list is dynamic (one ? per customer id).
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, value, verified_via, verified_at
		FROM authentication_identitylink
		WHERE customer_id IN (`+placeholders+`)
		ORDER BY verified_at DESC
	`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []LinkRecord{}
	for rows.Next() {
		var rec LinkRecord
		if err := rows.Scan(&rec.Kind, &rec.Value, &rec.VerifiedVia, &rec.VerifiedAt); err == nil {
			out = append(out, rec)
		}
	}
	return out
}
