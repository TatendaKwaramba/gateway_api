// Package httpapi provides HTTP middleware for the payments API.
package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/freeradius/payments_api/internal/metrics"
)

// normalizeEndpoint normalizes URL path for metrics grouping
func normalizeEndpoint(path string) string {
	// Remove trailing slash
	path = strings.TrimRight(path, "/")

	// Normalize UUIDs
	if idx := strings.Index(path, "/"); idx != -1 {
		parts := strings.Split(path, "/")
		for i, part := range parts {
			if len(part) == 36 && strings.Count(part, "-") == 4 {
				// Likely a UUID
				parts[i] = "{id}"
			}
		}
		path = strings.Join(parts, "/")
	}

	// Normalize numeric IDs
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if isNumeric(part) {
			parts[i] = "{id}"
		}
	}
	path = strings.Join(parts, "/")

	return path
}

// isNumeric checks if a string is numeric
func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// MetricsMiddleware records HTTP request metrics
func MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		endpoint := normalizeEndpoint(r.URL.Path)

		// Skip metrics endpoint to avoid recursion
		if endpoint == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		// Increment in-progress counter
		metrics.IncHTTPRequestInProgress(r.Method, endpoint)
		defer metrics.DecHTTPRequestInProgress(r.Method, endpoint)

		// Wrap response writer to capture status code
		wrapped := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// Serve request
		next.ServeHTTP(wrapped, r)

		// Record metrics
		duration := time.Since(start)
		status := http.StatusText(wrapped.statusCode)
		metrics.RecordHTTPRequest(r.Method, endpoint, status, duration)
	})
}

// statusResponseWriter wraps http.ResponseWriter to capture status code
type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

// WriteHeader captures the status code
func (w *statusResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// MetricsHandler returns an HTTP handler for the /metrics endpoint
func MetricsHandler() http.Handler {
	return metrics.Handler()
}
