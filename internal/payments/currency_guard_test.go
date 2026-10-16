package payments

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/freeradius/payments-api/internal/fulfillment"
	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/freeradius/payments-api/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubGateway implements gateways.Gateway with a configurable currency list.
type stubGateway struct {
	code       string
	currencies []string
}

func (g *stubGateway) Code() string { return g.code }

func (g *stubGateway) Capabilities() gateways.Capabilities {
	return gateways.Capabilities{SupportsRefund: false, RequiresPhone: true}
}

func (g *stubGateway) SupportedMethods() []gateways.Method {
	return []gateways.Method{{Code: g.code + "-main", DisplayName: "Stub", RequiresPhone: true}}
}

func (g *stubGateway) SupportedCurrencies() []string { return g.currencies }

func (g *stubGateway) ConfigSchema() string { return "" }

func (g *stubGateway) Initiate(ctx context.Context, req gateways.InitiateRequest) (gateways.InitiateResult, error) {
	return gateways.InitiateResult{ExternalReference: "stub-ref", State: "initiated"}, nil
}

func (g *stubGateway) Status(ctx context.Context, externalRef string) (gateways.StatusResult, error) {
	return gateways.StatusResult{State: "pending"}, nil
}

func (g *stubGateway) VerifyWebhook(ctx context.Context, headers http.Header, body []byte) (gateways.WebhookEvent, error) {
	return gateways.WebhookEvent{}, fmt.Errorf("not implemented")
}

func (g *stubGateway) Refund(ctx context.Context, externalRef string, amount gateways.Money) (gateways.RefundResult, error) {
	return gateways.RefundResult{}, fmt.Errorf("not implemented")
}

var _ gateways.Gateway = (*stubGateway)(nil)

// stubFulfillment keeps completed-webhook tests alive without a real
// fulfillment stack.
type stubFulfillment struct{}

func (stubFulfillment) Fulfill(ctx context.Context, req fulfillment.FulfillRequest) (*fulfillment.FulfillResult, error) {
	return &fulfillment.FulfillResult{VoucherPIN: "1111222233334444"}, nil
}

func (stubFulfillment) Rollback(ctx context.Context, voucherPIN string) error { return nil }

var _ FulfillmentService = stubFulfillment{}

// TestValidateGatewayCurrencyAdapterGuard locks the layer-2 initiate guard:
// a DB currency link that the registered adapter does not declare is
// rejected (vendor-rails capability floor), and an adapter-declared link
// passes.
func TestValidateGatewayCurrencyAdapterGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}

	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err, "spin up test DB")
	defer cleanup()

	// The seeded 'mock' gateway has DB links for ZAR + USD (testutil).
	// Stub adapter deliberately declares USD only — ZAR link is "stale".
	registry := gateways.NewRegistry()
	require.NoError(t, registry.Register(&stubGateway{code: "mock", currencies: []string{"USD"}}))

	svc := NewService(db, registry, nil, nil, "USD")
	ctx := context.Background()

	t.Run("stale db link rejected by adapter floor", func(t *testing.T) {
		err := svc.validateGatewayCurrency(ctx, "mock", "ZAR")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "adapter does not declare")
	})

	t.Run("adapter-declared link accepted", func(t *testing.T) {
		err := svc.validateGatewayCurrency(ctx, "mock", "USD")
		assert.NoError(t, err)
	})

	t.Run("db-layer rejection for unlinked currency", func(t *testing.T) {
		err := svc.validateGatewayCurrency(ctx, "mock", "GBP")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not supported for gateway")
	})
}

// TestListPaymentMethodsCurrencyFilter keeps the checkout method list
// currency-coherent: methods on gateways without the requested currency
// link are excluded.
func TestListPaymentMethodsCurrencyFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}

	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err, "spin up test DB")
	defer cleanup()

	// testutil seeds gateway 'mock' with methods + ZAR and USD links.
	svc := NewService(db, gateways.NewRegistry(), nil, nil, "USD")
	ctx := context.Background()

	t.Run("no filter returns methods", func(t *testing.T) {
		methods, err := svc.ListPaymentMethods(ctx, "", "")
		require.NoError(t, err)
		assert.NotEmpty(t, methods)
	})

	t.Run("linked currency returns methods", func(t *testing.T) {
		methods, err := svc.ListPaymentMethods(ctx, "mock", "ZAR")
		require.NoError(t, err)
		assert.NotEmpty(t, methods)
	})

	t.Run("linked currency case-insensitive", func(t *testing.T) {
		methods, err := svc.ListPaymentMethods(ctx, "mock", "usd")
		require.NoError(t, err)
		assert.NotEmpty(t, methods)
	})

	t.Run("unlinked currency returns none", func(t *testing.T) {
		methods, err := svc.ListPaymentMethods(ctx, "mock", "GBP")
		require.NoError(t, err)
		assert.Empty(t, methods)
	})
}

// TestProcessWebhookAmountMismatchGuard: a payment.completed webhook whose
// captured amount or currency differs from the transaction must NOT advance
// the state machine (completion held for reconciliation); a matching event
// completes normally.
func TestProcessWebhookAmountMismatchGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}

	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err, "spin up test DB")
	defer cleanup()

	registry := gateways.NewRegistry()
	svc := NewService(db, registry, stubFulfillment{}, nil, "USD")
	ctx := context.Background()

	seedTxn := func(t *testing.T, ref, amount, currency string) int64 {
		t.Helper()
		res, err := db.Exec(`
			INSERT INTO payments_paymenttransaction
				(transaction_id, gateway_id, external_reference, amount, currency, state, status,
				 fulfillment_kind, idempotency_key, created_at, updated_at)
			VALUES (?, 1, ?, ?, ?, 'initiated', 'initiated', 'voucher', ?, NOW(), NOW())
		`, "txn-"+ref, ref, amount, currency, "idem-"+ref)
		require.NoError(t, err, "seed transaction")
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}

	stateOf := func(t *testing.T, id int64) string {
		t.Helper()
		var state string
		require.NoError(t, db.QueryRow(`SELECT state FROM payments_paymenttransaction WHERE id = ?`, id).Scan(&state))
		return state
	}

	wrongAmountID := seedTxn(t, "guard-amt", "350.00", "ZAR")
	wrongCurrencyID := seedTxn(t, "guard-cur", "350.00", "ZAR")
	caseFoldID := seedTxn(t, "guard-case", "500.00", "USD")
	matchID := seedTxn(t, "guard-ok", "350.00", "ZAR")

	headers := http.Header{}

	t.Run("wrong amount is held", func(t *testing.T) {
		require.NoError(t, svc.ProcessWebhook(ctx, "mock", gateways.WebhookEvent{
			ExternalReference: "guard-amt",
			EventType:         EventTypePaymentCompleted,
			State:             "completed",
			Amount:            gateways.Money{Amount: 35100, Currency: "ZAR"},
		}, []byte(`{}`), headers))
		assert.Equal(t, "initiated", stateOf(t, wrongAmountID), "state must not advance on amount mismatch")
	})

	t.Run("amount ok but currency wrong is held", func(t *testing.T) {
		require.NoError(t, svc.ProcessWebhook(ctx, "mock", gateways.WebhookEvent{
			ExternalReference: "guard-cur",
			EventType:         EventTypePaymentCompleted,
			State:             "completed",
			Amount:            gateways.Money{Amount: 35000, Currency: "USD"},
		}, []byte(`{}`), headers))
		assert.Equal(t, "initiated", stateOf(t, wrongCurrencyID))
	})

	t.Run("lowercase currency matches", func(t *testing.T) {
		require.NoError(t, svc.ProcessWebhook(ctx, "mock", gateways.WebhookEvent{
			ExternalReference: "guard-case",
			EventType:         EventTypePaymentCompleted,
			State:             "completed",
			Amount:            gateways.Money{Amount: 50000, Currency: "usd"},
		}, []byte(`{}`), headers), "lowercase currency should match (EqualFold)")
		assert.NotEqual(t, "initiated", stateOf(t, caseFoldID))
	})

	t.Run("matching amount completes", func(t *testing.T) {
		require.NoError(t, svc.ProcessWebhook(ctx, "mock", gateways.WebhookEvent{
			ExternalReference: "guard-ok",
			EventType:         EventTypePaymentCompleted,
			State:             "completed",
			Amount:            gateways.Money{Amount: 35000, Currency: "ZAR"},
		}, []byte(`{}`), headers))
		assert.Equal(t, "completed", stateOf(t, matchID))
	})
}

// TestCheckGatewayCurrencyDrift locks the drift report's contract: WARN-only
// reporting, no DB mutation.
func TestCheckGatewayCurrencyDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("dockertest required")
	}

	db, cleanup, err := testutil.SetupTestDB()
	require.NoError(t, err, "spin up test DB")
	defer cleanup()

	registry := gateways.NewRegistry()
	// Seeded mock gateway has DB links ZAR + USD; the stub declares USD
	// only — ZAR is a stale link and (nothing) is missing.
	require.NoError(t, registry.Register(&stubGateway{code: "mock", currencies: []string{"USD"}}))

	require.NoError(t, CheckGatewayCurrencyDrift(context.Background(), db, registry))

	var stillActive int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM payments_gatewaysupportedcurrency WHERE gateway_id = 1 AND is_active = 1`,
	).Scan(&stillActive))
	assert.Equal(t, 2, stillActive, "drift check must be warn-only (no mutation)")
}
