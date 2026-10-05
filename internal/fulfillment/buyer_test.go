package fulfillment

import (
	"context"
	"database/sql"
	"testing"

	notifymock "github.com/freeradius/payments-api/internal/notify/mock"
	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// EnsureBuyerCustomer (M-CIM): identitylink-first resolution, per-buyer
// dedupe, retro-linking and no-customer-for-contactless purchases.
func TestEnsureBuyerCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	svc := NewService(db, notifymock.NewMockProvider())
	ctx := context.Background()
	noopOrg := sql.NullInt64{}

	req := func(txID int64, phone, email string) FulfillRequest {
		return FulfillRequest{TransactionID: txID, CustomerPhone: phone, CustomerEmail: email, FulfillmentKind: "voucher"}
	}

	t.Run("creates buyer once and links contact", func(t *testing.T) {
		id, err := svc.EnsureBuyerCustomer(ctx, req(101, "+263771234567", ""), noopOrg)
		require.NoError(t, err)
		require.NotZero(t, id)

		var kind, value string
		require.NoError(t, db.QueryRow(`
			SELECT kind, value FROM authentication_identitylink WHERE customer_id = ?
		`, id).Scan(&kind, &value))
		assert.Equal(t, "phone", kind)
		assert.Equal(t, "263771234567", value)
	})

	t.Run("reuses the same buyer across contact formats", func(t *testing.T) {
		id, err := svc.EnsureBuyerCustomer(ctx, req(102, "0771234567", ""), noopOrg)
		require.NoError(t, err)
		require.NotZero(t, id)

		id2, err := svc.EnsureBuyerCustomer(ctx, req(103, "+263771234567", ""), noopOrg)
		require.NoError(t, err)
		// Both digits normalize identically → identitylink match on 263…
		assert.Equal(t, id, id2, "identitylink-first bridges local vs E.164 formats")
	})

	t.Run("owner resolution wins over the legacy column match", func(t *testing.T) {
		id, err := svc.EnsureBuyerCustomer(ctx, req(104, "+263771234567", ""), noopOrg)
		require.NoError(t, err)
		require.NotZero(t, id)

		// Legacy customer row (minted by pre-M-CIM fulfillments) carrying the
		// same phone must NOT become the buyer's owner once a link exists.
		id2, err := svc.EnsureBuyerCustomer(ctx, req(105, "+263771234567", ""), noopOrg)
		require.NoError(t, err)
		assert.Equal(t, id, id2)

		// A legacy phone-matching row is upgraded with an identitylink.
		_, err = db.Exec(`
			INSERT INTO authentication_customer (customer_id, customer_type, customer_phone, radius_username)
			VALUES ('legacy-x', 'voucher', '0771234567', '0771234567')
		`)
		require.NoError(t, err)
		id3, err := svc.EnsureBuyerCustomer(ctx, req(107, "+263771234567", ""), noopOrg)
		require.NoError(t, err)
		assert.Equal(t, id, id3, "identitylink keeps winning after legacy rows exist")

		var links int
		require.NoError(t, db.QueryRow(`
			SELECT COUNT(*) FROM authentication_identitylink WHERE kind='phone' AND value='263771234567'
		`).Scan(&links))
		assert.Equal(t, 1, links, "no duplicate identitylink rows for the same value")
	})

	t.Run("linkless purchase stays anonymous (zero customer)", func(t *testing.T) {
		id, err := svc.EnsureBuyerCustomer(ctx, req(106, "", ""), noopOrg)
		require.NoError(t, err)
		assert.Zero(t, id, "contact-less purchase: no customer row minted")
	})

	t.Run("referrer agent recorded for new buyers", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO sales_agentprofile (id, status) VALUES (7, 'active')`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO sales_referrallink (agent_id, code, is_active) VALUES (7, 'AGENT-TEST-7', TRUE)`)
		require.NoError(t, err)

		ref := req(108, "+263779998888", "")
		ref.ReferralCode = "AGENT-TEST-7"
		id, err := svc.EnsureBuyerCustomer(ctx, ref, noopOrg)
		require.NoError(t, err)
		require.NotZero(t, id)

		var agentID int64
		require.NoError(t, db.QueryRow(`
			SELECT referred_by_id FROM authentication_customer WHERE id = ?
		`, id).Scan(&agentID))
		require.Equal(t, int64(7), agentID)
	})
}

// Ownership resolution must follow the identitylink verification tier: the
// unique(kind, value) key means a human usually has one row per stored
// format (local vs E.164), and when those rows disagree an OTP-proven link
// must win over a link minted from a typed checkout contact. A lone weak
// link still resolves when no stronger claim exists.
func TestEnsureBuyerCustomerTierOrdering(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	svc := NewService(db, notifymock.NewMockProvider())
	ctx := context.Background()
	noopOrg := sql.NullInt64{}

	for i, row := range []struct {
		id       int
		customer string
		value    string
		via      string
		verified string
	}{
		{1, "weak-owner", "07716543210", "contact", "2026-01-01 10:00:00.000000"},
		{2, "otp-owner", "2637716543210", "otp", "2026-02-01 10:00:00.000000"},
	} {
		_, err := db.Exec(`
			INSERT INTO authentication_customer (id, customer_id, customer_type, customer_phone, radius_username)
			VALUES (?, ?, 'voucher', ?, ?)`,
			row.id, row.customer, row.customer, row.customer)
		require.NoError(t, err, "seed customer %d", i)
		_, err = db.Exec(`
			INSERT INTO authentication_identitylink
				(customer_id, kind, value, verified_at, verified_via, created_at, updated_at)
			VALUES (?, 'phone', ?, ?, ?, NOW(), NOW())`,
			row.id, row.value, row.verified, row.via)
		require.NoError(t, err, "seed link %d", i)
	}

	// Presented as the local number: the exact-value row is the weak contact
	// link, while the OTP-proven row only matches through the trailing-9
	// bridge — proven ownership must still win.
	got, err := svc.EnsureBuyerCustomer(ctx, FulfillRequest{
		TransactionID:   501,
		CustomerPhone:   "07716543210",
		FulfillmentKind: "voucher",
	}, noopOrg)
	require.NoError(t, err)
	require.Equal(t, int64(2), got, "OTP-proven link must outrank the exact-value weak link")

	_, err = db.Exec(`
		INSERT INTO authentication_customer (id, customer_id, customer_type, customer_phone, radius_username)
		VALUES (3, 'weak-only', 'voucher', '07718887777', '07718887777')`)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO authentication_identitylink
			(customer_id, kind, value, verified_at, verified_via, created_at, updated_at)
		VALUES (3, 'phone', '263778887777', '2026-01-01 10:00:00.000000', 'contact', NOW(), NOW())`)
	require.NoError(t, err)

	got, err = svc.EnsureBuyerCustomer(ctx, FulfillRequest{
		TransactionID:   502,
		CustomerPhone:   "+263778887777",
		FulfillmentKind: "voucher",
	}, noopOrg)
	require.NoError(t, err)
	require.Equal(t, int64(3), got, "a lone weak link still resolves when no stronger claim exists")
}
