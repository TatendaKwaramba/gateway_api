package identity

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "identity-rotation-test-secret-32-chars!"
const testSecretOld = "identity-rotation-old-secret-32-char!!"

func TestIssueTokenCarriesKid(t *testing.T) {
	svc := NewService(nil, nil, nil, testSecret, "")
	token, err := svc.IssueToken(Identity{Phone: "+263771234567"})
	require.NoError(t, err)
	parts := strings.SplitN(token, ".", 2)
	require.Len(t, parts, 2)
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	var payload tokenPayload
	require.NoError(t, json.Unmarshal(raw, &payload))
	assert.Equal(t, keyIDFor(testSecret), payload.KeyID)
	assert.Equal(t, svc.CurrentKeyID(), payload.KeyID)
}

func TestRotation(t *testing.T) {
	old := NewService(nil, nil, nil, testSecretOld, "")
	oldToken, err := old.IssueToken(Identity{Email: "sub@example.com"})
	require.NoError(t, err)

	rotated := NewService(nil, nil, nil, testSecret, "")
	rotated.SetPreviousSecret(testSecretOld)

	ident, err := rotated.ValidateToken(oldToken)
	assert.NoError(t, err)
	assert.Equal(t, "sub@example.com", ident.Email, "pre-rotation token keeps validating")

	newToken, err := rotated.IssueToken(Identity{Email: "sub@example.com"})
	require.NoError(t, err)
	_, err = old.ValidateToken(newToken)
	assert.Error(t, err, "the pre-rotation service must not accept primary-kid tokens")

	body := strings.SplitN(newToken, ".", 2)[0]
	_, err = rotated.ValidateToken(body + ".0000")
	assert.Error(t, err)
}

func TestLegacyTokenNoKidValidates(t *testing.T) {
	now := time.Now().Unix()
	raw, _ := json.Marshal(map[string]any{
		"phone": "+263771234567", "iat": now, "exp": now + 3600,
	})
	body := base64.RawURLEncoding.EncodeToString(raw)
	legacy := body + "." + hex.EncodeToString(hmacSHA256Sum([]byte(testSecret), body))

	svc := NewService(nil, nil, nil, testSecret, "")
	ident, err := svc.ValidateToken(legacy)
	assert.NoError(t, err)
	assert.Equal(t, "+263771234567", ident.Phone)

	validated, err := svc.ValidateTokenFull(legacy)
	assert.NoError(t, err)
	assert.Equal(t, now, validated.IssuedAt, "legacy iat must surface for revocation windowing")
}

func hmacSHA256Sum(secret []byte, message string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

// ─── Revocation (dockertest) ────────────────────────────────────────────────

func TestRevocation(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	svc := NewService(db, nil, nil, testSecret, "")
	ctx := context.Background()
	phone := "+263771234567"

	before, err := svc.IssueToken(Identity{Phone: phone})
	require.NoError(t, err)
	validated, err := svc.ValidateTokenFull(before)
	require.NoError(t, err)
	revoked, err := svc.IdentityRevoked(ctx, validated.Identity, validated.IssuedAt)
	assert.NoError(t, err)
	assert.False(t, revoked, "no revocation rows yet")

	require.NoError(t, svc.RevokeIdentity(ctx, phone, "", "lost device", ""))

	// Self-service recovery: a fresh token minted on a later second clears the
	// revocation. Same-second ties fail closed (row wins, see revocation.go).
	time.Sleep(1100 * time.Millisecond)
	after, err := svc.IssueToken(Identity{Phone: phone})
	require.NoError(t, err)

	for _, tc := range []struct {
		name  string
		token string
		want  bool
	}{
		{"token issued before revocation", before, true},
		{"token issued after revocation", after, false},
	} {
		validated, err := svc.ValidateTokenFull(tc.token)
		require.NoError(t, err, tc.name)
		got, err := svc.IdentityRevoked(ctx, validated.Identity, validated.IssuedAt)
		assert.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}

	// Legacy tokens (no iat) fail closed against any revocation
	raw, _ := json.Marshal(map[string]any{"phone": phone, "exp": time.Now().Add(time.Hour).Unix()})
	body := base64.RawURLEncoding.EncodeToString(raw)
	legacy := body + "." + hex.EncodeToString(hmacSHA256Sum(svc.secret, body))
	validated, err = svc.ValidateTokenFull(legacy)
	require.NoError(t, err)
	got, err := svc.IdentityRevoked(ctx, validated.Identity, validated.IssuedAt)
	assert.NoError(t, err)
	assert.True(t, got, "legacy tokens with unknown iat fail closed")

	err = svc.RevokeIdentity(ctx, "", "", "", "")
	assert.ErrorContains(t, err, "required")
}

// ─── Google id_token verification (local JWKS issuer) ───────────────────────

func rsaJWK(t *testing.T, key *rsa.PrivateKey, kid string) map[string]any {
	t.Helper()
	pub := key.Public().(*rsa.PublicKey)
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func signGoogleJWT(t *testing.T, priv *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	h := base64.RawURLEncoding.EncodeToString(header)
	c := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := h + "." + c
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	require.NoError(t, err)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func googleIDToken(issuer string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss":            issuer,
		"aud":            "test-client-id-android",
		"email":          "User@Example.com",
		"email_verified": true,
		"iat":            now,
		"exp":            now + 3600,
	}
}

func TestGoogleVerify(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "test-kid"
	var issuer string

	mux := http.NewServeMux()
	var jwksURL string
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": jwksURL})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{rsaJWK(t, priv, kid)}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	issuer = server.URL
	jwksURL = server.URL + "/jwks"

	prevIssuer := googleIssuer
	googleIssuer = server.URL
	defer func() { googleIssuer = prevIssuer }()

	svc := NewService(nil, nil, nil, testSecret, "")
	require.NoError(t, svc.ConfigureGoogle([]string{"test-client-id-android", "test-client-id-ios"}, nil))

	t.Run("verified email issues identity", func(t *testing.T) {
		token := signGoogleJWT(t, priv, kid, googleIDToken(server.URL))
		ident, err := svc.VerifyGoogleIDToken(context.Background(), token)
		require.NoError(t, err)
		assert.Equal(t, "user@example.com", ident.Email, "email normalized lowercase")
	})

	t.Run("unverified email falls back (ErrGoogleEmailUnverified)", func(t *testing.T) {
		claims := googleIDToken(server.URL)
		claims["email_verified"] = false
		token := signGoogleJWT(t, priv, kid, claims)
		_, err := svc.VerifyGoogleIDToken(context.Background(), token)
		require.ErrorIs(t, err, ErrGoogleEmailUnverified)
	})

	t.Run("audience not allowlisted", func(t *testing.T) {
		claims := googleIDToken(server.URL)
		claims["aud"] = "rogue-client-id"
		token := signGoogleJWT(t, priv, kid, claims)
		_, err := svc.VerifyGoogleIDToken(context.Background(), token)
		require.ErrorIs(t, err, ErrGoogleTokenInvalid)
	})

	t.Run("not configured", func(t *testing.T) {
		bare := NewService(nil, nil, nil, testSecret, "")
		token := signGoogleJWT(t, priv, kid, googleIDToken(server.URL))
		_, err := bare.VerifyGoogleIDToken(context.Background(), token)
		require.ErrorIs(t, err, ErrGoogleNotConfigured)
	})
}

// ─── Multi-tier contact claims (M-CIM) ──────────────────────────────────────

func TestAddContactTiers(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	svc := NewService(db, nil, nil, testSecret, "")
	ctx := context.Background()

	// A buyer customer claimed by phone OTP (email log-in customer scenario)
	var buyer int64
	res, err := db.Exec(`
		INSERT INTO authentication_customer
			(customer_id, customer_type, customer_phone, customer_email, radius_username, is_active)
		VALUES ('email-owner-1', 'individual', '+263771234567', 'owner@example.com', 'owner-1', TRUE)
	`)
	require.NoError(t, err)
	buyer, err = res.LastInsertId()
	require.NoError(t, err)

	// seed the OTP identitylink: phone is the login identity
	_, err = db.Exec(`
		INSERT INTO authentication_identitylink
			(customer_id, kind, value, verified_at, verified_via)
		VALUES (?, 'phone', '263771234567', NOW(), 'otp')
	`, buyer)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO authentication_identitylink
			(customer_id, kind, value, verified_at, verified_via)
		VALUES (?, 'email', 'owner@example.com', NOW(), 'otp')
	`, buyer)
	require.NoError(t, err)

	validated := ValidatedToken{
		Identity: Identity{Phone: "+263771234567", Email: "owner@example.com"},
		IssuedAt: time.Now().Unix(),
	}

	t.Run("add phone without verification (tier 'contact')", func(t *testing.T) {
		err := svc.AddContact(ctx, validated, "phone", "0712000111", false)
		require.NoError(t, err)
		var via string
		require.NoError(t, db.QueryRow(`
			SELECT verified_via FROM authentication_identitylink
			WHERE kind='phone' AND value='0712000111'
		`).Scan(&via))
		assert.Equal(t, ViaContact, via)

		// purchases claim through the exact identitylink row
		ids := svc.LinkedCustomerIDs(ctx, Identity{Phone: "+0712000111"})
		require.NotEmpty(t, ids)
		assert.Equal(t, buyer, ids[0])
	})

	t.Run("duplicate add on own row is a diagnosis-level upgrade (no dup row)", func(t *testing.T) {
		// second Add for same value must not duplicate the identitylink row
		require.NoError(t, svc.AddContact(ctx, validated, "phone", "0712000111", false))
		var count int
		require.NoError(t, db.QueryRow(`
			SELECT COUNT(*) FROM authentication_identitylink
			WHERE kind='phone' AND value='0712000111'
		`).Scan(&count))
		assert.Equal(t, 1, count)
	})

	t.Run("pixel-and-email normalization", func(t *testing.T) {
		err := svc.AddContact(ctx, validated, "email", "That.Beast@Example.COM", false)
		require.NoError(t, err)
		var count int
		require.NoError(t, db.QueryRow(`
			SELECT COUNT(*) FROM authentication_identitylink
			WHERE kind='email' AND value='that.beast@example.com'
		`).Scan(&count))
		assert.Equal(t, 1, count, "email stored lowercase normalized")
	})

	t.Run("collision: OTP claim on another customer refuses tryOver for purchase/otp tiers", func(t *testing.T) {
		var other int64
		otherRes, err := db.Exec(`
			INSERT INTO authentication_customer
				(customer_id, customer_type, customer_phone, radius_username, is_active)
			VALUES ('other-holder-1', 'individual', '+263711111111', '07711111111', TRUE)
		`)
		require.NoError(t, err)
		other, err = otherRes.LastInsertId()
		require.NoError(t, err)
		_, err = db.Exec(`
			INSERT INTO authentication_identitylink
			(customer_id, kind, value, verified_at, verified_via)
			VALUES (?, 'phone', '263711111111', NOW(), 'otp')
		`, other)
		require.NoError(t, err)

		// buyer's claim is otp-verified on the OTHER user — no overwrite unless takeOver
		err2 := svc.AddContact(ctx, validated, "phone", "+263711111111", false)
		assert.ErrorIs(t, err2, ErrContactInUse)
	})

	t.Run("other customer holds it — takeOver only from 'contact'", func(t *testing.T) {
		err := svc.AddContact(ctx, validated, "phone", "0772000111", true)
		require.NoError(t, err) // no prior claim: takeOver harmless
	})
}
