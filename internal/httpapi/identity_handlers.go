package httpapi

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/freeradius/payments-api/internal/fulfillment"
	"github.com/freeradius/payments-api/internal/identity"
	"github.com/freeradius/payments-api/internal/payments"
)

// otpRequest is the body for POST /api/v1/identity/otp/request
type otpRequest struct {
	Channel   string `json:"channel"` // "sms" | "email"
	Recipient string `json:"recipient"`
}

// otpVerifyRequest is the body for POST /api/v1/identity/otp/verify
type otpVerifyRequest struct {
	Channel   string `json:"channel"`
	Recipient string `json:"recipient"`
	Code      string `json:"code"`
}

// googleVerifyRequest is the body for POST /api/v1/identity/google/verify
type googleVerifyRequest struct {
	IDToken string `json:"id_token"`
}

// revokeIdentityRequest is the body for POST /admin/api/identity/revoke
type revokeIdentityRequest struct {
	Phone  string `json:"phone"`
	Email  string `json:"email"`
	Reason string `json:"reason"`
}

// deviceRegistrationRequest is the body for POST /api/v1/devices
type deviceRegistrationRequest struct {
	ExpoToken string `json:"expo_token"`
	Platform  string `json:"platform"` // "ios" | "android" | "web"
}

// identityOTPRequestHandler issues an OTP code (POST /api/v1/identity/otp/request)
func (r *Router) identityOTPRequestHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}

	var body otpRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	expiresAt, err := r.identity.RequestOTP(req.Context(), body.Channel, body.Recipient)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrRateLimited):
			retryAfter := 3600
			var rateErr *identity.RateLimitedError
			if errors.As(err, &rateErr) {
				retryAfter = rateErr.RetryAfterSeconds()
			}
			respondJSON(w, http.StatusTooManyRequests, map[string]interface{}{
				"error":       err.Error(),
				"retry_after": retryAfter,
			})
		case errors.Is(err, identity.ErrInvalidRecipient), errors.Is(err, identity.ErrUnsupportedChannel):
			respondError(w, http.StatusBadRequest, err.Error())
		default:
			slog.Error("identity: otp request failed", slog.Any("error", err))
			respondError(w, http.StatusInternalServerError, "failed to send verification code")
		}
		return
	}

	respondJSON(w, http.StatusAccepted, map[string]interface{}{
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

// identityOTPVerifyHandler verifies the OTP and returns an identity token
// (POST /api/v1/identity/otp/verify)
func (r *Router) identityOTPVerifyHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}

	var body otpVerifyRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	token, ident, err := r.identity.VerifyOTP(req.Context(), body.Channel, body.Recipient, body.Code)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidCode),
			errors.Is(err, identity.ErrCodeExpired),
			errors.Is(err, identity.ErrTooManyAttempts),
			errors.Is(err, identity.ErrInvalidRecipient),
			errors.Is(err, identity.ErrUnsupportedChannel):
			respondError(w, http.StatusBadRequest, err.Error())
		default:
			slog.Error("identity: otp verify failed", slog.Any("error", err))
			respondError(w, http.StatusInternalServerError, "verification failed")
		}
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"token":     token,
		keyIdentity: ident,
	})
}

// identityBearer extracts and validates the identity token from the
// Authorization: Bearer header (identity + iat, rotation-aware)
func (r *Router) identityBearerFull(req *http.Request) (identity.ValidatedToken, error) {
	header := req.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return identity.ValidatedToken{}, identity.ErrInvalidToken
	}
	return r.identity.ValidateTokenFull(strings.TrimSpace(token))
}

// identityBearerRevocationChecked validates the token AND rejects revoked
// identities (any payment/usage/device surface).
func (r *Router) identityBearerRevocationChecked(w http.ResponseWriter, req *http.Request) (identity.ValidatedToken, bool) {
	validated, err := r.identityBearerFull(req)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "valid Bearer identity token required")
		return validated, false
	}
	revoked, err := r.identity.IdentityRevoked(req.Context(), validated.Identity, validated.IssuedAt)
	if err != nil {
		slog.Error("identity: revocation check failed", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, "identity lookup failed")
		return validated, false
	}
	if revoked {
		respondError(w, http.StatusUnauthorized, "identity revoked, sign in again")
		return validated, false
	}
	return validated, true
}

// identityListPaymentsHandler lists the signed-in customer's payments
// (GET /api/v1/payments). Requires a Bearer identity token; matches the
// phone/email embedded in the token against transaction customer contacts.
func (r *Router) identityListPaymentsHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}

	validated, ok := r.identityBearerRevocationChecked(w, req)
	if !ok {
		return
	}

	query := req.URL.Query()
	limit := 20
	if v, err := strconv.Atoi(query.Get("limit")); err == nil && v > 0 && v <= 100 {
		limit = v
	}
	offset := 0
	if v, err := strconv.Atoi(query.Get("offset")); err == nil && v >= 0 {
		offset = v
	}

	// M-CIM: historic identities (phone-linked buyers) surface email/Google
	// pulls through their identitylink customer references too.
	customerIDs := r.identity.LinkedCustomerIDs(req.Context(), validated.Identity)
	items, err := r.paymentService.ListIdentityPayments(req.Context(), payments.ListIdentityPaymentsRequest{
		Phone:       validated.Identity.Phone,
		Email:       validated.Identity.Email,
		CustomerIDs: customerIDs,
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		slog.Error("identity: payment history lookup failed", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, "failed to list payments")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"payments": items,
		"limit":    limit,
		"offset":   offset,
	})
}

// identityRevokeHandler revokes an identity contact point
// (POST /admin/api/identity/revoke — HMAC-signed internal route). Tokens
// issued before now stop validating everywhere; the customer re-verifies
// (OTP / Google) to recover.
func (r *Router) identityRevokeHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	var body revokeIdentityRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := r.identity.RevokeIdentity(req.Context(), body.Phone, body.Email, body.Reason, "admin-api"); err != nil {
		if strings.Contains(err.Error(), "required") {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.Error("identity: revocation failed", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, "revocation failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		keyStatus: "revoked",
	})
}

// googleVerifyHandler issues an identity token for a verified Google account
// (POST /api/v1/identity/google/verify). The app sends the raw id_token. The
// audience allowlist and email_verified are enforced server-side.
func (r *Router) googleVerifyHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}

	var body googleVerifyRequest
	if err := parseJSON(req, &body); err != nil || strings.TrimSpace(body.IDToken) == "" {
		respondError(w, http.StatusBadRequest, "id_token is required")
		return
	}

	ident, err := r.identity.VerifyGoogleIDToken(req.Context(), strings.TrimSpace(body.IDToken))
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrGoogleNotConfigured):
			respondError(w, http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, identity.ErrGoogleEmailUnverified):
			// M-Login decision: fall back to the email OTP flow — the client
			// shows the OTP channel with the Google account email prefilled.
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error":    err.Error(),
				keyEmail:   "", // token untrusted: email not exposed server-side
				"fallback": "email_otp",
			})
		case errors.Is(err, identity.ErrGoogleTokenInvalid):
			respondError(w, http.StatusBadRequest, err.Error())
		default:
			slog.Error("identity: google verify failed", slog.Any("error", err))
			respondError(w, http.StatusInternalServerError, "Google sign-in failed")
		}
		return
	}

	token, err := r.identity.IssueToken(ident)
	if err != nil {
		slog.Error("identity: google token issuance failed", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, "Google sign-in failed")
		return
	}
	r.identity.RegisterVerifiedContact(req.Context(), ident, identity.ViaGoogle) // M-CIM

	slog.Info("identity: google sign-in", slog.String(keyEmail, maskedGoogleEmail(ident.Email)))
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"token":     token,
		keyIdentity: ident,
	})
}

// identityMeHandler returns the current identity summary
// (GET /api/v1/identity/me, Bearer identity token).
func (r *Router) identityMeHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	validated, err := r.identityBearerFull(req)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "valid Bearer identity token required")
		return
	}
	revoked, err := r.identity.IdentityRevoked(req.Context(), validated.Identity, validated.IssuedAt)
	if err != nil {
		slog.Error("identity: revocation check failed", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, "identity lookup failed")
		return
	}
	if revoked {
		respondError(w, http.StatusUnauthorized, "identity revoked, sign in again")
		return
	}
	devices := r.identity.CountEnabledDevices(req.Context(), validated.Identity)
	contact := validated.Identity.Phone
	if contact == "" {
		contact = validated.Identity.Email
	}
	if validated.Identity.Phone != "" && validated.Identity.Email != "" {
		contact = validated.Identity.Email
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		keyPhone:       validated.Identity.Phone,
		keyEmail:       validated.Identity.Email,
		"contact":      contact,
		"issued_at":    validated.IssuedAt,
		"device_count": devices,
	})
}

// registerDeviceHandler registers/refreshes a mobile push device
// (POST /api/v1/devices). Requires a Bearer identity token.
func (r *Router) registerDeviceHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}

	validated, ok := r.identityBearerRevocationChecked(w, req)
	if !ok {
		return
	}
	ident := validated.Identity

	var body deviceRegistrationRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	body.ExpoToken = strings.TrimSpace(body.ExpoToken)
	if body.ExpoToken == "" || len(body.ExpoToken) > 255 {
		respondError(w, http.StatusBadRequest, "expo_token is required (max 255 chars)")
		return
	}
	platform := strings.ToLower(strings.TrimSpace(body.Platform))
	switch platform {
	case "ios", "android", "web":
	default:
		respondError(w, http.StatusBadRequest, "platform must be one of: ios, android, web")
		return
	}

	if err := r.identity.UpsertPushDevice(req.Context(), ident, body.ExpoToken, platform); err != nil {
		slog.Error("devices: registration failed", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, "failed to register device")
		return
	}

	slog.Info("devices: registered",
		slog.String("platform", platform),
		slog.Bool("has_phone", ident.Phone != ""),
		slog.Bool("has_email", ident.Email != ""),
	)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		keyStatus: "registered",
	})
}

// pushNotificationRequest is the body for POST /admin/api/notifications/push
// (internal — HMAC-signed via AdminHMACMiddleware; e.g. FUP threshold
// warnings emitted by flash-api).
type pushNotificationRequest struct {
	Phone string            `json:"phone"`
	Title string            `json:"title"`
	Body  string            `json:"body"`
	Data  map[string]string `json:"data,omitempty"`
}

// adminPushNotificationHandler sends a server-initiated push to all devices
// bound to a phone. Synchronous: the caller offers 2+ registered devices only
// rarely and the Expo API call is fast; failures are audited and non-fatal.
func (r *Router) adminPushNotificationHandler(w http.ResponseWriter, req *http.Request) {
	if r.fulfillmentSvc == nil {
		respondError(w, http.StatusServiceUnavailable, "push not configured")
		return
	}

	var body pushNotificationRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Data == nil {
		body.Data = map[string]string{}
	}

	if err := r.fulfillmentSvc.PushNotification(req.Context(), fulfillment.PushNotificationRequest{
		Phone: body.Phone,
		Title: body.Title,
		Body:  body.Body,
		Data:  body.Data,
	}); err != nil {
		if strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "no registered device") {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.Error("push notification delivery incomplete", slog.Any("error", err))
		// 202: the request is legitimate; at least one token failed tail-side.
		respondJSON(w, http.StatusAccepted, map[string]string{
			keyStatus: "partial",
			"detail":  err.Error(),
		})
		return
	}

	slog.Info("push: server-initiated notification sent",
		slog.String(keyPhone, body.Phone),
	)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		keyStatus: "sent",
	})
}

// maskedGoogleEmail masks an email for logs: ab***@domain
func maskedGoogleEmail(email string) string {
	parts := strings.SplitN(email, "@", 2)
	local := parts[0]
	if len(local) > 2 {
		local = local[:2] + "***"
	} else {
		local = "***"
	}
	if len(parts) == 2 {
		return local + "@" + parts[1]
	}
	return local
}

// identityProfileHandler — who am I + claimed customer + devices + recent
// purchases (GET /api/v1/identity/profile, Bearer identity token). The
// customer block is absent when the verified contact has no claimed
// buyer/customer yet (M-CIM retro-claim will attach one after the contact's
// first purchase — then keep the identitylink chain).
func (r *Router) identityProfileHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	validated, ok := r.identityBearerRevocationChecked(w, req)
	if !ok {
		return
	}

	// customer resolution mirrors the history endpoint
	customerIDs := r.identity.LinkedCustomerIDs(req.Context(), validated.Identity)
	customer := map[string]interface{}{}
	var primaryCustomerID int64
	if len(customerIDs) > 0 {
		primaryCustomerID = customerIDs[0]
	}
	if primaryCustomerID > 0 {
		var (
			customerCode   string
			customerType   string
			customerName   string
			customerEmail  string
			customerPhone  string
			radiusUsername string
			accessType     string
			orgID          sql.NullInt64
		)
		if err := r.paymentService.Database().QueryRowContext(req.Context(), `
			SELECT customer_id, customer_type, COALESCE(customer_name, ''),
			       COALESCE(customer_email, ''), COALESCE(customer_phone, ''),
			       COALESCE(radius_username, ''), access_type, organization_id
			FROM authentication_customer WHERE id = ?
		`, primaryCustomerID).Scan(&customerCode, &customerType, &customerName, &customerEmail,
			&customerPhone, &radiusUsername, &accessType, &orgID); err == nil {
			organization := ""
			if orgID.Valid {
				var orgName string
				if scanErr := r.paymentService.Database().QueryRowContext(req.Context(), `
					SELECT name FROM authentication_organization WHERE id = ?
				`, orgID.Int64).Scan(&orgName); scanErr == nil && orgName != "" {
					organization = orgName
				}
			}
			customer = map[string]interface{}{
				"id":              primaryCustomerID,
				"customer_id":     customerCode,
				"customer_type":   customerType,
				"name":            customerName,
				keyEmail:          customerEmail,
				keyPhone:          customerPhone,
				"radius_username": radiusUsername,
				"access_type":     accessType,
				"organization":    organization, // always present: "" or resolved name
			}
		}
	}

	// identitylink rows (verified contacts list)
	links := []map[string]interface{}{}
	for _, link := range r.identity.IdentityLinksFor(req.Context(), validated.Identity) {
		links = append(links, map[string]interface{}{
			"kind":         link.Kind,
			"value":        link.Value,
			"verified_via": link.VerifiedVia,
			"verified_at":  link.VerifiedAt,
		})
	}

	devices := r.identity.CountEnabledDevices(req.Context(), validated.Identity)

	// recent purchases summary (uses the same resolution as history)
	items, err := r.paymentService.ListIdentityPayments(req.Context(), payments.ListIdentityPaymentsRequest{
		Phone:       validated.Identity.Phone,
		Email:       validated.Identity.Email,
		CustomerIDs: customerIDs,
		Limit:       10,
	})
	completed := 0
	if err == nil {
		for _, item := range items {
			if strings.EqualFold(item.State, "completed") {
				completed++
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		keyIdentity:     validated.Identity,
		"issued_at":     validated.IssuedAtTime().Unix(),
		"contact":       contactOf(validated.Identity),
		"contact_kind":  contactKindOf(validated.Identity),
		"customer":      customer,
		"identitylinks": links,
		"claim_hint": func() string {
			if primaryCustomerID > 0 {
				return ""
			}
			return "verify_with_the_phone_number_you_use_to_buy"
		}(),
		"device_count": devices,
		"purchase_count": map[string]int{
			"total":     len(items),
			"completed": completed,
		},
		"recent_payments": items,
	})
}

func contactOf(id identity.Identity) string {
	if id.Phone != "" {
		return id.Phone
	}
	return id.Email
}

func contactKindOf(id identity.Identity) string {
	if id.Phone != "" {
		return keyPhone
	}
	return keyEmail
}

// contactAddRequest — POST /api/v1/identity/contacts/add (Bearer)
type contactAddRequest struct {
	Kind     string `json:"kind"` // "phone" | "email"
	Value    string `json:"value"`
	TakeOver bool   `json:"take_over,omitempty"`
}

// contactAddHandler adds a phone/email to the logged-in customer's identity
// set without verification (tier: 'contact' — purchases claim instantly).
func (r *Router) contactAddHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	validated, ok := r.identityBearerRevocationChecked(w, req)
	if !ok {
		return
	}
	var body contactAddRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	err := r.identity.AddContact(req.Context(), validated, body.Kind, body.Value, body.TakeOver)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrContactInUse):
			respondError(w, http.StatusConflict,
				"contact already claimed — verify by code instead, or retry with take_over: true")
		case errors.Is(err, identity.ErrPasswordNoOwner):
			respondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, identity.ErrInvalidRecipient), errors.Is(err, identity.ErrUnsupportedChannel):
			respondError(w, http.StatusBadRequest, err.Error())
		default:
			slog.Error("identity: add contact failed", slog.Any("error", err))
			respondError(w, http.StatusInternalServerError, "add contact failed")
		}
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{keyStatus: "added"})
}

// contactVerifyOTPRequest / contactVerifyOTPVerify — promote tier:
//   POST /api/v1/identity/contacts/verify/request {kind,value}
//   POST /api/v1/identity/contacts/verify/confirm {kind,value,code}
// They re-use the OTP request/verify machinery and then flip the row from
// 'contact' to 'otp'; the customer claim is preserved.

type contactVerifyOTPRequest struct {
	Kind  string `json:"kind"` // "phone" | "email"
	Value string `json:"value"`
}

func contactKindChannel(kind string) string {
	if strings.ToLower(strings.TrimSpace(kind)) == keyEmail {
		return keyEmail
	}
	return "sms"
}

func (r *Router) contactVerifyRequestHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	var body contactVerifyOTPRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(body.Kind))
	value := strings.TrimSpace(body.Value)
	if kind != keyPhone && kind != keyEmail || value == "" {
		respondError(w, http.StatusBadRequest, "kind (phone|email) and value are required")
		return
	}

	ch := contactKindChannel(kind)
	expiresAt, err := r.identity.RequestOTPCh(req.Context(), ch, value)
	if err != nil {
		passwordErrorResponse(w, err)
		return
	}
	respondJSON(w, http.StatusAccepted, map[string]interface{}{
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
}

type contactVerifyConfirmRequest struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Code  string `json:"code"`
}

func (r *Router) contactVerifyConfirmHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	validated, ok := r.identityBearerRevocationChecked(w, req)
	if !ok {
		return
	}
	var body contactVerifyConfirmRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ch := contactKindChannel(body.Kind)
	if err := r.identity.PromoteContact(req.Context(), validated, ch, body.Value, strings.TrimSpace(body.Code)); err != nil {
		passwordErrorResponse(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{keyStatus: "verified"})
}
