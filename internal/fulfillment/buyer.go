package fulfillment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// Buyer-customer consolidation (M-CIM): purchases attach to ONE human
// customer, keyed through verified contacts (authentication_identitylink)
// instead of minting a PIN-ghost customer row per voucher.

// normalizeContact returns (kind, value) for an identitylink entry, or "" kind
// when neither contact is usable.
func contactKind(req FulfillRequest) (kind, value string) {
	phone := strings.TrimSpace(req.CustomerPhone)
	email := strings.ToLower(strings.TrimSpace(req.CustomerEmail))
	if phone != "" {
		return "phone", digitsOnly(phone)
	}
	if email != "" {
		return "email", email
	}
	return "", ""
}

// digitsOnly reduces a phone (local or E.164) to digits, canonical key for
// identitylink rows (value normalization mirrors identitylink Django写法).
// phoneSuffix9 returns the last 9 digits of a digit-run ("" when shorter).
func phoneSuffix9(s string) string {
	var b []byte
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b = append(b, byte(r))
		}
	}
	if len(b) > 9 {
		return string(b[len(b)-9:])
	}
	return string(b)
}

func digitsOnly(s string) string {
	var b []byte
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b = append(b, byte(r))
		}
	}
	return string(b)
}

// EnsureBuyerCustomer resolves or creates the human customer behind a
// purchase. Priority:
//  1. identitylink(kind, value) — previously verified contact
//  2. customer row with the exact phone/email (advertised by the buyer) —
//     link gets minted retroactively
//  3. new customer (one row per buyer contact), with identitylink rows
//     for every captured contact
//
// A purchase with NO captured contact stays anonymous: no customer row is
// created (vouchers_voucher.customer_id / radcheck links stay NULL where
// supported — voucher-level records still fully work via voucher_pin).
func (s *Service) EnsureBuyerCustomer(ctx context.Context, req FulfillRequest, orgID sql.NullInt64) (int64, error) {
	kind, value := contactKind(req)
	if kind == "" {
		return 0, nil // anonymous purchase — voucher relies on PIN alone
	}

	// 1) Previously verified: identitylink is authoritative. Phone values
	// bridge local-vs-E.164 formats via the trailing-9 rule (mobile-app-wide
	// convention).
	//
	// The unique(kind, value) key means one row per stored format, so the
	// same human routinely has a local-format row and an E.164 row — and
	// they need not point at the same customer. Ownership therefore follows
	// the verification tier first (a link proved by OTP outranks one minted
	// from a typed checkout contact), exact value second, recency last: a
	// weak or stale claim must never displace a proven one.
	const tierRank = `
		CASE verified_via
			WHEN 'otp' THEN 1
			WHEN 'google' THEN 2
			WHEN 'purchase' THEN 3
			ELSE 4
		END`
	var customerID int64
	query := fmt.Sprintf(`
		SELECT customer_id FROM authentication_identitylink
		WHERE kind = ? AND value = ?
		ORDER BY %s, verified_at DESC, id DESC
		LIMIT 1
	`, tierRank)
	args := []interface{}{kind, value}
	if kind == "phone" && len(value) >= 9 {
		query = fmt.Sprintf(`
		SELECT customer_id FROM authentication_identitylink
		WHERE kind = 'phone' AND (value = ? OR RIGHT(value, 9) = ?)
		ORDER BY %s, value = ? DESC, verified_at DESC, id DESC
		LIMIT 1
	`, tierRank)
		args = []interface{}{value, phoneSuffix9(value), value}
	}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&customerID)
	if err == nil {
		return customerID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("fulfillment: identitylink lookup: %w", err)
	}

	// 2) Exact contact column match (pre-identitylink rows).
	column := "customer_phone"
	compare := req.CustomerPhone
	if kind == "email" {
		column = "customer_email"
		compare = strings.ToLower(strings.TrimSpace(req.CustomerEmail))
	}
	row := s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT id FROM authentication_customer WHERE %s = ? LIMIT 1
	`, column), compare)
	err = row.Scan(&customerID)
	switch {
	case err == nil:
		// Upgrade an old customer with a verified link row.
		s.linkIdentity(ctx, kind, value, customerID, "purchase", orgID)
		return customerID, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("fulfillment: buyer lookup: %w", err)
	}

	// 3) Brand new buyer.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO authentication_customer (
			customer_id, customer_type, customer_name, customer_email, customer_phone,
			customer_address, customer_city, organization_id, referred_by_id,
			radius_username, access_type, is_active, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 'Online checkout', 'N/A', ?, ?, ?, ?, TRUE, NOW(), NOW())
	`, fmt.Sprintf("buyer-%d", req.TransactionID), "individual", buyerName(req),
		req.CustomerEmail, req.CustomerPhone, orgID, s.resolveReferredBy(ctx, req), req.CustomerPhone,
		buyerAccessType(req))
	if err != nil {
		return 0, fmt.Errorf("fulfillment: create buyer customer: %w", err)
	}
	customerID, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("fulfillment: buyer customer id: %w", err)
	}

	s.linkIdentity(ctx, kind, value, customerID, "purchase", orgID)
	slog.Info("fulfillment: buyer customer created",
		slog.Int64("customer_id", customerID),
		slog.String("contact", kind),
	)
	return customerID, nil
}

// linkIdentity best-effort inserts the identitylink row (idempotent on the
// unique (kind, value) index: duplicate-key is expected on re-orgs).
func (s *Service) linkIdentity(ctx context.Context, kind, value string, customerID int64, via string, orgID sql.NullInt64) {
	_ = orgID // organizations are encoded on the customer; link rows are org-free (M-CIM decision)
	if _, err := s.db.ExecContext(ctx, `
		INSERT IGNORE INTO authentication_identitylink
			(customer_id, kind, value, verified_at, verified_via, created_at, updated_at)
		VALUES (?, ?, ?, NOW(), ?, NOW(), NOW())
	`, customerID, kind, value, via); err != nil {
		slog.Warn("fulfillment: identitylink write failed",
			slog.Int64("customer_id", customerID),
			slog.Any("error", err),
		)
	}
}

func buyerName(req FulfillRequest) string {
	if req.CustomerPhone != "" {
		return fmt.Sprintf("Buyer %s", digitsOnly(req.CustomerPhone))
	}
	return strings.ToLower(strings.TrimSpace(req.CustomerEmail))
}

func buyerAccessType(req FulfillRequest) string {
	if req.FulfillmentKind == "subscription" {
		return "subscription"
	}
	return "hotspot"
}

// resolveReferredBy maps a purchase referral code to the agent's customer id
// (sales_referrallink → sales_agentprofile), nil when absent or unresolvable.
func (s *Service) resolveReferredBy(ctx context.Context, req FulfillRequest) interface{} {
	if req.ReferralCode == "" {
		return nil
	}
	var agentID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT ap.id FROM sales_agentprofile ap
		JOIN sales_referrallink rl ON rl.agent_id = ap.id
		WHERE rl.code = ? AND rl.is_active = TRUE AND ap.status = 'active'
		LIMIT 1
	`, req.ReferralCode).Scan(&agentID)
	if err != nil {
		return nil
	}
	return agentID
}
