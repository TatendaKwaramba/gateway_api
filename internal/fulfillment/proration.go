package fulfillment

// ComputeProration returns the mid-cycle plan-change math shared by the
// adjustment quote endpoint (payments service) and the adjustment fulfillment
// path, so a quote shown to the customer and the guard applied at fulfillment
// can never drift apart. All prices are minor currency units; the returned
// Credit/Charge/Net are floats in minor units (DB decimal columns are
// scanned/inserted the same way).
//
// Net is floored at 0: unused credit on a downgrade is lost (no refunds).
func ComputeProration(oldPriceMinor, newPriceMinor int64, billingDays, daysRemaining int) (credit, charge, net float64) {
	if billingDays <= 0 {
		billingDays = 30
	}
	if daysRemaining < 0 {
		daysRemaining = 0
	}
	credit = float64(oldPriceMinor) * float64(daysRemaining) / float64(billingDays)
	charge = float64(newPriceMinor) * float64(daysRemaining) / float64(billingDays)
	net = charge - credit
	if net < 0 {
		net = 0
	}
	return credit, charge, net
}
