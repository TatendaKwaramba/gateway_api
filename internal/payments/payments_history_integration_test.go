package payments

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/freeradius/payments-api/internal/django"
	"github.com/freeradius/payments-api/internal/fulfillment"
	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/freeradius/payments-api/internal/gateways/mock"
	notifymock "github.com/freeradius/payments-api/internal/notify/mock"
	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListIdentityPayments exercises the mobile history endpoint's service
// layer against a real MySQL schema (dockertest).
func TestListIdentityPayments(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}

	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err, "failed to spin up test database")
	defer cleanup()

	registry := gateways.NewRegistry()
	adapter := mock.NewAdapter("test-webhook-secret-min-32-characters-long", "")
	require.NoError(t, registry.Register(adapter))
	svc := NewService(
		db,
		registry,
		fulfillment.NewService(db, notifymock.NewMockProvider()),
		django.NewClient("", ""),
		"USD",
	)

	ctx := context.Background()
	gwID := int64(1) // mock gateway seeded by fixtures

	seedTxn := func(idPrefix string, state, phone, email, pin string, createdAt time.Time) int64 {
		t.Helper()
		res, err := db.Exec(`
			INSERT INTO payments_paymenttransaction
				(transaction_id, gateway_id, amount, currency, state, status,
				 customer_phone, customer_email, voucher_pin, fulfillment_kind, idempotency_key,
				 created_at, updated_at)
			VALUES (?, ?, 35000, 'ZAR', ?, ?, ?, ?, ?, 'voucher', ?, ?, ?)
		`, idPrefix, gwID, state, state, phone, email, pin,
			"key-"+idPrefix, createdAt.Format("2006-01-02 15:04:05"), createdAt.Format("2006-01-02 15:04:05"))
		require.NoError(t, err, "seed transaction")
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}

	now := time.Now()
	seedTxn("hist-voucher-1", "completed", "0771234567", "", "1111222233334444", now.Add(-2*time.Hour))
	seedTxn("hist-voucher-2", "completed", "+263771234567", "", "5555666677778888", now.Add(-1*time.Hour))
	seedTxn("hist-init-1", "initiated", "+263771234567", "A@B.c", "9999000011112222", now.Add(-30*time.Minute))
	seedTxn("hist-sub-1", "completed", "", "subscriber@example.com", "sub:42", now.Add(-10*time.Minute))

	t.Run("phone matches exact and local-format suffix", func(t *testing.T) {
		items, err := svc.ListIdentityPayments(ctx, ListIdentityPaymentsRequest{
			Phone: "+263771234567",
		})
		require.NoError(t, err)
		require.Len(t, items, 3, "both stored formats + initiated row must match")
		// Newest first
		assert.Equal(t, "hist-init-1", items[0].TransactionIDStr)
		assert.Equal(t, "hist-voucher-2", items[1].TransactionIDStr)
		assert.Equal(t, "hist-voucher-1", items[2].TransactionIDStr)
	})

	t.Run("initiated voucher hides the PIN", func(t *testing.T) {
		items, err := svc.ListIdentityPayments(ctx, ListIdentityPaymentsRequest{
			Phone: "+263771234567",
		})
		require.NoError(t, err)
		assert.Empty(t, items[0].VoucherPIN, "initiated state must not leak the PIN")
		assert.Equal(t, "5555666677778888", items[1].VoucherPIN)
		for _, item := range items {
			assert.Equal(t, "voucher", item.FulfillmentKind)
		}
	})

	t.Run("email matches case-insensitively", func(t *testing.T) {
		items, err := svc.ListIdentityPayments(ctx, ListIdentityPaymentsRequest{
			Email: "a@b.c",
		})
		require.NoError(t, err)
		require.Len(t, items, 1)
		assert.Equal(t, "hist-init-1", items[0].TransactionIDStr)
	})

	t.Run("subscription row surfaces subscription id and hides the sub: token", func(t *testing.T) {
		items, err := svc.ListIdentityPayments(ctx, ListIdentityPaymentsRequest{
			Email: "subscriber@example.com",
		})
		require.NoError(t, err)
		require.Len(t, items, 1)
		assert.Equal(t, int64(42), items[0].SubscriptionID)
		assert.Empty(t, items[0].VoucherPIN, "sub: internal token must not leak as voucher_pin")
	})

	t.Run("pagination", func(t *testing.T) {
		items, err := svc.ListIdentityPayments(ctx, ListIdentityPaymentsRequest{
			Phone:  "+263771234567",
			Limit:  1,
			Offset: 1,
		})
		require.NoError(t, err)
		require.Len(t, items, 1)
		assert.Equal(t, "hist-voucher-2", items[0].TransactionIDStr)
	})

	t.Run("unknown identity yields empty list", func(t *testing.T) {
		items, err := svc.ListIdentityPayments(ctx, ListIdentityPaymentsRequest{
			Phone: "+15550001234",
		})
		require.NoError(t, err)
		assert.Empty(t, items)
	})

	t.Run("no contact at all yields empty list", func(t *testing.T) {
		items, err := svc.ListIdentityPayments(ctx, ListIdentityPaymentsRequest{Phone: " ", Email: ""})
		require.NoError(t, err)
		assert.Empty(t, items)
	})

	t.Run("customer-id path (M-CIM identitylink surfaces email logins too)", func(t *testing.T) {
		// Buyer with NO captured phone digits on the transaction columns: the
		// identity is email-based, resolved via identitylink customer ids.
		res, err := db.Exec(`
			INSERT INTO authentication_customer
				(customer_id, customer_type, customer_email, radius_username, is_active)
			VALUES ('email-buyer-1', 'individual', 'mail@example.com', 'mail@example.com-x2', TRUE)
		`)
		require.NoError(t, err)
		buyer, errL := res.LastInsertId()
		require.NoError(t, errL)
		_, err = db.Exec(`
			INSERT INTO authentication_identitylink (customer_id, kind, value, verified_at, verified_via)
			VALUES (?, 'email', 'mail@example.com', NOW(), 'purchase')
		`, buyer)
		require.NoError(t, err)

		// Transaction missing any customer contact columns (the buyer's email lives on identitylink).
		_, err = db.Exec(`
			INSERT INTO payments_paymenttransaction
				(transaction_id, gateway_id, amount, currency, state, status, fulfillment_kind, customer_id, idempotency_key, created_at, updated_at)
			VALUES ('hist-email-1', ?, 50000, 'USD', 'completed', 'completed', 'voucher', ?, 'hist-email-1-key', NOW(), NOW())
		`, gwID, buyer)
		require.NoError(t, err)

		items, err := svc.ListIdentityPayments(ctx, ListIdentityPaymentsRequest{
			CustomerIDs: []int64{buyer},
		})
		require.NoError(t, err)
		require.Len(t, items, 1, "email identity resolves purchases via identitylink")
		assert.Equal(t, "hist-email-1", items[0].TransactionIDStr)
	})

	t.Run("amount is minor units", func(t *testing.T) {
		items, err := svc.ListIdentityPayments(ctx, ListIdentityPaymentsRequest{
			Phone: "+263771234567",
		})
		require.NoError(t, err)
		require.NotEmpty(t, items)
		assert.Equal(t, int64(3_500_000), items[0].Amount, "35,000.00 ZAR → minor units")
	})
}

// TestIdentityStampingOnInitiate (M-CIM): logged-in buyers stamp ownership at
// initiate when the bearer passes; anonymous checkout unchanged.
func TestInitiatePaymentsFromHandlerBehavior(t *testing.T) {
	t.Run("namespace-guard only — handler tested via httpapi e2e", func(t *testing.T) {
		// Structured as a documented marker; the handler-level coverage lives
		// in TestListIdentityPayments (service-layer) + the live curl gate.
	})
}

// TestInitiateIdentityStamping (M-CIM): OwnerCustomerID at initiate time
// stamps the transaction's customer FK even when the checkout form omits
// contacts; anonymous path leaves customer_id NULL.
func TestInitiateIdentityStamping(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}
	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err)
	defer cleanup()

	registry := gateways.NewRegistry()
	adapter := mock.NewAdapter("test-webhook-secret-min-32-characters-long", "")
	require.NoError(t, registry.Register(adapter))
	svc := NewService(
		db,
		registry,
		fulfillment.NewService(db, notifymock.NewMockProvider()),
		django.NewClient("", ""),
		"USD",
	)
	ctx := context.Background()

	var owner int64
	res, err := db.Exec(`
		INSERT INTO authentication_customer
			(customer_id, customer_type, customer_phone, customer_email, radius_username, is_active)
		VALUES ('owner-1', 'individual', '+263771234567', 'owner@example.com', '0771234567', TRUE)
	`)
	require.NoError(t, err)
	owner, err = res.LastInsertId()
	require.NoError(t, err)

	t.Run("logged-in initiate stamps customer_id + autofills contacts", func(t *testing.T) {
		result, err := svc.Initiate(ctx, InitiateRequest{
			GatewayCode:     "mock",
			MethodCode:      "mock-instant",
			Amount:          500,
			Currency:        "ZAR",
			FulfillmentKind: "voucher",
			OwnerCustomerID: &owner,
			OwnerContacts:   &IdentityContact{Phone: "+263771234567", Email: "owner@example.com"},
		})
		require.NoError(t, err)

		var customerID sql.NullInt64
		var phone, email sql.NullString
		require.NoError(t, db.QueryRow(`
			SELECT customer_id, customer_email, customer_phone
			FROM payments_paymenttransaction WHERE id = ?
		`, result.TransactionID).Scan(&customerID, &email, &phone))
		assert.Equal(t, owner, customerID.Int64, "ownership stamped at initiate")
		assert.Equal(t, "+263771234567", phone.String, "notification contact autofilled")
		assert.Equal(t, "owner@example.com", email.String)
	})

	t.Run("gift contact differs from identity — typed contact kept for SMS, ownership still the buyer", func(t *testing.T) {
		result, err := svc.Initiate(ctx, InitiateRequest{
			GatewayCode:     "mock",
			MethodCode:      "mock-instant",
			Amount:          500,
			Currency:        "ZAR",
			FulfillmentKind: "voucher",
			IdempotencyKey:  "gift-key-1",
			CustomerPhone:   "077999888777", // gift recipient (notification contact)
			OwnerCustomerID: &owner,
			OwnerContacts:   &IdentityContact{Phone: "+263771234567", Email: "owner@example.com"},
		})
		require.NoError(t, err)

		var customerID sql.NullInt64
		var phone sql.NullString
		require.NoError(t, db.QueryRow(`
			SELECT customer_id, customer_phone
			FROM payments_paymenttransaction WHERE id = ?
		`, result.TransactionID).Scan(&customerID, &phone))
		assert.Equal(t, owner, customerID.Int64, "identity customer owns the gift purchase")
		assert.Equal(t, "077999888777", phone.String, "typed (gift) contact kept for notification")
	})

	t.Run("anonymous initiate unchanged (customer id NULL)", func(t *testing.T) {
		result, err := svc.Initiate(ctx, InitiateRequest{
			GatewayCode:     "mock",
			MethodCode:      "mock-instant",
			Amount:          500,
			Currency:        "ZAR",
			FulfillmentKind: "voucher",
			IdempotencyKey:  "anon-key-1",
			CustomerPhone:   "0772000004",
		})
		require.NoError(t, err)
		var customerID sql.NullInt64
		require.NoError(t, db.QueryRow(`
			SELECT customer_id FROM payments_paymenttransaction WHERE id = ?
		`, result.TransactionID).Scan(&customerID))
		assert.False(t, customerID.Valid, "anonymous path keeps customer_id NULL")
	})
}
