// Package metrics provides Prometheus metrics for the payments API
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// paymentsInitiatedTotal counts initiated payments by gateway and method
	paymentsInitiatedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "payments_initiated_total",
			Help: "Total number of initiated payments",
		},
		[]string{"gateway", "method"},
	)

	// paymentsCompletedTotal counts completed payments
	paymentsCompletedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "payments_completed_total",
			Help: "Total number of completed payments",
		},
	)

	// paymentsFailedTotal counts failed payments by reason
	paymentsFailedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "payments_failed_total",
			Help: "Total number of failed payments",
		},
		[]string{"reason"},
	)

	// webhooksReceivedTotal counts webhooks received by gateway and validity
	webhooksReceivedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhooks_received_total",
			Help: "Total number of webhooks received",
		},
		[]string{"gateway", "signature_valid"},
	)

	// fulfillmentDurationSeconds tracks fulfillment latency
	fulfillmentDurationSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name: "fulfillment_duration_seconds",
			Help: "Duration of fulfillment operations in seconds",
			// Extend beyond prometheus.DefBuckets (max 10s) so the 60s
			// fulfillment latency SLO (docs/operations/sli-slo.md SLO-2) is resolvable.
			Buckets: []float64{1, 5, 10, 30, 60, 120, 300},
		},
	)

	// fulfillmentSuccessTotal counts successful fulfillments by kind
	fulfillmentSuccessTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fulfillment_success_total",
			Help: "Total number of successful fulfillments",
		},
		[]string{"kind"},
	)

	// fulfillmentFailureTotal counts failed fulfillments by kind and reason
	fulfillmentFailureTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fulfillment_failure_total",
			Help: "Total number of failed fulfillments",
		},
		[]string{"kind", "reason"},
	)

	// pollerRunsTotal counts poller executions
	pollerRunsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "poller_runs_total",
			Help: "Total number of poller runs",
		},
	)

	// pollerTransactionsChecked counts transactions checked per run
	pollerTransactionsChecked = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "poller_transactions_checked",
			Help:    "Number of transactions checked per poller run",
			Buckets: []float64{0, 1, 5, 10, 25, 50, 100, 250, 500},
		},
	)

	// notificationAttemptsTotal counts notification attempts by provider and status
	notificationAttemptsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "notification_attempts_total",
			Help: "Total number of notification attempts",
		},
		[]string{"provider", "status"},
	)

	// HTTP request metrics
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "payments_http_requests_total",
			Help: "Total HTTP requests to payments API",
		},
		[]string{"method", "endpoint", "status"},
	)

	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "payments_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0},
		},
		[]string{"method", "endpoint"},
	)

	httpRequestsInProgress = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "payments_http_requests_in_progress",
			Help: "Number of HTTP requests currently being processed",
		},
		[]string{"method", "endpoint"},
	)
)

// RecordPaymentInitiated increments the initiated counter
func RecordPaymentInitiated(gateway, method string) {
	paymentsInitiatedTotal.WithLabelValues(gateway, method).Inc()
}

// RecordPaymentCompleted increments the completed counter
func RecordPaymentCompleted() {
	paymentsCompletedTotal.Inc()
}

// RecordPaymentFailed increments the failed counter
func RecordPaymentFailed(reason string) {
	paymentsFailedTotal.WithLabelValues(reason).Inc()
}

// RecordWebhookReceived increments the webhook counter
func RecordWebhookReceived(gateway string, signatureValid bool) {
	validStr := "false"
	if signatureValid {
		validStr = "true"
	}
	webhooksReceivedTotal.WithLabelValues(gateway, validStr).Inc()
}

// RecordFulfillmentDuration records the duration of a fulfillment operation
func RecordFulfillmentDuration(seconds float64) {
	fulfillmentDurationSeconds.Observe(seconds)
}

// RecordFulfillmentSuccess records a successful fulfillment
func RecordFulfillmentSuccess(kind string) {
	fulfillmentSuccessTotal.WithLabelValues(kind).Inc()
}

// RecordFulfillmentFailure records a failed fulfillment
func RecordFulfillmentFailure(kind, reason string) {
	fulfillmentFailureTotal.WithLabelValues(kind, reason).Inc()
}

// RecordPollerRun records a poller run
func RecordPollerRun(transactionsChecked int) {
	pollerRunsTotal.Inc()
	pollerTransactionsChecked.Observe(float64(transactionsChecked))
}

// RecordNotificationAttempt records a notification attempt
func RecordNotificationAttempt(provider, status string) {
	notificationAttemptsTotal.WithLabelValues(provider, status).Inc()
}

// RecordHTTPRequest records an HTTP request
func RecordHTTPRequest(method, endpoint, status string, duration time.Duration) {
	httpRequestsTotal.WithLabelValues(method, endpoint, status).Inc()
	httpRequestDuration.WithLabelValues(method, endpoint).Observe(duration.Seconds())
}

// IncHTTPRequestInProgress increments the in-progress counter
func IncHTTPRequestInProgress(method, endpoint string) {
	httpRequestsInProgress.WithLabelValues(method, endpoint).Inc()
}

// DecHTTPRequestInProgress decrements the in-progress counter
func DecHTTPRequestInProgress(method, endpoint string) {
	httpRequestsInProgress.WithLabelValues(method, endpoint).Dec()
}

// Handler returns an HTTP handler for the /metrics endpoint
func Handler() http.Handler {
	return promhttp.Handler()
}
