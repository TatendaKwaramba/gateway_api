// Package gateways defines the gateway plugin interface and registry
package gateways

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/xeipuuv/gojsonschema"
)

// Money represents a monetary amount with currency
type Money struct {
	Amount   int64  `json:"amount"`   // Amount in smallest currency unit (e.g., cents)
	Currency string `json:"currency"` // ISO 4217 currency code
}

// Float64 returns the amount as a float64 (for display/logging)
func (m Money) Float64() float64 {
	return float64(m.Amount) / 100.0
}

// String returns a string representation
func (m Money) String() string {
	return fmt.Sprintf("%.2f %s", m.Float64(), m.Currency)
}

// Method represents a payment method supported by a gateway
type Method struct {
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
	RequiresPhone bool `json:"requires_phone"`
	RequiresRedirect bool `json:"requires_redirect"`
}

// Capabilities describes what a gateway supports
type Capabilities struct {
	SupportsRefund   bool `json:"supports_refund"`
	SupportsPolling  bool `json:"supports_polling"`
	RequiresRedirect bool `json:"requires_redirect"`
	RequiresPhone    bool `json:"requires_phone"`
	WebhookAsync     bool `json:"webhook_async"`
}

// InitiateRequest contains parameters for initiating a payment
type InitiateRequest struct {
	Amount       Money             `json:"amount"`
	Currency     string            `json:"currency"`
	MethodCode   string            `json:"method_code"`
	CustomerEmail string           `json:"customer_email,omitempty"`
	CustomerPhone string           `json:"customer_phone,omitempty"`
	ReturnURL    string            `json:"return_url"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

// InitiateResult contains the result of initiating a payment
type InitiateResult struct {
	ExternalReference string            `json:"external_reference"`
	State             string            `json:"state"`
	RedirectURL       string            `json:"redirect_url,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

// StatusResult contains the current status of a payment
type StatusResult struct {
	State        string `json:"state"`
	Amount       Money  `json:"amount"`
	Currency     string `json:"currency"`
	FailureReason string `json:"failure_reason,omitempty"`
}

// WebhookEvent represents a parsed webhook event
type WebhookEvent struct {
	ExternalReference string            `json:"external_reference"`
	State             string            `json:"state"`
	Amount            Money             `json:"amount"`
	Currency          string            `json:"currency"`
	EventType         string            `json:"event_type"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

// RefundResult contains the result of a refund
type RefundResult struct {
	ExternalReference string `json:"external_reference"`
	State             string `json:"state"`
}

// Gateway is the interface that all payment gateway adapters must implement
type Gateway interface {
	// Code returns the unique code for this gateway (e.g., "mock", "paynow")
	Code() string
	
	// Capabilities returns what this gateway supports
	Capabilities() Capabilities
	
	// SupportedMethods returns the payment methods this gateway supports
	SupportedMethods() []Method
	
	// SupportedCurrencies returns the ISO 4217 currency codes this gateway supports
	SupportedCurrencies() []string
	
	// ConfigSchema returns the JSON schema for gateway configuration
	ConfigSchema() *gojsonschema.Schema
	
	// Initiate starts a new payment transaction
	Initiate(ctx context.Context, req InitiateRequest) (InitiateResult, error)
	
	// Status checks the current status of a transaction
	Status(ctx context.Context, externalRef string) (StatusResult, error)
	
	// VerifyWebhook validates and parses a webhook request
	VerifyWebhook(ctx context.Context, headers http.Header, body []byte) (WebhookEvent, error)
	
	// Refund processes a refund for a completed transaction
	Refund(ctx context.Context, externalRef string, amount Money) (RefundResult, error)
}

// Registry maintains a map of gateway codes to gateway instances
type Registry struct {
	mu       sync.RWMutex
	gateways map[string]Gateway
}

// NewRegistry creates a new empty registry
func NewRegistry() *Registry {
	return &Registry{
		gateways: make(map[string]Gateway),
	}
}

// Register adds a gateway to the registry
func (r *Registry) Register(g Gateway) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	code := g.Code()
	if code == "" {
		return fmt.Errorf("gateway code cannot be empty")
	}
	
	if _, exists := r.gateways[code]; exists {
		return fmt.Errorf("gateway with code %q already registered", code)
	}
	
	r.gateways[code] = g
	return nil
}

// Resolve returns the gateway with the given code
func (r *Registry) Resolve(code string) (Gateway, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	g, ok := r.gateways[code]
	return g, ok
}

// List returns all registered gateways
func (r *Registry) List() []Gateway {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	list := make([]Gateway, 0, len(r.gateways))
	for _, g := range r.gateways {
		list = append(list, g)
	}
	return list
}

// AvailableMethods returns all payment methods from all gateways, optionally filtered by currency
func (r *Registry) AvailableMethods(currency string) []struct {
	GatewayCode string `json:"gateway_code"`
	Method      Method `json:"method"`
} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	var methods []struct {
		GatewayCode string `json:"gateway_code"`
		Method      Method `json:"method"`
	}
	
	for _, g := range r.gateways {
		// Check if gateway supports the requested currency
		if currency != "" {
			supported := false
			for _, c := range g.SupportedCurrencies() {
				if c == currency {
					supported = true
					break
				}
			}
			if !supported {
				continue
			}
		}
		
		for _, m := range g.SupportedMethods() {
			methods = append(methods, struct {
				GatewayCode string `json:"gateway_code"`
				Method      Method `json:"method"`
			}{
				GatewayCode: g.Code(),
				Method:      m,
			})
		}
	}
	
	return methods
}

// ConfigSchemaJSON returns a JSON-serializable config schema for a gateway
type ConfigSchemaJSON struct {
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
}

// GetConfigSchema returns the configuration schema for a gateway as JSON
func (r *Registry) GetConfigSchema(code string) (ConfigSchemaJSON, bool) {
	g, ok := r.Resolve(code)
	if !ok {
		return ConfigSchemaJSON{}, false
	}
	
	schema := g.ConfigSchema()
	if schema == nil {
		return ConfigSchemaJSON{Type: "object"}, true
	}
	
	// Convert to JSON and back to get a clean map structure
	jsonData, _ := json.Marshal(schema)
	var result ConfigSchemaJSON
	json.Unmarshal(jsonData, &result)
	
	return result, true
}
