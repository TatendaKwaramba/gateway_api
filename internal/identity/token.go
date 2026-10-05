package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// tokenPayload is the JWT-like (but simpler) identity token body.
type tokenPayload struct {
	Phone     string `json:"phone,omitempty"`
	Email     string `json:"email,omitempty"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	// KeyID identifies the HMAC secret generation ("k" + 8 hex of
	// sha256(secret)), enabling zero-downtime secret rotation: validators
	// hold primary + previous secrets and select by kid. Legacy tokens
	// (M4 token shape) carry no kid and validate against the primary key.
	KeyID string `json:"kid,omitempty"`
}

// hmacSign returns the hex-encoded HMAC-SHA256 of message under secret
func hmacSign(secret []byte, message string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// keyIDFor derives a short, non-secret key identifier from a specific secret
func keyIDFor(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "k" + hex.EncodeToString(sum[:4])
}

// ValidatedToken is a verified token plus its embedded issue time, needed by
// callers that enforce iat-relative rules (revocation).
type ValidatedToken struct {
	Identity Identity
	IssuedAt int64
}

// IssuedAtTime returns the token issue time (zero when absent/legacy-shaped)
func (v ValidatedToken) IssuedAtTime() time.Time {
	if v.IssuedAt <= 0 {
		return time.Time{}
	}
	return time.Unix(v.IssuedAt, 0)
}

// ValidateToken verifies the HMAC and expiry, returning the embedded identity.
// Validates against primary + configured previous secrets (rotation).
func (s *Service) ValidateToken(token string) (Identity, error) {
	validated, err := s.ValidateTokenFull(token)
	if err != nil {
		return Identity{}, err
	}
	return validated.Identity, nil
}

// secretFor picks the HMAC secret matching a token kid. Legacy (M4) tokens
// carry no kid and validate against the primary secret only.
func (s *Service) secretFor(kid string) ([]byte, bool) {
	if kid == "" || kid == s.primaryID {
		return s.secret, true
	}
	secret, ok := s.previousSecrets[kid]
	return secret, ok
}

// VerifySignatureAndExpiry decodes + verifies a token, returning the payload.
// Shared by flash-api-compatible validation rules.
func (s *Service) ValidateTokenFull(token string) (ValidatedToken, error) {
	if !s.Enabled() {
		return ValidatedToken{}, ErrDisabled
	}
	body, sig, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || body == "" || sig == "" {
		return ValidatedToken{}, ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return ValidatedToken{}, ErrInvalidToken
	}
	var payload tokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ValidatedToken{}, ErrInvalidToken
	}

	secret, known := s.secretFor(payload.KeyID)
	if !known {
		return ValidatedToken{}, ErrInvalidToken
	}
	expected := hmacSign(secret, body)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return ValidatedToken{}, ErrInvalidToken
	}
	now := time.Now().Unix()
	if now > payload.ExpiresAt || payload.ExpiresAt <= 0 {
		return ValidatedToken{}, ErrInvalidToken
	}
	return ValidatedToken{
		Identity: Identity{Phone: payload.Phone, Email: payload.Email},
		IssuedAt: payload.IssuedAt,
	}, nil
}
