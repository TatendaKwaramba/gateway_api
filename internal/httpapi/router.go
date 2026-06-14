package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/freeradius/payments-api/internal/gateways/mock"
	"github.com/freeradius/payments-api/internal/metrics"
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
		// Allow requests from any origin in development
		// In production, this should be restricted to specific origins
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Idempotency-Key")
		w.Header().Set("Access-Control-Max-Age", "86400")

		// Handle preflight requests
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

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
	
	// Health check
	router.Get("/health", r.healthHandler)
	
	// Prometheus metrics
	router.Get("/metrics", metrics.Handler().ServeHTTP)
	
	// Public API (rate-limited in production)
	router.Route("/api", func(api chi.Router) {
		// Tariff plans
		api.Get("/plans", r.listPlans)

		// Subscription plans
		api.Get("/subscription-plans", r.listSubscriptionPlans)

		// Payment gateways
		api.Get("/payments/gateways", r.listGateways)

		// Payment methods (filtered by gateway)
		api.Get("/payments/methods", r.listPaymentMethods)
		
		// Payment initiation
		api.Post("/payments/initiate", r.initiatePayment)
		
		// Payment status
		api.Get("/payments/{transaction_id}/status", r.getPaymentStatus)
		
		// Manual poll (queries gateway and updates state)
		api.Post("/payments/{transaction_id}/poll", r.pollPaymentStatus)
	})
	
	// Webhooks (from payment gateways)
	router.Post("/webhooks/{gateway_code}", r.webhookHandler)
	
	// Admin API (authenticated)
	router.Route("/admin/api", func(admin chi.Router) {
		if r.adminAuth != nil {
			admin.Use(r.adminAuth)
		}

		// Transaction management
		admin.Get("/payments", r.adminListTransactions)
		admin.Get("/payments/{transaction_id}", r.adminGetTransaction)
		admin.Get("/payments/{transaction_id}/receipt", r.adminGetReceipt)
		admin.Get("/payments/{transaction_id}/webhooks", r.adminGetTransactionWebhooks)
		admin.Post("/payments/{transaction_id}/refund", r.adminRefund)
		admin.Post("/payments/{transaction_id}/cancel", r.adminCancel)

		// Gateway management
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
		
		// Mock checkout page
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
	
	// This is a bit of a hack - in production we'd use a type assertion
	// or have a registry method to get mock-specific interface
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
	// Ensure slog is set up
	slog.SetDefault(slog.Default())
}
