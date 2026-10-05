package identity

import (
	"context"
	"testing"

	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M-CIM sign-up: a verified identity with no customer behind it gets one
// (customer row + identitylink) instead of staying ownerless until purchase.
func TestSignupCreatesCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	svc := NewService(db, nil, nil, testSecret, "")
	ctx := context.Background()

	t.Run("fresh email sign-up creates customer and otp link", func(t *testing.T) {
		ident := Identity{Email: "new.user@example.com"}

		id, err := svc.EnsureSelfCustomer(ctx, ident)
		require.NoError(t, err)
		require.NotZero(t, id)

		var customerCode, customerType, radiusUsername, email string
		require.NoError(t, db.QueryRow(`
			SELECT customer_id, customer_type, radius_username, customer_email
			FROM authentication_customer WHERE id = ?
		`, id).Scan(&customerCode, &customerType, &radiusUsername, &email))
		assert.Equal(t, "subscriber", customerType)
		assert.Equal(t, customerCode, radiusUsername, "radius username mirrors the customer code")
		assert.Equal(t, "new.user@example.com", email)
		assert.Contains(t, customerCode, "app-", "codes are namespaced to the app signup path")

		again, err := svc.EnsureSelfCustomer(ctx, ident)
		require.NoError(t, err)
		assert.Equal(t, id, again, "sign-up is idempotent per contact")

		svc.RegisterVerifiedContact(ctx, ident, ViaOtp)
		var linkCustomer int64
		var via string
		require.NoError(t, db.QueryRow(`
			SELECT customer_id, verified_via FROM authentication_identitylink
			WHERE kind = 'email' AND value = 'new.user@example.com'
		`).Scan(&linkCustomer, &via))
		assert.Equal(t, id, linkCustomer)
		assert.Equal(t, ViaOtp, via)

		var count int
		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM authentication_customer WHERE id = ?`, id).Scan(&count))
		assert.Equal(t, 1, count)
	})

	t.Run("phone sign-up adopts the pre-existing buyer row", func(t *testing.T) {
		res, err := db.Exec(`
			INSERT INTO authentication_customer
				(customer_id, customer_type, customer_name, customer_email, customer_phone,
				 radius_username, is_active)
			VALUES ('legacy-phone-owner', 'subscriber', 'Legacy', 'legacy@example.com',
				'+263771111222', 'legacy-phone-owner', TRUE)
		`)
		require.NoError(t, err)
		legacyID, err := res.LastInsertId()
		require.NoError(t, err)

		ident := Identity{Phone: "+263771111222"}
		id, err := svc.EnsureSelfCustomer(ctx, ident)
		require.NoError(t, err)
		assert.Equal(t, legacyID, id, "existing customer wins over creating a new one")

		svc.RegisterVerifiedContact(ctx, ident, ViaOtp)
		var via string
		require.NoError(t, db.QueryRow(`
			SELECT verified_via FROM authentication_identitylink
			WHERE kind = 'phone' AND value = '263771111222'
		`).Scan(&via))
		assert.Equal(t, ViaOtp, via)
	})

	t.Run("self-added contact lands on a signup customer", func(t *testing.T) {
		ident := Identity{Email: "ownerless@example.com"}
		require.NoError(t, svc.AddContact(ctx, ValidatedToken{Identity: ident}, "phone", "0712345678", false))

		ids := svc.LinkedCustomerIDs(ctx, ident)
		require.Len(t, ids, 1, "adding a contact materializes the customer first")

		var via string
		require.NoError(t, db.QueryRow(`
			SELECT verified_via FROM authentication_identitylink
			WHERE kind = 'phone' AND value = '0712345678'
		`).Scan(&via))
		assert.Equal(t, ViaContact, via)
	})

	t.Run("purchase-tier claim of another customer is not stolen", func(t *testing.T) {
		res, err := db.Exec(`
			INSERT INTO authentication_customer
				(customer_id, customer_type, customer_phone, radius_username, is_active)
			VALUES ('other-buyer', 'subscriber', '0777777777', 'other-buyer', TRUE)
		`)
		require.NoError(t, err)
		otherID, err := res.LastInsertId()
		require.NoError(t, err)
		_, err = db.Exec(`
			INSERT INTO authentication_identitylink
				(customer_id, kind, value, verified_at, verified_via, created_at, updated_at)
			VALUES (?, 'phone', '0777777777', NOW(), 'purchase', NOW(), NOW())
		`, otherID)
		require.NoError(t, err)

		ident := Identity{Email: "thief@example.com"}
		err = svc.AddContact(ctx, ValidatedToken{Identity: ident}, "phone", "0777777777", false)
		assert.ErrorIs(t, err, ErrContactInUse)
	})
}
