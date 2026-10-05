package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Google SSO sign-in (M-Login): the app obtains a Google id_token
// (expo-auth-session) and hands it here for verification. On success the
// customer receives the same HMAC identity token that OTP verify returns,
// with the Google email as the identity contact.

// googleIssuer defaults to Google and is a var for tests (local JWKS issuer)
var googleIssuer = "https://accounts.google.com"

var (
	ErrGoogleNotConfigured   = errors.New("identity: Google sign-in not configured")
	ErrGoogleTokenInvalid    = errors.New("identity: invalid Google token")
	ErrGoogleEmailUnverified = errors.New("identity: Google account email is not verified")
)

// googleClaims is the subset of the Google id_token payload payments-api needs
type googleClaims struct {
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"` // Google may send bool or string
}

// ConfigureGoogle activates Google identity verification. clientIDs is the
// OAuth audience allowlist (Android/iOS/Web client IDs, comma-separated in
// env); empty clears Google. httpClient allows test injection; nil uses
// defaults.
func (s *Service) ConfigureGoogle(clientIDs []string, httpClient *http.Client) error {
	if len(clientIDs) == 0 {
		s.googleClientIDs = nil
		s.googleVerifier = nil
		return nil
	}
	ctx := context.Background()
	if httpClient != nil {
		ctx = oidc.ClientContext(ctx, httpClient)
	}
	provider, err := oidc.NewProvider(ctx, googleIssuer)
	if err != nil {
		return fmt.Errorf("identity: Google OIDC discovery failed: %w", err)
	}
	// Audience allowlisting is done explicitly (a token may be minted for any
	// of the platform client IDs, so the verifier itself cannot pin one).
	s.googleVerifier = provider.Verifier(&oidc.Config{
		ClientID:          clientIDs[0],
		SkipClientIDCheck: true,
		SkipExpiryCheck:   false,
	})
	s.googleClientIDs = clientIDs
	slog.Info("identity: Google sign-in enabled", slog.Int("client_ids", len(clientIDs)))
	return nil
}

// VerifyGoogleIDToken validates a Google id_token (signature, expiry, audience
// allowlist, email presence, email_verified) and returns the verified email
// identity. email_verified=false surfaces ErrGoogleEmailUnverified so callers
// can route the user to the email-OTP flow (M-Login decision).
func (s *Service) VerifyGoogleIDToken(ctx context.Context, rawIDToken string) (Identity, error) {
	if s.googleVerifier == nil {
		return Identity{}, ErrGoogleNotConfigured
	}

	idToken, err := s.googleVerifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrGoogleTokenInvalid, err)
	}
	if !audienceAllowlisted(idToken.Audience, s.googleClientIDs) {
		return Identity{}, fmt.Errorf("%w: audience not allowlisted", ErrGoogleTokenInvalid)
	}

	var claims googleClaims
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrGoogleTokenInvalid, err)
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" {
		return Identity{}, fmt.Errorf("%w: missing email claim", ErrGoogleTokenInvalid)
	}
	if !emailVerifiedTrue(claims.EmailVerified) {
		return Identity{}, ErrGoogleEmailUnverified
	}
	return Identity{Email: email}, nil
}

func audienceAllowlisted(auds []string, clientIDs []string) bool {
	for _, aud := range auds {
		for _, wanted := range clientIDs {
			if aud == wanted {
				return true
			}
		}
	}
	return false
}

func emailVerifiedTrue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true"
	default:
		return false
	}
}
