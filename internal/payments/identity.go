package payments

// IdentityContact is the logged-in buyer's verified contact (M-CIM). When
// present at initiate time it drives:
//  - transaction.customer_id stamping (ownership, no digits-matching later)
//  - customer_phone/email autofill from verified contacts when the checkout
//    form omits them (gift contact ≠ buyer contact ⇒ both kept: the typed
//    contact still receives the SMS/PIN; ownership stays identity-customer)
//
// Anonymous purchases keep working exactly as before (Owner* fields nil).

// IdentityContact is the verified buyer contact passed with the token.
type IdentityContact struct {
	Phone string
	Email string
}

// Empty reports whether the contacts are both absent
func (c IdentityContact) Empty() bool { return c.Phone == "" && c.Email == "" }
