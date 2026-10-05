// Package identity — multi-tier contact claims (M-CIM):
//
//	tier 1: verified_via = 'otp'     (proven via OTP / Google SSO)
//	tier 2: verified_via = 'purchase'(proven by a checkout that carried it)
//	tier 3: verified_via = 'contact' (self-added on profile, unverified)
//
// Higher tiers supersede lower ones. A self-added phone/email immediately
// claims purchases whose checkout contact matches, matching the user's
// "add phone → purchases identifiable — no verification needed" flow.
package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// contact trust tiers, ordered ascending (highest last)
const (
	ViaOtp      = "otp"
	ViaPurchase = "purchase" // an executed checkout proves contact ownership
	ViaContact  = "contact"  // self-added on profile; unverified
	ViaLegacy   = "legacy"
	ViaGoogle   = "google"
)

// ErrContactInUse — the contact is strongly claimed by another customer.
// Suggest support / an OTP claim instead (supersede policy lives here).
var ErrContactInUse = errors.New("identity: contact already claimed by another customer")

// tierRank: higher wins (used for supersede decisions)
func tierRank(via string) int {
	switch strings.ToLower(via) {
	case ViaOtp, ViaGoogle:
		return 3
	case ViaPurchase:
		return 2
	case ViaContact:
		return 1
	case ViaLegacy:
		return 0
	}
	return 0
}

// normalizeContact canonicalizes the value for storage/matching.
func normalizeContact(kind, value string) string {
	value = strings.TrimSpace(value)
	if kind == pointEmail {
		return strings.ToLower(value)
	}
	// phones: digits only — identitylink values store digit form
	return NormalizePhoneDigits(value)
}

// AddContact attaches a contact (phone or email) to the logged-in
// customer's identitylink set WITHOUT verification (tier 'contact').
// Supersede rule (user decision 2026-09-29): a newer OTP-verified claim
// always wins; an 'otp'/'purchase' row held by ANOTHER customer blocks the
// add with ErrContactInUse unless the caller marks takeOver=true, in which
// case the prior contact-tier row is removed (archived by deletion) and the
// new one inserted for the logged-in customer.
func (s *Service) AddContact(ctx context.Context, validated ValidatedToken, kind, value string, takeOver bool) error {
	kind = strings.ToLower(strings.TrimSpace(kind))
	value = normalizeContact(kind, value)
	if kind != pointPhone && kind != pointEmail {
		return ErrUnsupportedChannel
	}
	if value == "" {
		return ErrInvalidRecipient
	}

	customerIDs := s.LinkedCustomerIDs(ctx, validated.Identity)
	if len(customerIDs) == 0 {
		// Sign-up creates the customer record: an identity that has never
		// purchased owns a customer row after OTP/Google verification, and a
		// self-added contact must land somewhere even for pre-upgrade tokens.
		customerID, err := s.EnsureSelfCustomer(ctx, validated.Identity)
		if err != nil {
			return err
		}
		customerIDs = []int64{customerID}
	}
	primaryCustomer := customerIDs[0]

	// Collision check across all customers holding this exact contact.
	var (
		ownerID       int64
		ownerVia      string
		heldByOther   bool
		otherCustomer int64
		otherVia      string
	)
	rows, err := s.db.QueryContext(ctx, `
		SELECT customer_id, verified_via FROM authentication_identitylink
		WHERE kind = ? AND value = ?
	`, kind, value)
	if err != nil {
		return fmt.Errorf("identity: contact claim lookup: %w", err)
	}
	for rows.Next() {
		var id int64
		var via string
		if scanErr := rows.Scan(&id, &via); scanErr == nil {
			if id == primaryCustomer {
				ownerID = id
				ownerVia = via
			} else {
				heldByOther = true
				otherCustomer = id
				otherVia = via
			}
		}
	}
	_ = rows.Close()

	if ownerID == primaryCustomer {
		// Already linked to us — upgrade tier in place if the new tier wins.
		if tierRank(ownerVia) < tierRank("contact") {
			if _, err := s.db.ExecContext(ctx, `
				UPDATE authentication_identitylink
				SET verified_via = 'contact', updated_at = NOW()
				WHERE customer_id = ? AND kind = ? AND value = ?
			`, primaryCustomer, kind, value); err == nil {
				slog.Info("identity: contact tier registered (self-added)",
					slog.String("kind", kind),
					slog.String("value", maskContact(value)),
					slog.Int64("customer_id", primaryCustomer),
				)
			}
		}
		return nil
	}

	if heldByOther {
		// Sign the supersede rule: only a logged-in OTP/buyer identity is
		// allowed to takeOver a weaker claim.
		if !takeOver || tierRank(otherVia) >= tierRank(ViaPurchase) {
			return ErrContactInUse
		}
		if _, err := s.db.ExecContext(ctx, `
			DELETE FROM authentication_identitylink
			WHERE customer_id = ? AND kind = ? AND value = ?
		`, otherCustomer, kind, value); err != nil {
			return fmt.Errorf("identity: supersede delete: %w", err)
		}
		slog.Warn("identity: contact superseded (takeOver)",
			slog.String("kind", kind),
			slog.String("value", maskContact(value)),
			slog.Int64("prior_customer_id", otherCustomer),
			slog.Int64("new_customer_id", primaryCustomer),
		)
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO authentication_identitylink
			(customer_id, kind, value, verified_at, verified_via, created_at, updated_at)
		VALUES (?, ?, ?, NOW(), ?, NOW(), NOW())
	`, primaryCustomer, kind, value, ViaContact); err != nil {
		return fmt.Errorf("identity: contact add: %w", err)
	}
	slog.Info("identity: contact added (self-claimed)",
		slog.String("kind", kind),
		slog.String("value", maskContact(value)),
		slog.Int64("customer_id", primaryCustomer),
	)
	return nil
}

// PromoteContact upgrades a self-added contact to OTP trust after the
// customer completes a verification code challenge for that contact.
// Rate limiting and code issuance reuse the OTP infrastructure via
// RequestOTP/VerifyOTP with the OTP channel; success locks the tier.
func (s *Service) PromoteContact(ctx context.Context, validated ValidatedToken, channelStr string, recipient, code string) error {
	var ch otpChannel
	switch strings.ToLower(strings.TrimSpace(channelStr)) {
	case pointEmail:
		ch = channelEmail
	case "sms", "":
		ch = channelSMS
	default:
		return ErrUnsupportedChannel
	}
	customerIDs := s.LinkedCustomerIDs(ctx, validated.Identity)
	if len(customerIDs) == 0 {
		customerID, err := s.EnsureSelfCustomer(ctx, validated.Identity)
		if err != nil {
			return err
		}
		customerIDs = []int64{customerID}
	}
	primaryCustomer := customerIDs[0]

	kind := pointPhone
	if ch == channelEmail {
		kind = pointEmail
	}
	value := normalizeContact(kind, recipient)

	// The contact must already belong to this customer (added before).
	var linkCustomer int64
	err := s.db.QueryRowContext(ctx, `
		SELECT customer_id FROM authentication_identitylink
		WHERE kind = ? AND value = ? LIMIT 1
	`, kind, value).Scan(&linkCustomer)
	if errors.Is(err, context.Canceled) {
		return err
	}
	if err != nil || linkCustomer != primaryCustomer {
		return ErrInvalidRecipient
	}

	// Verify the OTP normally (rate limiters already gate this).
	if _, _, err := s.VerifyOTP(ctx, string(ch), recipient, code); err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE authentication_identitylink
		SET verified_via = 'otp', verified_at = NOW(), updated_at = NOW()
		WHERE customer_id = ? AND kind = ? AND value = ?
	`, primaryCustomer, kind, value); err != nil {
		return fmt.Errorf("identity: contact promote: %w", err)
	}
	slog.Info("identity: contact promoted to otp-verified",
		slog.String("kind", kind),
		slog.String("value", maskContact(value)),
		slog.Int64("customer_id", primaryCustomer),
	)
	return nil
}
