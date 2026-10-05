package identity

// Password credential for the unified customer identity (M-CIM / M-Login).
// One row per customer: phone OR email login resolves via
// authentication_identitylink → customer → payments_identity_account.
// Passwords only mint the standard HMAC identity token — network (RADIUS)
// credentials are never touched.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMinLenDefault     = 10
	passwordLockAfter         = 5
	passwordLockoutFor        = 15 * time.Minute
	passwordResetFreshnessFor = 15 * time.Minute // register/reset must follow a fresh OTP verify
)

var (
	ErrPasswordUnknown = errors.New("identity: no password account for this contact")
	ErrPasswordLocked  = errors.New("identity: account locked, try again later")
	ErrPasswordInvalid = errors.New("identity: incorrect phone/email or password")
	ErrPasswordWeak    = errors.New("identity: password too short (minimum 10 characters)")
	ErrPasswordNoOwner = errors.New("identity: no customer for this contact")
)

// argon2id params (login path ~50-100ms on small cloud instances)
const (
	argon2idMemoryKB = uint32(64 * 1024) // 64 MiB
	argon2idTime     = uint32(2)
	argon2idThreads  = uint8(2)
)

// NormalizePhoneDigits reduces a phone number to its digits ("+26377..." and
// "077..." keep their distinct digit sequences; callers match on suffix).
func NormalizePhoneDigits(phone string) string {
	var b []byte
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b = append(b, byte(r))
		}
	}
	return string(b)
}

// HashPassword produces the argon2id PHC string persisted in
// payments_identity_account.password_hash. Never plaintext.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argon2idTime, argon2idMemoryKB, argon2idThreads, 32)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2idMemoryKB, argon2idTime, argon2idThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword recomputes and constant-time compares against the stored
// PHC string; malformed hashes verify false (defensive).
func VerifyPassword(hash, password string) bool {
	parts := strings.Split(hash, "$") // "", "argon2id", "v=19", "m=65536,t=2,p=2", salt, hash
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version, memKB, timeParam, threads int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memKB, &timeParam, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	keyLen := len(key)
	// Sscanf fields come from the untrusted hash string: bound them before the
	// uint32/uint8 conversions so a crafted m=/t=/p=/hash cannot wrap around
	// and skew the argon2 parameters used for verification.
	if timeParam <= 0 || int64(timeParam) > math.MaxUint32 ||
		memKB <= 0 || int64(memKB) > math.MaxUint32 ||
		threads <= 0 || threads > 15 ||
		keyLen <= 0 || int64(keyLen) > math.MaxUint32 {
		return false
	}
	expected := argon2.IDKey([]byte(password), salt, uint32(timeParam), uint32(memKB), uint8(threads), uint32(keyLen))
	if len(expected) != len(key) {
		return false
	}
	return subtle.ConstantTimeCompare(expected, key) == 1
}

// ─── Contact → customer (shared by login/register/change) ────────────────────

// ContactLookup resolves a channel+recipient to the owning customer via
// identitylink, inserting the legacy upgrade link when found on old columns.
func (s *Service) ContactLookup(ctx context.Context, ch otpChannel, recipient string) (int64, error) {
	normalized := strings.TrimSpace(recipient)
	if ch == channelEmail {
		normalized = strings.ToLower(normalized)
	}
	if normalized == "" {
		return 0, ErrInvalidRecipient
	}

	kind := pointPhone
	suffix := ""
	if ch == channelSMS {
		kind = pointPhone
		normalized = NormalizePhoneDigits(normalized)
		if len(normalized) >= 9 {
			suffix = normalized[len(normalized)-9:]
		}
	}

	var customerID int64
	query := `SELECT customer_id FROM authentication_identitylink WHERE kind = ? AND value = ? LIMIT 1`
	args := []interface{}{kind, normalized}
	if ch == channelSMS && suffix != "" {
		query = `
			SELECT customer_id FROM authentication_identitylink
			WHERE kind = 'phone' AND (value = ? OR value = ? OR value = ? OR RIGHT(value, 9) = ?)
			ORDER BY value = ? DESC
			LIMIT 1
		`
		args = []interface{}{normalized, "+" + normalized, "+" + suffix, suffix, normalized}
	}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&customerID)
	if err == nil {
		return customerID, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("identity: contact lookup: %w", err)
	}

	// Legacy fallback: pre-identitylink customers with the contact on their
	// own columns — upgraded with a link row on first hit.
	column := "customer_phone"
	if ch == channelEmail {
		column = "customer_email"
	}
	err = s.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT id FROM authentication_customer WHERE LOWER(%s) = ? LIMIT 1", column),
		normalized,
	).Scan(&customerID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, sql.ErrNoRows
	}
	if err != nil {
		return 0, fmt.Errorf("identity: legacy contact lookup: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT IGNORE INTO authentication_identitylink
			(customer_id, kind, value, verified_at, verified_via, created_at, updated_at)
		VALUES (?, ?, ?, NOW(), 'legacy', NOW(), NOW())
	`, customerID, kind, normalized); err != nil {
		slog.Warn("identity: legacy contact link upgrade failed", slog.Any("error", err))
	}
	return customerID, nil
}

// ─── Account operations ──────────────────────────────────────────────────────

// RegisterPassword creates/updates the password account (OTP-gated callers:
// the bearer must be a valid unrevoked identity token proving ownership).
func (s *Service) RegisterPassword(ctx context.Context, ident Identity, password string) error {
	if err := validatePasswordLen(s.minPasswordLen, password); err != nil {
		return err
	}
	return s.registerPasswordChecked(ctx, ident, password, false)
}

func (s *Service) registerPasswordChecked(ctx context.Context, ident Identity, password string, requireExisting bool) error {
	if err := validatePasswordLen(s.minPasswordLen, password); err != nil {
		return err
	}
	customerID, err := s.contactCustomerFromIdentity(ctx, ident)
	if errors.Is(err, sql.ErrNoRows) {
		// M-CIM sign-up: a verified identity without a customer row gets one
		// here — password accounts always hang off a customer.
		customerID, err = s.EnsureSelfCustomer(ctx, ident)
	}
	if err != nil {
		return err
	}
	if customerID == 0 {
		return ErrPasswordNoOwner
	}
	if requireExisting {
		var hasAccount bool
		if err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM payments_identity_account WHERE customer_id = ?
		`, customerID).Scan(&hasAccount); err == nil && !hasAccount {
			return ErrPasswordUnknown
		}
	}

	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("identity: password hash: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO payments_identity_account
			(customer_id, password_hash, password_set_at, failed_attempts, locked_until, created_at, updated_at)
		VALUES (?, ?, NOW(), 0, NULL, NOW(), NOW())
		ON DUPLICATE KEY UPDATE
			password_hash = VALUES(password_hash),
			password_set_at = NOW(),
			failed_attempts = 0,
			locked_until = NULL
	`, customerID, hash)
	if err != nil {
		return fmt.Errorf("identity: password register: %w", err)
	}
	slog.Info("identity: password registered", slog.Int64("customer_id", customerID))
	return nil
}

// LoginWithPassword verifies the credential and mints the standard token.
// Brute-force metric: 5 failures → 15-minute lock on the account row.
func (s *Service) LoginWithPassword(ctx context.Context, channelStr, recipient, password string) (string, Identity, error) {
	var ch otpChannel
	switch strings.ToLower(strings.TrimSpace(channelStr)) {
	case pointEmail:
		ch = channelEmail
	case "sms", "":
		ch = channelSMS
	default:
		return "", Identity{}, ErrUnsupportedChannel
	}
	recipient, err := validateRecipient(ch, recipient)
	if err != nil {
		return "", Identity{}, err
	}
	customerID, err := s.contactCustomer(ctx, ch, recipient)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", Identity{}, ErrPasswordUnknown
		}
		return "", Identity{}, err
	}

	var (
		id           int64
		passwordHash string
		lockedUntil  sql.NullTime
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT id, password_hash, locked_until
		FROM payments_identity_account
		WHERE customer_id = ?
	`, customerID).Scan(&id, &passwordHash, &lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return "", Identity{}, ErrPasswordUnknown
	}
	if err != nil {
		return "", Identity{}, fmt.Errorf("identity: account load: %w", err)
	}
	if lockedUntil.Valid && lockedUntil.Time.After(time.Now()) {
		return "", Identity{}, ErrPasswordLocked
	}

	ident := Identity{}
	switch ch {
	case channelSMS:
		ident.Phone = recipient
	case channelEmail:
		ident.Email = recipient
	}

	if !VerifyPassword(passwordHash, password) {
		// Atomic increment: concurrent failures must not clobber each other.
		// The old read-modify-write under-counted (a pair of parallel attempts
		// added 1 instead of 2), so an attacker firing requests concurrently
		// could keep failed_attempts below the threshold indefinitely.
		_, err := s.db.ExecContext(ctx, `
			UPDATE payments_identity_account
			SET failed_attempts = failed_attempts + 1,
				locked_until = CASE WHEN failed_attempts + 1 >= ? THEN ? ELSE locked_until END
			WHERE id = ?
		`, passwordLockAfter, time.Now().Add(passwordLockoutFor), id)
		if err != nil {
			return "", Identity{}, fmt.Errorf("identity: record failed login: %w", err)
		}
		var attempts int
		var locked sql.NullTime
		if err := s.db.QueryRowContext(ctx, `
			SELECT failed_attempts, locked_until
			FROM payments_identity_account
			WHERE id = ?
		`, id).Scan(&attempts, &locked); err != nil {
			return "", Identity{}, fmt.Errorf("identity: read lock state: %w", err)
		}
		if locked.Valid && locked.Time.After(time.Now()) {
			slog.Warn("identity: account locked after failed logins",
				slog.Int64("customer_id", customerID),
				slog.Int("failed_attempts", attempts),
			)
			return "", Identity{}, ErrPasswordLocked
		}
		return "", Identity{}, ErrPasswordInvalid
	}

	token, err := s.IssueToken(ident)
	if err != nil {
		return "", Identity{}, err
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE payments_identity_account
		SET failed_attempts = 0, locked_until = NULL
		WHERE id = ?
	`, id)
	s.RegisterVerifiedContact(ctx, ident, ViaOtp)
	slog.Info("identity: password login", slog.Int64("customer_id", customerID))
	return token, ident, nil
}

// ChangePassword replaces the password for the token-bearer customer
// (both the current and the new password required).
func (s *Service) ChangePassword(ctx context.Context, validated ValidatedToken, currentPassword, newPassword string) error {
	if err := validatePasswordLen(s.minPasswordLen, newPassword); err != nil {
		return err
	}
	customerID, err := s.contactCustomerFromIdentity(ctx, validated.Identity)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPasswordUnknown
		}
		return err
	}
	var passwordHash string
	err = s.db.QueryRowContext(ctx, `
		SELECT password_hash FROM payments_identity_account WHERE customer_id = ?
	`, customerID).Scan(&passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPasswordUnknown
	}
	if err != nil {
		return fmt.Errorf("identity: account load: %w", err)
	}
	if !VerifyPassword(passwordHash, currentPassword) {
		return ErrPasswordInvalid
	}
	return s.setPassword(ctx, customerID, newPassword)
}

// ResetPassword sets a new password without a current one — allowed ONLY on
// a fresh identity token (iat within the reset freshness window = customer
// just verified by OTP/Google).
func (s *Service) ResetPassword(ctx context.Context, validated ValidatedToken, newPassword string) error {
	if err := validatePasswordLen(s.minPasswordLen, newPassword); err != nil {
		return err
	}
	if validated.IssuedAtTime().IsZero() ||
		time.Since(validated.IssuedAtTime()) > passwordResetFreshnessFor {
		return fmt.Errorf("%w: verify by OTP/Google again to reset the password", ErrPasswordLocked)
	}
	customerID, err := s.contactCustomerFromIdentity(ctx, validated.Identity)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPasswordNoOwner
		}
		return err
	}
	return s.setPassword(ctx, customerID, newPassword)
}

// ─── internals ───────────────────────────────────────────────────────────────

// contactCustomer resolves the customer id for a contact. Empty kind/values
// with an already-validated identity go straight to the identitylink.// contactCustomerFromIdentity resolves the customer for an already-verified
// identity (register/change/reset all come with a valid token).
func (s *Service) contactCustomerFromIdentity(ctx context.Context, ident Identity) (int64, error) {
	if ident.Email != "" {
		return s.customerByLink(ctx, pointEmail, strings.ToLower(ident.Email))
	}
	if ident.Phone != "" {
		digits := NormalizePhoneDigits(ident.Phone)
		if len(digits) >= 9 {
			return s.customerByLinkOrSuffix(ctx, digits)
		}
	}
	return 0, sql.ErrNoRows
}

// customerByLinkOrSuffix resolves a digit phone identitylink (exact first,
// distinct "+digits" then last-9 suffix), falling back to the legacy column.
func (s *Service) customerByLinkOrSuffix(ctx context.Context, digits string) (int64, error) {
	suffix := digits[len(digits)-9:]
	query := `
		SELECT customer_id FROM authentication_identitylink
		WHERE kind = 'phone' AND (value = ? OR value = ? OR RIGHT(value, 9) = ?)
		ORDER BY value = ? DESC
		LIMIT 1
	`
	var customerID int64
	err := s.db.QueryRowContext(ctx, query, digits, "+"+digits, suffix, digits).Scan(&customerID)
	if err == nil {
		return customerID, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("identity: contact lookup: %w", err)
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT id FROM authentication_customer
		WHERE RIGHT(REGEXP_REPLACE(customer_phone, '[^0-9]', ''), 9) = ?
		LIMIT 1
	`, suffix).Scan(&customerID)
	if err != nil {
		return 0, sql.ErrNoRows
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT IGNORE INTO authentication_identitylink
			(customer_id, kind, value, verified_at, verified_via, created_at, updated_at)
		VALUES (?, 'phone', ?, NOW(), 'legacy', NOW(), NOW())
	`, customerID, digits); err != nil {
		slog.Warn("identity: legacy contact link upgrade failed", slog.Any("error", err))
	}
	return customerID, nil
}

// customerByLinkOrSuffix variant for email.
func (s *Service) customerByLink(ctx context.Context, kind, value string) (int64, error) {
	var customerID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT customer_id FROM authentication_identitylink
		WHERE kind = ? AND value = ? LIMIT 1
	`, kind, value).Scan(&customerID)
	if err == nil {
		return customerID, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("identity: contact lookup: %w", err)
	}
	column := "customer_email"
	if kind == pointPhone {
		column = "customer_phone"
	}
	err = s.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT id FROM authentication_customer WHERE LOWER(%s) = ? LIMIT 1", column),
		value,
	).Scan(&customerID)
	if err != nil {
		return 0, sql.ErrNoRows
	}
	return customerID, nil
}

// contactCustomer routes a login credential (channel+recipient) to its customer.
func (s *Service) contactCustomer(ctx context.Context, ch otpChannel, recipient string) (int64, error) { // ch normalized by caller
	value, err := validateRecipient(ch, recipient)
	if err != nil {
		return 0, err
	}
	if ch == channelEmail {
		return s.customerByLink(ctx, pointEmail, strings.ToLower(value))
	}
	digits := NormalizePhoneDigits(value)
	if len(digits) >= 9 {
		return s.customerByLinkOrSuffix(ctx, digits)
	}

	// Digit-only match on the customer_phone column (exact string compare).
	var customerID int64
	err = s.db.QueryRowContext(ctx,
		"SELECT id FROM authentication_customer WHERE customer_phone = ? LIMIT 1", digits,
	).Scan(&customerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = s.db.QueryRowContext(ctx,
				"SELECT id FROM authentication_customer WHERE customer_phone = ? LIMIT 1", "+"+digits,
			).Scan(&customerID)
		}
		if err != nil {
			return 0, sql.ErrNoRows
		}
	}
	return customerID, nil
}

// setPassword writes the hash row (insert or update), idempotent.
func (s *Service) setPassword(ctx context.Context, customerID int64, newPassword string) error {
	hash, err := HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("identity: password hash: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO payments_identity_account
			(customer_id, password_hash, password_set_at, failed_attempts, locked_until, created_at, updated_at)
		VALUES (?, ?, NOW(), 0, NULL, NOW(), NOW())
		ON DUPLICATE KEY UPDATE
			password_hash = VALUES(password_hash),
			password_set_at = NOW(),
			failed_attempts = 0,
			locked_until = NULL
	`, customerID, hash); err != nil {
		return fmt.Errorf("identity: password set: %w", err)
	}
	slog.Info("identity: password set", slog.Int64("customer_id", customerID))
	return nil
}

func validatePasswordLen(minLen int, password string) error {
	if len(password) < minLen {
		return fmt.Errorf("%w (minimum %d characters)", ErrPasswordWeak, minLen)
	}
	return nil
}

// PasswordStatus reports whether a contact has a password account
// (contact → identitylink → account row). Used by the login screen to
// default to the password method when available.
func (s *Service) PasswordStatus(ctx context.Context, channelStr string, recipient string) (bool, error) {
	var ch otpChannel
	switch strings.ToLower(strings.TrimSpace(channelStr)) {
	case pointEmail:
		ch = channelEmail
	case "sms", "":
		ch = channelSMS
	default:
		return false, ErrUnsupportedChannel
	}
	customerID, err := s.contactCustomer(ctx, ch, recipient)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if customerID == 0 {
		return false, nil
	}
	var hasAccount bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM payments_identity_account WHERE customer_id = ?
	`, customerID).Scan(&hasAccount); err != nil {
		return false, fmt.Errorf("identity: password status: %w", err)
	}
	return hasAccount, nil
}
