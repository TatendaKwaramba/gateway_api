// Package identity implements lightweight customer identity for the mobile app:
// OTP codes delivered over SMS or email, and HMAC-signed identity tokens that
// authorize device (push token) registration and usage lookups.
//
// Tokens are stateless (payload + HMAC, ~30 day TTL); OTP codes are stored
// hashed in the payments_otptoken table (DDL owned by Django migrations).
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/freeradius/payments-api/internal/notify"
)

const (
	otpLength        = 6
	otpExpiry        = 10 * time.Minute
	otpMaxAttempts   = 5
	otpMaxPerHour    = 5
	identityTokenTTL = 30 * 24 * time.Hour
)

var (
	ErrDisabled           = errors.New("identity: not configured")
	ErrInvalidRecipient   = errors.New("identity: invalid recipient")
	ErrRateLimited        = errors.New("identity: too many OTP requests, try again later")
	ErrInvalidCode        = errors.New("identity: invalid verification code")
	ErrCodeExpired        = errors.New("identity: verification code expired")
	ErrTooManyAttempts    = errors.New("identity: too many attempts, request a new code")
	ErrInvalidToken       = errors.New("identity: invalid or expired token")
	ErrUnsupportedChannel = errors.New("identity: unsupported OTP channel")
)

// Identity is a verified customer contact point (phone, email, or both).
type Identity struct {
	Phone string `json:"phone,omitempty"`
	Email string `json:"email,omitempty"`
}

// IsEmpty reports whether the identity carries no contact point
func (i Identity) IsEmpty() bool {
	return i.Phone == "" && i.Email == ""
}

// Service issues and verifies OTP codes and identity tokens
type Service struct {
	db        *sql.DB
	sms       notify.Provider
	email     notify.Provider
	secret    []byte
	primaryID string
	// previousSecrets maps legacy token kids -> secrets, used during secret
	// rotation (primary rotated; previous still validates old tokens)
	previousSecrets map[string][]byte
	// Google SSO (M-Login)
	googleVerifier  *oidc.IDTokenVerifier
	googleClientIDs []string
	// Password accounts (M-CIM): policy floor, set via SetPasswordPolicy
	minPasswordLen int
	tokenTTL       time.Duration
	emailFrom      string
	brandName      string
	emailAccent    string // hex color for the branded OTP emails (no '#')
	// Organization stamped on sign-up customer rows (0 = auto-detect)
	signupOrgID int64
}

// NewService creates an identity service. It is disabled (Enabled() == false)
// when secret is empty; handlers must guard on that.
func NewService(db *sql.DB, sms notify.Provider, email notify.Provider, secret string, emailFrom string) *Service {
	secretBytes := []byte(secret)
	svc := &Service{
		db:        db,
		sms:       sms,
		email:     email,
		secret:    secretBytes,
		tokenTTL:  identityTokenTTL,
		emailFrom: emailFrom,
		brandName: "AmaniConnect",
	}
	if len(secretBytes) >= 32 {
		svc.primaryID = keyIDFor(secret)
		svc.previousSecrets = map[string][]byte{}
	}
	svc.minPasswordLen = passwordMinLenDefault
	return svc
}

// SetPasswordPolicy overrides the minimum password length (0 keeps default).
func (s *Service) SetPasswordPolicy(minLen int) {
	if len(minPasswordLenGuard(minLen)) > 0 && minLen > 0 {
		s.minPasswordLen = minLen
	}
}

func minPasswordLenGuard(v int) string { return "" }

// SetPreviousSecret registers the pre-rotation HMAC secret so tokens minted
// with the old key keep validating during a rollout window. Silent no-op for
// empty input.
func (s *Service) SetPreviousSecret(previous string) {
	if previous == "" || len(previous) < 32 || !s.Enabled() {
		return
	}
	kid := keyIDFor(previous)
	if kid == s.primaryID {
		return
	}
	s.previousSecrets[kid] = []byte(previous)
}

// CurrentKeyID returns the kid baked into newly issued tokens ("" when disabled)
func (s *Service) CurrentKeyID() string {
	if !s.Enabled() {
		return ""
	}
	return s.primaryID
}

// SetBrandName overrides the user-visible brand name used in SMS/email texts
func (s *Service) SetBrandName(brandName string) {
	if brandName == "" {
		return
	}
	s.brandName = brandName
}

// SetEmailAccentColor overrides the branded OTP template header color
// (hex WITHOUT the leading '#', e.g. "145377").
func (s *Service) SetEmailAccentColor(color string) {
	s.emailAccent = color
}

// SetDefaultOrgID pins the organization stamped on sign-up customer rows
// (0 keeps the auto-detect behaviour: the sole organization when unambiguous).
func (s *Service) SetDefaultOrgID(orgID int64) {
	s.signupOrgID = orgID
}

// Enabled reports whether identity features are configured
func (s *Service) Enabled() bool {
	return s != nil && len(s.secret) >= 32
}

type otpChannel string

const (
	channelSMS   otpChannel = "sms"
	channelEmail otpChannel = "email"

	// pointEmail/pointPhone are the wire values shared by OTP channel switches,
	// identitylink kinds, and log attribute names.
	pointEmail = "email"
	pointPhone = "phone"
)

var (
	emailRe    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	phoneRawRe = regexp.MustCompile(`[^0-9+]`)
)

// NormalizePhone canonicalizes a phone number for storage/matching:
// strips separators, requires a leading + (E.164-ish). The last-9-digit
// suffix is used for tolerant matching against locally formatted numbers.
func NormalizePhone(phone string) string {
	digits := phoneRawRe.ReplaceAllString(strings.TrimSpace(phone), "")
	if digits == "" {
		return ""
	}
	if !strings.HasPrefix(digits, "+") {
		digits = "+" + digits
	}
	if len(digits) < 8 || len(digits) > 20 {
		return ""
	}
	return digits
}

func validateRecipient(ch otpChannel, recipient string) (string, error) {
	r := strings.TrimSpace(recipient)
	switch ch {
	case channelSMS:
		n := NormalizePhone(r)
		if n == "" {
			return "", fmt.Errorf("%w: %q", ErrInvalidRecipient, recipient)
		}
		return n, nil
	case channelEmail:
		if !emailRe.MatchString(r) {
			return "", fmt.Errorf("%w: %q", ErrInvalidRecipient, recipient)
		}
		return strings.ToLower(r), nil
	default:
		return "", ErrUnsupportedChannel
	}
}

// RequestOTP generates an OTP code for the recipient and delivers it over
// the requested channel. Previous unconsumed codes for the recipient are
// invalidated.
func (s *Service) RequestOTP(ctx context.Context, channelStr, recipientStr string) (time.Time, error) {
	if !s.Enabled() {
		return time.Time{}, ErrDisabled
	}
	ch := otpChannel(channelStr)
	recipient, err := validateRecipient(ch, recipientStr)
	if err != nil {
		return time.Time{}, err
	}

	// Rate limit: at most otpMaxPerHour requests per recipient per hour
	var recent int
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM payments_otptoken
		WHERE channel = ? AND recipient = ? AND created_at > NOW() - INTERVAL 1 HOUR
	`, ch, recipient).Scan(&recent)
	if err != nil {
		return time.Time{}, fmt.Errorf("identity: rate limit query: %w", err)
	}
	if recent >= otpMaxPerHour {
		return time.Time{}, &RateLimitedError{RetryAfter: otpWindowRemaining(ctx, s.db, ch, recipient)}
	}

	code, err := generateOTP()
	if err != nil {
		return time.Time{}, fmt.Errorf("identity: generate code: %w", err)
	}

	expiresAt := time.Now().Add(otpExpiry)

	// Invalidate previous codes, then store the new one (hashed)
	if _, err := s.db.ExecContext(ctx, `
		UPDATE payments_otptoken SET consumed_at = NOW()
		WHERE channel = ? AND recipient = ? AND consumed_at IS NULL
	`, ch, recipient); err != nil {
		return time.Time{}, fmt.Errorf("identity: invalidate old codes: %w", err)
	}

	codeHash := sha256Hex(code)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO payments_otptoken (channel, recipient, code_hash, attempts, expires_at, created_at)
		VALUES (?, ?, ?, 0, ?, NOW())
	`, ch, recipient, codeHash, expiresAt); err != nil {
		return time.Time{}, fmt.Errorf("identity: store code: %w", err)
	}

	if err := s.deliverOTP(ctx, ch, recipient, code); err != nil {
		return time.Time{}, fmt.Errorf("identity: deliver code: %w", err)
	}

	slog.Info("identity: otp requested",
		slog.String("channel", string(ch)),
		slog.String("recipient", maskRecipient(recipient)),
		slog.Time("expires_at", expiresAt),
	)
	return expiresAt, nil
}

// RequestOTPCh mirrors RequestOTP with channel as string (handler-facing).
func (s *Service) RequestOTPCh(ctx context.Context, channelStr, recipientStr string) (time.Time, error) {
	return s.RequestOTP(ctx, channelStr, recipientStr)
}

// VerifyOTP checks the code and, on success, returns a signed identity token.
func (s *Service) VerifyOTP(ctx context.Context, channelStr, recipientStr, code string) (string, Identity, error) {
	if !s.Enabled() {
		return "", Identity{}, ErrDisabled
	}
	ch := otpChannel(channelStr)
	recipient, err := validateRecipient(ch, recipientStr)
	if err != nil {
		return "", Identity{}, err
	}
	code = strings.TrimSpace(code)
	if len(code) != otpLength {
		return "", Identity{}, ErrInvalidCode
	}

	var id int64
	var codeHash string
	var attempts int
	err = s.db.QueryRowContext(ctx, `
		SELECT id, code_hash, attempts FROM payments_otptoken
		WHERE channel = ? AND recipient = ? AND consumed_at IS NULL
		ORDER BY id DESC LIMIT 1
	`, ch, recipient).Scan(&id, &codeHash, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", Identity{}, ErrInvalidCode
	}
	if err != nil {
		return "", Identity{}, fmt.Errorf("identity: lookup code: %w", err)
	}
	if attempts >= otpMaxAttempts {
		return "", Identity{}, ErrTooManyAttempts
	}

	if subtle.ConstantTimeCompare([]byte(sha256Hex(code)), []byte(codeHash)) != 1 {
		_, _ = s.db.ExecContext(ctx, `
			UPDATE payments_otptoken SET attempts = attempts + 1 WHERE id = ?
		`, id)
		return "", Identity{}, ErrInvalidCode
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE payments_otptoken SET consumed_at = NOW()
		WHERE id = ? AND consumed_at IS NULL AND expires_at > NOW()
	`, id)
	if err != nil {
		return "", Identity{}, fmt.Errorf("identity: consume code: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", Identity{}, ErrCodeExpired
	}

	identity := Identity{}
	switch ch {
	case channelSMS:
		identity.Phone = recipient
	case channelEmail:
		identity.Email = recipient
	}

	token, err := s.IssueToken(identity)
	if err != nil {
		return "", Identity{}, err
	}
	s.RegisterVerifiedContact(ctx, identity, ViaOtp) // M-CIM: link + sign-up customer

	slog.Info("identity: otp verified",
		slog.String("channel", string(ch)),
		slog.String("recipient", maskRecipient(recipient)),
	)
	return token, identity, nil
}

// IssueToken signs an identity token for the given identity
func (s *Service) IssueToken(identity Identity) (string, error) {
	if identity.IsEmpty() {
		return "", fmt.Errorf("identity: cannot issue token for empty identity")
	}
	now := time.Now()
	payload := tokenPayload{
		Phone:     identity.Phone,
		Email:     identity.Email,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(s.tokenTTL).Unix(),
		KeyID:     s.primaryID,
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("identity: encode payload: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + hmacSign(s.secret, body), nil
}

func (s *Service) deliverOTP(ctx context.Context, ch otpChannel, recipient, code string) error {
	message := notify.OtpEmail{
		Brand:   s.brandName,
		Code:    code,
		Minutes: int(otpExpiry.Minutes()),
		Color:   s.emailAccent,
	}
	text, htmlMessage := message.RenderText(), message.RenderHTML()

	switch ch {
	case channelSMS:
		if s.sms == nil {
			return fmt.Errorf("identity: no SMS provider configured")
		}
		return s.sms.SendSMS(ctx, recipient, text)
	case channelEmail:
		if s.email == nil {
			return fmt.Errorf("identity: no email provider configured")
		}
		return s.email.Send(ctx, notify.Message{
			To:      recipient,
			Subject: fmt.Sprintf("Your %s verification code", s.brandName),
			Body:    text,
			HTML:    htmlMessage,
		})
	default:
		return ErrUnsupportedChannel
	}
}

func generateOTP() (string, error) {
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(otpLength), nil)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", otpLength, n.Int64()), nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

func maskRecipient(recipient string) string {
	if len(recipient) <= 4 {
		return "***"
	}
	return "***" + recipient[len(recipient)-4:]
}

// RateLimitedError carries the retry window for OTP request rate-limiting so
// clients can render an accurate cooldown (M-Login: resend timer).
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

// RetryAfterSeconds returns the client-facing integer seconds (>= 0)
func (e *RateLimitedError) RetryAfterSeconds() int {
	if e == nil || e.RetryAfter <= 0 {
		return 3600
	}
	return int(e.RetryAfter / time.Second)
}

// rateLimitedRetryAfter computes when the oldest of the last hour's OTP
// requests leaves the 1-hour window. Falls back to a full hour on failure.
func otpWindowRemaining(ctx context.Context, db *sql.DB, ch otpChannel, recipient string) time.Duration {
	if db == nil {
		return time.Hour
	}
	var seconds int
	err := db.QueryRowContext(ctx, `
		SELECT GREATEST(0, 3600 - TIMESTAMPDIFF(SECOND, MIN(created_at), NOW()))
		FROM payments_otptoken
		WHERE channel = ? AND recipient = ? AND created_at > NOW() - INTERVAL 1 HOUR
	`, ch, recipient).Scan(&seconds)
	if err != nil || seconds <= 0 {
		return time.Hour
	}
	return time.Duration(seconds) * time.Second
}
