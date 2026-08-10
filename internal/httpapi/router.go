package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/freeradius/payments-api/internal/gateways/mock"
	"github.com/freeradius/payments-api/internal/payments"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Router creates and configures the HTTP router
type Router struct {
	paymentService *payments.Service
	registry       *gateways.Registry
	adminAuth      func(http.Handler) http.Handler
}

// NewRouter creates a new HTTP router
func NewRouter(paymentService *payments.Service, registry *gateways.Registry, adminAuth func(http.Handler) http.Handler) *Router {
	return &Router{
		paymentService: paymentService,
		registry:       registry,
		adminAuth:      adminAuth,
	}
}

// corsMiddleware handles CORS for cross-origin requests
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Idempotency-Key")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// DeprecationMiddleware adds RFC 8594 headers to deprecated unversioned endpoints
func DeprecationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Deprecation", "true")
		w.Header().Set("Sunset", "Sat, 01 Aug 2027 00:00:00 GMT")
		w.Header().Set("Link", "</api/v1"+r.URL.Path+">; rel=\"successor-version\"")
		w.Header().Set("X-API-Version", "v1 (unversioned deprecated)")
		next.ServeHTTP(w, r)
	})
}

// Setup configures all routes
func (r *Router) Setup() chi.Router {
	router := chi.NewRouter()

	// Middleware
	router.Use(corsMiddleware)
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Logger)
	router.Use(middleware.Recoverer)
	router.Use(middleware.Timeout(30 * time.Second))
	router.Use(JSONContentType)
	router.Use(MetricsMiddleware)

	// Health check
	router.Get("/health", r.healthHandler)

	// Prometheus metrics
	router.Get("/metrics", MetricsHandler().ServeHTTP)

	// v1 API (canonical)
	router.Route("/api/v1", func(v1 chi.Router) {
		v1.Get("/plans", r.listPlans)
		v1.Get("/subscription-plans", r.listSubscriptionPlans)
		v1.Get("/payments/gateways", r.listGateways)
		v1.Get("/payments/methods", r.listPaymentMethods)
		v1.Post("/payments/subscriptions/{subscription_id}/adjust", r.adjustSubscription)
		v1.Get("/payments/subscriptions/{subscription_id}", r.getSubscription)
		v1.Post("/payments/initiate", r.initiatePayment)
		v1.Get("/payments/{transaction_id}/status", r.getPaymentStatus)
		v1.Post("/payments/{transaction_id}/poll", r.pollPaymentStatus)
	})

	// Unversioned API (deprecated - serves v1, returns deprecation headers)
	router.Route("/api", func(api chi.Router) {
		api.Use(DeprecationMiddleware)
		api.Get("/plans", r.listPlans)
		api.Get("/subscription-plans", r.listSubscriptionPlans)
		api.Get("/payments/gateways", r.listGateways)
		api.Get("/payments/methods", r.listPaymentMethods)
		api.Post("/payments/subscriptions/{subscription_id}/adjust", r.adjustSubscription)
		api.Get("/payments/subscriptions/{subscription_id}", r.getSubscription)
		api.Post("/payments/initiate", r.initiatePayment)
		api.Get("/payments/{transaction_id}/status", r.getPaymentStatus)
		api.Post("/payments/{transaction_id}/poll", r.pollPaymentStatus)
	})

	// Webhooks (from payment gateways)
	router.Post("/webhooks/{gateway_code}", r.webhookHandler)

	// Admin API (authenticated - no versioning, internal)
	router.Route("/admin/api", func(admin chi.Router) {
		if r.adminAuth != nil {
			admin.Use(r.adminAuth)
		}
		admin.Get("/payments", r.adminListTransactions)
		admin.Get("/payments/{transaction_id}", r.adminGetTransaction)
		admin.Get("/payments/{transaction_id}/receipt", r.adminGetReceipt)
		admin.Get("/payments/{transaction_id}/webhooks", r.adminGetTransactionWebhooks)
		admin.Post("/payments/{transaction_id}/refund", r.adminRefund)
		admin.Post("/payments/{transaction_id}/cancel", r.adminCancel)
		admin.Get("/gateways", r.adminListGateways)
		admin.Get("/gateways/{gateway_code}/schema", r.adminGetGatewaySchema)
	})

	// Mock gateway admin endpoints (only when mock is enabled)
	if mockAdapter := r.getMockAdapter(); mockAdapter != nil {
		router.Route("/api/mock", func(mock chi.Router) {
			mock.Get("/transactions", r.mockListTransactions)
			mock.Post("/transactions/{external_ref}/complete", r.mockCompleteTransaction)
			mock.Post("/transactions/{external_ref}/fail", r.mockFailTransaction)
			mock.Post("/transactions/{external_ref}/refund", r.mockRefundTransaction)
			mock.Post("/transactions/{external_ref}/webhook", r.mockTriggerWebhook)
		})
		router.Get("/mock/checkout/{external_ref}", r.mockCheckoutPage)
	}

	return router
}

func (r *Router) healthHandler(w http.ResponseWriter, req *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{
		"status": "healthy",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (r *Router) getMockAdapter() *mock.Adapter {
	g, ok := r.registry.Resolve("mock")
	if !ok {
		return nil
	}
	if adapter, ok := g.(*mock.Adapter); ok {
		return adapter
	}
	return nil
}

// JSONContentType middleware ensures JSON content type
func JSONContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

// respondJSON writes a JSON response
func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.WriteHeader(status)
	if data != nil {
		json.NewEncoder(w).Encode(data)
	}
}

// respondError writes an error response
func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{
		"error": message,
	})
}

// parseJSON parses JSON from request body
func parseJSON(r *http.Request, v interface{}) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func init() {
	slog.SetDefault(slog.Default())
}
