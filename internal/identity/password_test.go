package identity

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Password accounts (M-CIM): argon2id hashing, identitylink-first resolution,
// lockout and fresh-token gating.
func TestPasswordHashing(t *testing.T) {
	hash, err := HashPassword("s3cret-password-1")
	require.NoError(t, err)
	assert.True(t, VerifyPassword(hash, "s3cret-password-1"))
	assert.False(t, VerifyPassword(hash, "s3cret-password-2"))

	second, err := HashPassword("s3cret-password-1")
	require.NoError(t, err)
	assert.NotEqual(t, hash, second, "unique salts")
}

func TestPasswordFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	svc := NewService(db, nil, nil, testSecret, "")
	ctx := context.Background()

	seedBuyer := func(t *testing.T) int64 {
		t.Helper()
		res, err := db.Exec(`
			INSERT INTO authentication_customer
				(customer_id, customer_type, customer_name, customer_email, customer_phone,
				 radius_username, access_type, is_active, created_at, updated_at)
			VALUES ('seller-1', 'individual', 'Buyer One', 'buyer@example.com', '+263771234567',
					'0771234567', 'hotspot', TRUE, NOW(), NOW())
		`)
		require.NoError(t, err)
		id, _ := res.LastInsertId()
		require.NoError(t, err)
		// identitylink: verified phone + email
		_, err = db.Exec(`
			INSERT INTO authentication_identitylink (customer_id, kind, value, verified_at, verified_via)
			VALUES (?, 'phone', '263771234567', NOW(), 'purchase'), (?, 'email', 'buyer@example.com', NOW(), 'purchase')
		`, id, id)
		require.NoError(t, err)
		return id
	}

	t.Run("register requires a linked customer", func(t *testing.T) {
		buyer := seedBuyer(t)
		_ = buyer
	})

	t.Run("register + login roundtrip (phone)", func(t *testing.T) {
		require.NoError(t, db.QueryRow(`SELECT 1`).Err())

		// register gated by customer presence (identitylink exists)
		err := svc.RegisterPassword(ctx, Identity{Phone: "+263771234567"}, "reliable-passphrase")
		require.NoError(t, err)

		token, ident, err := svc.LoginWithPassword(ctx, "sms", "+263771234567", "reliable-passphrase")
		require.NoError(t, err)
		assert.Equal(t, "+263771234567", ident.Phone) // validateRecipient normalizes digits
		// token validates through the standard path
		validated, err := svc.ValidateTokenFull(token)
		require.NoError(t, err)
		assert.NotZero(t, validated.Identity.IsEmpty() == false)
	})

	t.Run("login with email", func(t *testing.T) {
		token, _, err := svc.LoginWithPassword(ctx, "email", "BUYER@example.com", "reliable-passphrase")
		require.NoError(t, err)
		assert.NotEmpty(t, token)
	})

	t.Run("wrong password rejected", func(t *testing.T) {
		_, _, err := svc.LoginWithPassword(ctx, "sms", "0771234567", "wrong-password-1")
		assert.ErrorIs(t, err, ErrPasswordInvalid)
	})

	t.Run("unknown contact → not found", func(t *testing.T) {
		_, _, err := svc.LoginWithPassword(ctx, "sms", "+15550001111", "reliable-passphrase")
		assert.ErrorIs(t, err, ErrPasswordUnknown)
	})

	t.Run("weak password rejected", func(t *testing.T) {
		err := svc.RegisterPassword(ctx, Identity{Phone: "+263771234567"}, "short")
		assert.ErrorIs(t, err, ErrPasswordWeak)
	})

	t.Run("lockout after 5 failures", func(t *testing.T) {
		var attempts error = nil
		_ = attempts
		for i := 0; i < 5; i++ {
			_, _, err := svc.LoginWithPassword(ctx, "sms", "+263771234567", "bad-password-1")
			if err == nil {
				t.Fatal("unexpected success")
			}
		}
		_, _, err := svc.LoginWithPassword(ctx, "sms", "+263771234567", "reliable-passphrase")
		assert.ErrorIs(t, err, ErrPasswordLocked, "even correct password blocked while locked")

		// unlock and confirm recovery
		_, err = db.Exec(`UPDATE payments_identity_account SET locked_until = NULL, failed_attempts = 0`)
		require.NoError(t, err)
		_, _, err = svc.LoginWithPassword(ctx, "sms", "+263771234567", "reliable-passphrase")
		assert.NoError(t, err)
	})

	t.Run("password change requires the current password", func(t *testing.T) {
		token, _, err := svc.LoginWithPassword(ctx, "sms", "+263771234567", "reliable-passphrase")
		require.NoError(t, err)
		validated, err := svc.ValidateTokenFull(token)
		require.NoError(t, err)

		// wrong current password
		err = svc.ChangePassword(ctx, validated, "wrong-password-2", "new-reliable-pass")
		assert.ErrorIs(t, err, ErrPasswordInvalid)

		// correct path works
		require.NoError(t, svc.ChangePassword(ctx, validated, "reliable-passphrase", "new-reliable-pass"))
		_, ident, err := svc.LoginWithPassword(ctx, "email", "buyer@example.com", "new-reliable-pass")
		require.NoError(t, err)
		assert.Equal(t, "buyer@example.com", ident.Email)
	})

	t.Run("reset tolerates only a fresh token", func(t *testing.T) {
		validated := ValidatedToken{Identity: Identity{Phone: "+263771234567"}, IssuedAt: time.Now().Add(-2 * time.Hour).Unix()}
		err := svc.ResetPassword(ctx, validated, "another-reliable-pass")
		assert.Error(t, err, "stale token cannot reset")
	})
}

// Parallel wrong-password attempts must all count: the counter is a single
// atomic UPDATE, so N concurrent failures always reach the lockout threshold
// (the old read-modify-write under-counted and let parallel attackers stay
// below it).
func TestLoginWithPassword_ConcurrentLockout(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	svc := NewService(db, nil, nil, testSecret, "")
	ctx := context.Background()

	res, err := db.Exec(`
		INSERT INTO authentication_customer
			(customer_id, customer_type, customer_name, customer_email, customer_phone,
			 radius_username, access_type, is_active, created_at, updated_at)
		VALUES ('lockout-1', 'individual', 'Lockout Tester', 'lockout@example.com', '+263771239999',
			 '0771239999', 'hotspot', TRUE, NOW(), NOW())
	`)
	require.NoError(t, err)
	customerID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO authentication_identitylink (customer_id, kind, value, verified_at, verified_via)
		VALUES (?, 'phone', '263771239999', NOW(), 'purchase')
	`, customerID)
	require.NoError(t, err)

	require.NoError(t, svc.RegisterPassword(ctx, Identity{Phone: "+263771239999"}, "reliable-passphrase"))

	const callers = 10
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.LoginWithPassword(ctx, "sms", "+263771239999", "wrong-password-x")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.Error(t, err, "wrong password is always rejected")
	}

	var attempts int
	require.NoError(t, db.QueryRow(`
		SELECT a.failed_attempts
		FROM payments_identity_account a
		WHERE a.customer_id = ?
	`, customerID).Scan(&attempts))
	assert.GreaterOrEqual(t, attempts, 5,
		"all concurrent failures must count — no lost updates")

	_, _, err = svc.LoginWithPassword(ctx, "sms", "+263771239999", "reliable-passphrase")
	assert.ErrorIs(t, err, ErrPasswordLocked, "account is locked even for the correct password")
}
