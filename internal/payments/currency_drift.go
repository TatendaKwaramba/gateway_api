package payments

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/freeradius/payments-api/internal/gateways"
)

// CheckGatewayCurrencyDrift compares each registered adapter's
// SupportedCurrencies() capability floor against the operational currency
// links in payments_gatewaysupportedcurrency.
//
// The two caches are deliberately separate data sources (adapter = vendor
// rails capability, code-owned and release-gated; DB = admin-editable
// business policy). Divergence is made *inert* by the initiate guard and
// the currency filters; this check just surfaces it for cleanup:
// a WARN log per mismatch. No mutation.
func CheckGatewayCurrencyDrift(ctx context.Context, db *sql.DB, registry *gateways.Registry) error {
	if db == nil || registry == nil {
		return nil
	}

	// DB links per gateway code.
	rows, err := db.QueryContext(ctx, `
		SELECT pg.gateway_code, sc.code
		FROM payments_gatewaysupportedcurrency gsc
		JOIN payments_paymentgateway pg ON pg.id = gsc.gateway_id
		JOIN services_supportedcurrency sc ON sc.id = gsc.currency_id
		WHERE gsc.is_active = 1 AND sc.is_active = 1
	`)
	if err != nil {
		// Drift visibility is best-effort; a schema error must not block boot.
		slog.Warn("currency drift check skipped (query failed)", slog.Any("error", err))
		return nil
	}
	defer rows.Close()

	dbLinks := map[string]map[string]bool{}
	scanned := false
	for rows.Next() {
		scanned = true
		var gatewayCode, currencyCode string
		if err := rows.Scan(&gatewayCode, &currencyCode); err != nil {
			continue
		}
		if dbLinks[gatewayCode] == nil {
			dbLinks[gatewayCode] = map[string]bool{}
		}
		dbLinks[gatewayCode][strings.ToUpper(currencyCode)] = true
	}
	if err := rows.Err(); err != nil {
		slog.Warn("currency drift check failed while iterating", slog.Any("error", err))
		return nil
	}
	if !scanned {
		return nil // no links seeded yet (fresh DB) — nothing to compare
	}

	for _, g := range registry.List() {
		adapterCurrencies := make([]string, 0, len(g.SupportedCurrencies()))
		adapterSet := map[string]bool{}
		for _, c := range g.SupportedCurrencies() {
			adapterSet[strings.ToUpper(c)] = true
			adapterCurrencies = append(adapterCurrencies, strings.ToUpper(c))
		}
		sort.Strings(adapterCurrencies)

		linkSet := dbLinks[g.Code()]

		var stale []string // DB links the adapter cannot charge
		for code := range linkSet {
			if !adapterSet[code] {
				stale = append(stale, code)
			}
		}
		sort.Strings(stale)

		var missing []string // adapter currencies with no DB link (not visible to checkout)
		for _, code := range g.SupportedCurrencies() {
			upper := strings.ToUpper(code)
			if !linkSet[upper] {
				missing = append(missing, upper)
			}
		}
		sort.Strings(missing)

		if len(stale) == 0 && len(missing) == 0 {
			continue
		}

		if len(stale) > 0 {
			slog.Warn("gateway currency drift: DB links the gateway to currencies the adapter cannot charge (stale rows are inert: initiate guard + listing filters; run seed_gateway_currencies to prune)",
				slog.String("gateway", g.Code()),
				slog.String("adapter_currencies", fmt.Sprint(adapterCurrencies)),
				slog.String("stale_db_links", fmt.Sprint(stale)),
			)
		}
		if len(missing) > 0 {
			slog.Warn("gateway currency drift: adapter currencies with no DB link (not offered at checkout; run seed_gateway_currencies to link)",
				slog.String("gateway", g.Code()),
				slog.String("adapter_currencies", fmt.Sprint(adapterCurrencies)),
				slog.String("missing_db_links", fmt.Sprint(missing)),
			)
		}
	}
	return nil
}
