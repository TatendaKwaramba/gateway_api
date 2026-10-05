package httpapi

// Password identity endpoints (M-CIM / M-Login):
//   POST /api/v1/identity/password/register — OTP/SSO-bearer-gated creation/upsert
//   POST /api/v1/identity/password/login    — phone/email + password → identity token
//   POST /api/v1/identity/password/change   — bearer + current password
//   POST /api/v1/identity/password/reset    — fresh OTP-verified bearer + new password

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/freeradius/payments-api/internal/identity"
)

type passwordLoginRequest struct {
	Channel   string `json:"channel"` // "sms" | "email"
	Recipient string `json:"recipient"`
	Password  string `json:"password"`
}

type passwordSetRequest struct {
	Password string `json:"password"`
}

type passwordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	Password        string `json:"password"`
}

func passwordErrorResponse(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identity.ErrPasswordUnknown):
		respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, identity.ErrPasswordInvalid):
		respondError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, identity.ErrPasswordLocked):
		respondError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, identity.ErrPasswordWeak):
		respondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, identity.ErrPasswordNoOwner):
		respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, identity.ErrInvalidRecipient), errors.Is(err, identity.ErrUnsupportedChannel):
		respondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, identity.ErrDisabled):
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
	default:
		slog.Error("identity: password endpoint failed", slog.Any("error", err))
		respondError(w, http.StatusInternalServerError, "password operation failed")
	}
}

// passwordLoginHandler — POST /api/v1/identity/password/login
func (r *Router) passwordLoginHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	var body passwordLoginRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	channel := "sms"
	if strings.ToLower(strings.TrimSpace(body.Channel)) == keyEmail {
		channel = keyEmail
	}
	token, ident, err := r.identity.LoginWithPassword(req.Context(), channel, body.Recipient, body.Password)
	if err != nil {
		passwordErrorResponse(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"token":     token,
		keyIdentity: ident,
	})
}

// freshToken enforces that the bearer was minted recently (register/reset).
// Password changes must have an unexpired-in-any-case token but a fresh one
// proves the contact was just verified (OTP / Google).
func (r *Router) freshBearer(w http.ResponseWriter, req *http.Request) (identity.ValidatedToken, error) {
	validated, ok := r.identityBearerRevocationChecked(w, req)
	if !ok {
		return identity.ValidatedToken{}, errors.New("unauthorized")
	}
	return validated, nil
}

// passwordRegisterHandler sets the initial password (bearer-gated).
func (r *Router) passwordRegisterHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	validated, err := r.freshBearer(w, req)
	if err != nil && !validated.IssuedAtTime().IsZero() {
		return
	}
	var body passwordSetRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	err = r.identity.ResetPassword(req.Context(), validated, strings.TrimSpace(body.Password))
	if err != nil {
		passwordErrorResponse(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{keyStatus: "password set"})
}

// passwordChangeHandler swaps the password (bearer + current password).
func (r *Router) passwordChangeHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	validated, ok := r.identityBearerRevocationChecked(w, req)
	if !ok {
		return
	}
	var body passwordChangeRequest
	if err := parseJSON(req, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	err := r.identity.ChangePassword(req.Context(), validated, body.CurrentPassword, strings.TrimSpace(body.Password))
	if err != nil {
		passwordErrorResponse(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{keyStatus: "password changed"})
}

// passwordResetHandler — forgot-password: the SAME flow as register (OTP
// first, then set). Freshness (iat) is enforced server-side.
func (r *Router) passwordResetHandler(w http.ResponseWriter, req *http.Request) {
	r.passwordRegisterHandler(w, req)
}

// passwordStatusHandler — GET /api/v1/identity/password/status?channel=sms|email&recipient=…
// Public (no bearer): answers whether this contact has a password to sign in
// with, so the login screen can default to the password method.
type passwordStatusResponse struct {
	HasPassword bool `json:"has_password"`
}

func (r *Router) passwordStatusHandler(w http.ResponseWriter, req *http.Request) {
	if !r.identity.Enabled() {
		respondError(w, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	query := req.URL.Query()
	channel := strings.ToLower(strings.TrimSpace(query.Get("channel")))
	recipient := strings.TrimSpace(query.Get("recipient"))
	if channel != keyEmail {
		channel = "sms"
	}

	var ch = "sms"
	if channel == keyEmail {
		ch = keyEmail
	}
	has, err := r.identity.PasswordStatus(req.Context(), ch, recipient)
	if err != nil {
		passwordErrorResponse(w, err)
		return
	}
	respondJSON(w, http.StatusOK, passwordStatusResponse{HasPassword: has})
}
