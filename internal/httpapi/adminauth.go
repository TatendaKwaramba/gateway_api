package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AdminHMACMiddleware creates an HMAC authentication middleware for admin endpoints.
// It validates X-Admin-Signature (HMAC-SHA256 of "METHOD|PATH|BODY|TIMESTAMP") and
// X-Admin-Timestamp (must be within 5 minutes).
func AdminHMACMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for health and metrics
			if r.URL.Path == "/health" || r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			sig := r.Header.Get("X-Admin-Signature")
			tsStr := r.Header.Get("X-Admin-Timestamp")

			if sig == "" || tsStr == "" {
				respondError(w, http.StatusUnauthorized, "missing admin authentication headers")
				return
			}

			// Validate timestamp within 5 minutes
			ts, err := strconv.ParseInt(tsStr, 10, 64)
			if err != nil {
				respondError(w, http.StatusUnauthorized, "invalid timestamp")
				return
			}

			now := time.Now().Unix()
			if now-ts > 300 || ts-now > 60 {
				respondError(w, http.StatusUnauthorized, "timestamp expired")
				return
			}

			// Read body
			body, err := io.ReadAll(r.Body)
			if err != nil {
				respondError(w, http.StatusBadRequest, "failed to read body")
				return
			}
			// Restore body for downstream handlers
			r.Body = io.NopCloser(bytes.NewReader(body))

			// Build canonical string
			canonical := fmt.Sprintf("%s|%s|%s|%s",
				strings.ToUpper(r.Method),
				r.URL.Path,
				string(body),
				tsStr,
			)

			expectedSig := hmacSHA256(secret, canonical)
			if !hmac.Equal([]byte(expectedSig), []byte(sig)) {
				respondError(w, http.StatusUnauthorized, "invalid signature")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func hmacSHA256(secret, message string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(message))
	return hex.EncodeToString(h.Sum(nil))
}
