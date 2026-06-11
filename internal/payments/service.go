package payments

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/freeradius/payments-api/internal/django"
	"github.com/freeradius/payments-api/internal/fulfillment"
	"github.com/freeradius/payments-api/internal/gateways"
	"github.com/freeradius/payments-api/internal/metrics"
	"github.com/google/uuid"
)

// FulfillmentService is the interface the payment service needs from fulfillment
type FulfillmentService interface {
	Fulfill(ctx context.Context, req fulfillment.FulfillRequest) (*fulfillment.FulfillResult, error)
	Rollback(ctx context.Context, voucherPIN string) error
}

// Service handles payment operations
type Service struct {
	db                  *sql.DB
	registry            *gateways.Registry
	stateMachine        *StateMachine
	idempotencyStore    *IdempotencyStore
	webhookReplayStore  *WebhookReplayStore
	fulfillmentService  FulfillmentService
	djangoClient        *django.Client
	defaultCurrency     string
}

// NewService creates a new payment service
func NewService(db *sql.DB, registry *gateways.Registry, fulfillmentService FulfillmentService, djangoClient *django.Client, defaultCurrency string) *Service {
	if defaultCurrency == "" {
		defaultCurrency = "USD"
	}
	return &Service{
		db:                 db,
		registry:           registry,
		stateMachine:       NewStateMachine(),
		idempotencyStore:   NewIdempotencyStore(db),
		webhookReplayStore: NewWebhookReplayStore(db),
		fulfillmentService: fulfillmentService,
		djangoClient:       djangoClient,
		defaultCurrency:    defaultCurrency,
	}
}

// InitiateRequest represents a request to initiate a payment
type InitiateRequest struct {
	GatewayCode      string            `json:"gateway_code"`
	GatewayID        *int64            `json:"gateway_id,omitempty"` // F3.8: explicit org-scoped gateway
	MethodCode       string            `json:"method_code"`
	Amount           int64             `json:"amount"` // minor currency units (cents)
	Currency         string            `json:"currency"`
	PlanID           *int64            `json:"plan_id,omitempty"`
	CustomerEmail    string            `json:"customer_email,omitempty"`
	CustomerPhone    string            `json:"customer_phone,omitempty"`
	CustomerID       *int64            `json:"customer_id,omitempty"`
	FulfillmentKind  string            `json:"fulfillment_kind,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
}

// InitiateResponse represents the response from initiating a payment
type InitiateResponse struct {
	TransactionID     int64             `json:"transaction_id"`
	TransactionIDStr  string            `json:"transaction_id_str"`
	State             string            `json:"state"`
	GatewayCode       string            `json:"gateway_code"`
	Amount            int64             `json:"amount"`
	Currency          string            `json:"currency"`
	RedirectURL       string            `json:"redirect_url,omitempty"`
	IsReplay          bool              `json:"is_replay,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
}

// Initiate creates a new payment transaction
func (s *Service) Initiate(ctx context.Context, req InitiateRequest) (*InitiateResponse, error) {
	// Validate request
	if req.GatewayCode == "" && req.GatewayID == nil {
		return nil, fmt.Errorf("gateway_code or gateway_id is required")
	}
	if req.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	if req.Currency == "" {
		return nil, fmt.Errorf("currency is required")
	}

	// F3.8: Resolve gateway code from gateway_id if not provided
	if req.GatewayID != nil && req.GatewayCode == "" {
		gatewayCode, err := s.resolveGatewayCode(ctx, *req.GatewayID)
		if err != nil {
			return nil, fmt.Errorf("gateway not found for id %d", *req.GatewayID)
		}
		req.GatewayCode = gatewayCode
	}

	if err := s.validateGatewayCurrency(ctx, req.GatewayCode, req.Currency); err != nil {
		return nil, err
	}

	if req.PlanID != nil {
		if err := s.validatePlanAmount(ctx, *req.PlanID, req.Amount, req.Currency); err != nil {
			return nil, err
		}
	}
	
	// Resolve gateway
	gateway, ok := s.registry.Resolve(req.GatewayCode)
	if !ok {
		return nil, fmt.Errorf("gateway not found: %s", req.GatewayCode)
	}
	
	// Check idempotency
	reqJSON, _ := json.Marshal(req)
	requestHash := ComputeRequestHash(reqJSON)
	
	if req.IdempotencyKey != "" {
		existingID, isReplay, err := s.idempotencyStore.CheckAndStore(ctx, req.IdempotencyKey, requestHash)
		if err != nil {
			return nil, fmt.Errorf("idempotency check failed: %w", err)
		}
		
		if isReplay {
			// Return existing transaction as InitiateResponse
			return s.getInitiateResponse(ctx, existingID)
		}
	}
	
	// Create transaction record
	transactionIDStr := uuid.New().String()
	fulfillmentKind := req.FulfillmentKind
	if fulfillmentKind == "" {
		fulfillmentKind = "voucher"
	}
	
	var planID interface{}
	if req.PlanID != nil {
		planID = *req.PlanID
	}

	// Serialize metadata to JSON for gateway_response storage
	gatewayResponseJSON := "{}"
	if len(req.Metadata) > 0 {
		if b, err := json.Marshal(req.Metadata); err == nil {
			gatewayResponseJSON = string(b)
		}
	}

	var transactionID int64

	// Tier 2: Resolve organization_id from gateway and calculate fees
	var orgID *int64
	var feeAmount float64
	gatewayOrgID, _ := s.resolveGatewayOrgID(ctx, req.GatewayCode, req.GatewayID)
	if gatewayOrgID > 0 {
		orgID = &gatewayOrgID
	}
	feeAmount = s.calculateFee(ctx, req.GatewayCode, req.GatewayID, float64(req.Amount)/100.0)
	netAmount := float64(req.Amount)/100.0 - feeAmount

	// F3.8: When gateway_id is provided, insert directly without gateway_code join
	if req.GatewayID != nil {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO payments_paymenttransaction (
				transaction_id, gateway_id, payment_method_id, amount, currency,
				tariff_plan_id,
				state, status, customer_id, customer_email, customer_phone,
				idempotency_key, fulfillment_kind, gateway_response,
				organization_id, fee_amount, net_amount,
				created_at, updated_at
			)
			SELECT 
				?, ?, pm.id, ?, ?, ?,
				'initiated', 'initiated', ?, ?, ?,
				?, ?, ?,
				?, ?, ?,
				NOW(), NOW()
			FROM payments_paymentmethod pm
			WHERE pm.gateway_id = ? AND pm.method_code = ?
		`,
			transactionIDStr,
			*req.GatewayID,
			float64(req.Amount)/100.0,
			req.Currency,
			planID,
			req.CustomerID,
			req.CustomerEmail,
			req.CustomerPhone,
			req.IdempotencyKey,
			fulfillmentKind,
			gatewayResponseJSON,
			orgID,
			feeAmount,
			netAmount,
			*req.GatewayID,
			req.MethodCode,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create transaction: %w", err)
		}
	} else {
		result, err := s.db.ExecContext(ctx, `
			INSERT INTO payments_paymenttransaction (
				transaction_id, gateway_id, payment_method_id, amount, currency,
				tariff_plan_id,
				state, status, customer_id, customer_email, customer_phone,
				idempotency_key, fulfillment_kind, gateway_response,
				organization_id, fee_amount, net_amount,
				created_at, updated_at
			)
			SELECT 
				?, g.id, pm.id, ?, ?, ?,
				'initiated', 'initiated', ?, ?, ?,
				?, ?, ?,
				g.organization_id, ?, ?,
				NOW(), NOW()
			FROM payments_paymentgateway g
			LEFT JOIN payments_paymentmethod pm ON pm.gateway_id = g.id AND pm.method_code = ?
			WHERE g.gateway_code = ?
		`,
			transactionIDStr,
			float64(req.Amount)/100.0,
			req.Currency,
			planID,
			req.CustomerID,
			req.CustomerEmail,
			req.CustomerPhone,
			req.IdempotencyKey,
			fulfillmentKind,
			gatewayResponseJSON,
			feeAmount,
			netAmount,
			req.MethodCode,
			req.GatewayCode,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create transaction: %w", err)
		}
		transactionID, _ = result.LastInsertId()
	}
	
	slog.Info("transaction created",
		slog.Int64("transaction_id", transactionID),
		slog.String("gateway", req.GatewayCode),
		slog.String("amount", fmt.Sprintf("%d %s", req.Amount, req.Currency)),
	)
	
	metrics.RecordPaymentInitiated(req.GatewayCode, req.MethodCode)
	
	// Call gateway to initiate
	gatewayReq := gateways.InitiateRequest{
		Amount: gateways.Money{
			Amount:   req.Amount,
			Currency: req.Currency,
		},
		Currency:        req.Currency,
		MethodCode:      req.MethodCode,
		CustomerEmail:   req.CustomerEmail,
		CustomerPhone:   req.CustomerPhone,
		ReturnURL:       "", // Will be configured per gateway
		Metadata:        req.Metadata,
		IdempotencyKey:  req.IdempotencyKey,
	}
	
	gatewayResult, err := gateway.Initiate(ctx, gatewayReq)
	if err != nil {
		// Mark as failed
		s.db.ExecContext(ctx, `
			UPDATE payments_paymenttransaction 
			SET state = 'failed', status = 'failed', updated_at = NOW()
			WHERE id = ?
		`, transactionID)
		metrics.RecordPaymentFailed("gateway_initiate_error")
		return nil, fmt.Errorf("gateway initiation failed: %w", err)
	}
	
	// Merge gateway result into existing metadata (preserving referral_code etc.)
	mergedResponse := make(map[string]interface{})
	if len(req.Metadata) > 0 {
		for k, v := range req.Metadata {
			mergedResponse[k] = v
		}
	}
	mergedResponse["state"] = gatewayResult.State
	mergedResponse["external_reference"] = gatewayResult.ExternalReference
	gatewayResultJSON, _ := json.Marshal(mergedResponse)
	
	// Update transaction with gateway response
	_, err = s.db.ExecContext(ctx, `
		UPDATE payments_paymenttransaction 
		SET state = ?, status = ?, external_reference = ?, gateway_response = ?,
		    updated_at = NOW()
		WHERE id = ?
	`,
		gatewayResult.State,
		gatewayResult.State,
		gatewayResult.ExternalReference,
		string(gatewayResultJSON),
		transactionID,
	)
	
	if err != nil {
		slog.Error("failed to update transaction after gateway initiation",
			slog.Int64("transaction_id", transactionID),
			slog.Any("error", err),
		)
	}
	
	// Handle synchronous terminal states
	switch State(gatewayResult.State) {
	case StateCompleted:
		metrics.RecordPaymentCompleted()
		slog.Info("transaction completed synchronously, triggering fulfillment",
			slog.Int64("transaction_id", transactionID),
		)
		go s.triggerFulfillment(transactionID)
	case StateFailed:
		metrics.RecordPaymentFailed("gateway_rejected")
	}
	
	return &InitiateResponse{
		TransactionID:     transactionID,
		TransactionIDStr:  transactionIDStr,
		State:             gatewayResult.State,
		GatewayCode:       req.GatewayCode,
		Amount:            req.Amount,
		Currency:          req.Currency,
		RedirectURL:       gatewayResult.RedirectURL,
		CreatedAt:         time.Now(),
	}, nil
}

// GetStatusRequest represents a request to get transaction status
type GetStatusRequest struct {
	TransactionID int64 `json:"transaction_id"`
}

// GetStatusResponse represents the response with transaction status
type GetStatusResponse struct {
	TransactionID     int64             `json:"transaction_id"`
	TransactionIDStr  string            `json:"transaction_id_str"`
	State             string            `json:"state"`
	Amount            int64             `json:"amount"`
	Currency          string            `json:"currency"`
	GatewayCode       string            `json:"gateway_code"`
	ExternalReference string            `json:"external_reference,omitempty"`
	VoucherPIN        string            `json:"voucher_pin,omitempty"`
	CustomerEmail     string            `json:"customer_email,omitempty"`
	CustomerPhone     string            `json:"customer_phone,omitempty"`
	GatewayResponse   map[string]interface{} `json:"gateway_response,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// nullString is a helper to convert sql.NullString to regular string
type nullString struct {
	sql.NullString
}

func (ns nullString) Value() string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

// GetStatus retrieves the current status of a transaction
func (s *Service) GetStatus(ctx context.Context, transactionID int64) (*GetStatusResponse, error) {
	return s.getTransactionResponse(ctx, transactionID)
}

func (s *Service) getTransactionResponse(ctx context.Context, transactionID int64) (*GetStatusResponse, error) {
	var resp GetStatusResponse
	var gatewayResponseJSON string
	var amountFloat float64
	
	// Use nullString for nullable fields
	var externalRef, voucherPin, customerEmail, customerPhone nullString
	
	err := s.db.QueryRowContext(ctx, `
		SELECT 
			t.id, t.transaction_id, t.state, 
			CAST(t.amount * 100 AS SIGNED) as amount_cents, t.currency,
			g.gateway_code, t.external_reference, t.voucher_pin,
			t.customer_email, t.customer_phone,
			JSON_OBJECT() as gateway_response, -- Placeholder
			t.created_at, t.updated_at
		FROM payments_paymenttransaction t
		JOIN payments_paymentgateway g ON g.id = t.gateway_id
		WHERE t.id = ?
	`, transactionID).Scan(
		&resp.TransactionID,
		&resp.TransactionIDStr,
		&resp.State,
		&resp.Amount,
		&resp.Currency,
		&resp.GatewayCode,
		&externalRef,
		&voucherPin,
		&customerEmail,
		&customerPhone,
		&gatewayResponseJSON,
		&resp.CreatedAt,
		&resp.UpdatedAt,
	)
	
	if err != nil {
		return nil, fmt.Errorf("failed to query transaction: %w", err)
	}
	
	_ = amountFloat // Silence unused warning, we'll use the amount_cents value
	
	// Convert nullStrings to regular strings
	resp.ExternalReference = externalRef.Value()
	resp.VoucherPIN = voucherPin.Value()
	resp.CustomerEmail = customerEmail.Value()
	resp.CustomerPhone = customerPhone.Value()
	
	return &resp, nil
}

// getInitiateResponse retrieves transaction data formatted for InitiateResponse (used for idempotency replays)
func (s *Service) getInitiateResponse(ctx context.Context, transactionID int64) (*InitiateResponse, error) {
	var resp InitiateResponse
	
	err := s.db.QueryRowContext(ctx, `
		SELECT 
			t.id, t.transaction_id, t.state, 
			CAST(t.amount * 100 AS SIGNED) as amount_cents, t.currency,
			g.gateway_code, t.created_at
		FROM payments_paymenttransaction t
		JOIN payments_paymentgateway g ON g.id = t.gateway_id
		WHERE t.id = ?
	`, transactionID).Scan(
		&resp.TransactionID,
		&resp.TransactionIDStr,
		&resp.State,
		&resp.Amount,
		&resp.Currency,
		&resp.GatewayCode,
		&resp.CreatedAt,
	)
	
	if err != nil {
		return nil, fmt.Errorf("failed to query transaction: %w", err)
	}
	
	resp.IsReplay = true
	return &resp, nil
}

// ListTransactionsRequest represents a request to list transactions
type ListTransactionsRequest struct {
	CustomerID *int64 `json:"customer_id,omitempty"`
	State      string `json:"state,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Offset     int    `json:"offset,omitempty"`
}

// ListTransactions lists transactions matching the criteria
func (s *Service) ListTransactions(ctx context.Context, req ListTransactionsRequest) ([]*GetStatusResponse, error) {
	if req.Limit <= 0 || req.Limit > 100 {
		req.Limit = 20
	}
	
	query := `
		SELECT 
			t.id, t.transaction_id, t.state, 
			CAST(t.amount * 100 AS SIGNED) as amount_cents, t.currency,
			g.gateway_code, t.external_reference, t.voucher_pin,
			t.customer_email, t.customer_phone,
			JSON_OBJECT(),
			t.created_at, t.updated_at
		FROM payments_paymenttransaction t
		JOIN payments_paymentgateway g ON g.id = t.gateway_id
		WHERE 1=1
	`
	args := []interface{}{}
	
	if req.CustomerID != nil {
		query += " AND t.customer_id = ?"
		args = append(args, *req.CustomerID)
	}
	
	if req.State != "" {
		query += " AND t.state = ?"
		args = append(args, req.State)
	}
	
	query += " ORDER BY t.created_at DESC LIMIT ? OFFSET ?"
	args = append(args, req.Limit, req.Offset)
	
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query transactions: %w", err)
	}
	defer rows.Close()
	
	var transactions []*GetStatusResponse
	for rows.Next() {
		var resp GetStatusResponse
		var gatewayResponseJSON string
		
		err := rows.Scan(
			&resp.TransactionID,
			&resp.TransactionIDStr,
			&resp.State,
			&resp.Amount,
			&resp.Currency,
			&resp.GatewayCode,
			&resp.ExternalReference,
			&resp.VoucherPIN,
			&resp.CustomerEmail,
			&resp.CustomerPhone,
			&gatewayResponseJSON,
			&resp.CreatedAt,
			&resp.UpdatedAt,
		)
		if err != nil {
			continue
		}
		
		transactions = append(transactions, &resp)
	}
	
	return transactions, nil
}

// ProcessWebhook processes a verified webhook event and transitions transaction state.
func (s *Service) ProcessWebhook(ctx context.Context, gatewayCode string, event gateways.WebhookEvent, rawBody []byte, headers http.Header) error {
	// Look up transaction by external reference and gateway
	var transactionID int64
	var currentState string
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.state 
		FROM payments_paymenttransaction t
		JOIN payments_paymentgateway g ON g.id = t.gateway_id
		WHERE t.external_reference = ? AND g.gateway_code = ?
	`, event.ExternalReference, gatewayCode).Scan(&transactionID, &currentState)

	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("transaction not found for external_reference=%s gateway=%s", event.ExternalReference, gatewayCode)
		}
		return fmt.Errorf("failed to query transaction: %w", err)
	}

	// Replay guard: check if this exact webhook has been processed before
	replayKey := WebhookEventKey{
		GatewayCode:       gatewayCode,
		ExternalReference: event.ExternalReference,
		EventType:         event.EventType,
	}
	isReplay, rpErr := s.webhookReplayStore.CheckAndLog(ctx, replayKey, rawBody, headers)
	if rpErr != nil {
		slog.Error("webhook replay check failed, continuing anyway",
			slog.String("gateway", gatewayCode),
			slog.String("external_ref", event.ExternalReference),
			slog.String("event_type", event.EventType),
			slog.Any("error", rpErr),
		)
	}
	if isReplay {
		slog.Info("webhook replay detected, skipping transition",
			slog.String("gateway", gatewayCode),
			slog.String("external_ref", event.ExternalReference),
			slog.String("event_type", event.EventType),
		)
		// Still log the replayed webhook for audit trail
		if logErr := s.logWebhook(ctx, gatewayCode, event, rawBody, headers, transactionID, true); logErr != nil {
			slog.Error("failed to log replayed webhook", slog.Any("error", logErr))
		}
		return nil
	}

	// Log webhook to audit table
	if logErr := s.logWebhook(ctx, gatewayCode, event, rawBody, headers, transactionID, true); logErr != nil {
		slog.Error("failed to log webhook", slog.Any("error", logErr))
	}

	// Attempt state transition
	fromState := State(currentState)
	toState := State(event.State)

	if !toState.Valid() {
		return fmt.Errorf("invalid target state from webhook: %s", event.State)
	}

	changed, err := s.stateMachine.Transition(ctx, s.db, transactionID, fromState, toState)
	if err != nil {
		// If transition is invalid (e.g., already terminal), log but don't fail
		// Gateways expect 200 even for noop/replayed events
		slog.Info("webhook state transition skipped",
			slog.Int64("transaction_id", transactionID),
			slog.String("from", fromState.String()),
			slog.String("to", toState.String()),
			slog.Any("error", err),
		)
		return nil // Swallow to return 200 to gateway
	}

	if changed {
		slog.Info("webhook state transition applied",
			slog.Int64("transaction_id", transactionID),
			slog.String("from", fromState.String()),
			slog.String("to", toState.String()),
			slog.String("gateway", gatewayCode),
		)

		if toState == StateCompleted {
			metrics.RecordPaymentCompleted()
			slog.Info("transaction completed, triggering fulfillment",
				slog.Int64("transaction_id", transactionID),
			)
			go s.triggerFulfillment(transactionID)
		}
		if toState == StateFailed {
			metrics.RecordPaymentFailed("gateway_failure")
		}
	}

	return nil
}

// triggerFulfillment triggers fulfillment for a completed transaction.
// It should be called with `go s.triggerFulfillment(id)` to run asynchronously.
func (s *Service) triggerFulfillment(transactionID int64) {
	ctx := context.Background()

	// Fetch transaction details needed for fulfillment
	var amountFloat float64
	var currency, customerPhone, customerEmail, fulfillmentKind, transactionIDStr string
	var planID sql.NullInt64
	var gatewayOrgID sql.NullInt64
	var gatewayResponse sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT
			t.transaction_id, t.amount, t.currency,
			t.customer_phone, t.customer_email, t.fulfillment_kind,
			t.tariff_plan_id, g.organization_id, t.gateway_response
		FROM payments_paymenttransaction t
		JOIN payments_paymentgateway g ON g.id = t.gateway_id
		WHERE t.id = ?
	`, transactionID).Scan(
		&transactionIDStr, &amountFloat, &currency,
		&customerPhone, &customerEmail, &fulfillmentKind,
		&planID, &gatewayOrgID, &gatewayResponse,
	)
	if err != nil {
		slog.Error("fulfillment: failed to query transaction details",
			slog.Int64("transaction_id", transactionID),
			slog.Any("error", err),
		)
		return
	}

	amountCents := int64(amountFloat * 100)

	var nasIP, nasID, referralCode string
	if gatewayResponse.Valid && gatewayResponse.String != "" {
		var meta map[string]interface{}
		if json.Unmarshal([]byte(gatewayResponse.String), &meta) == nil {
			if v, ok := meta["nas_ip_address"].(string); ok {
				nasIP = v
			}
			if v, ok := meta["nas_identifier"].(string); ok {
				nasID = v
			}
			if v, ok := meta["referral_code"].(string); ok {
				referralCode = v
			}
		}
	}

	fr := fulfillment.FulfillRequest{
		TransactionID:    transactionID,
		TransactionIDStr: transactionIDStr,
		Amount:           amountCents,
		Currency:         currency,
		CustomerPhone:    customerPhone,
		CustomerEmail:    customerEmail,
		FulfillmentKind:  fulfillmentKind,
		NasIPAddress:     nasIP,
		NasIdentifier:    nasID,
		ReferralCode:     referralCode,
	}
	if gatewayOrgID.Valid {
		fr.OrganizationID = gatewayOrgID.Int64
	}
	if planID.Valid {
		fr.PlanID = planID.Int64
	}
	result, err := s.fulfillmentService.Fulfill(ctx, fr)
	if err != nil {
		slog.Error("fulfillment failed",
			slog.Int64("transaction_id", transactionID),
			slog.Any("error", err),
		)
		// Rollback partial radcheck rows if a PIN was generated
		if result != nil && result.VoucherPIN != "" {
			_ = s.fulfillmentService.Rollback(ctx, result.VoucherPIN)
		}
		// Mark transaction as failed
		_, _ = s.stateMachine.Transition(ctx, s.db, transactionID, StateCompleted, StateFailed)
		metrics.RecordPaymentFailed("fulfillment_error")
		return
	}

	slog.Info("fulfillment succeeded",
		slog.Int64("transaction_id", transactionID),
		slog.String("voucher_pin", result.VoucherPIN),
	)

	// Notify Django of payment completion (triggers commission processing)
	go s.notifyDjangoCompletion(transactionID, referralCode)
}

// PollStatus checks the current status of a transaction via the gateway's API.
func (s *Service) PollStatus(ctx context.Context, transactionID int64) (*GetStatusResponse, error) {
	// Fetch transaction details
	var externalRef, gatewayCode, currentState string
	err := s.db.QueryRowContext(ctx, `
		SELECT t.external_reference, g.gateway_code, t.state
		FROM payments_paymenttransaction t
		JOIN payments_paymentgateway g ON g.id = t.gateway_id
		WHERE t.id = ?
	`, transactionID).Scan(&externalRef, &gatewayCode, &currentState)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("transaction not found: %d", transactionID)
		}
		return nil, fmt.Errorf("failed to query transaction: %w", err)
	}

	if State(currentState).IsTerminal() {
		return s.GetStatus(ctx, transactionID)
	}

	// Resolve gateway
	gateway, ok := s.registry.Resolve(gatewayCode)
	if !ok {
		return nil, fmt.Errorf("gateway not found: %s", gatewayCode)
	}

	// Update last_polled_at
	_, _ = s.db.ExecContext(ctx, `
		UPDATE payments_paymenttransaction 
		SET last_polled_at = NOW() 
		WHERE id = ?
	`, transactionID)

	// Query gateway status
	statusResult, err := gateway.Status(ctx, externalRef)
	if err != nil {
		return nil, fmt.Errorf("gateway status check failed: %w", err)
	}

	// Transition if state changed
	if statusResult.State != currentState {
		fromState := State(currentState)
		toState := State(statusResult.State)
		if toState.Valid() {
			changed, _ := s.stateMachine.Transition(ctx, s.db, transactionID, fromState, toState)
			if changed && toState == StateCompleted {
				metrics.RecordPaymentCompleted()
				go s.triggerFulfillment(transactionID)
			}
			if changed && toState == StateFailed {
				metrics.RecordPaymentFailed("gateway_failure")
			}
		}
	}

	return s.GetStatus(ctx, transactionID)
}

// Plan represents a tariff plan from the catalog
type Plan struct {
	ID                 int64   `json:"id"`
	Name               string  `json:"name"`
	Price              int64   `json:"price"` // minor currency units (500 = R5.00)
	DisplayAmount      float64 `json:"display_amount"`
	Currency           string  `json:"currency"`
	DurationSeconds    int64   `json:"duration_seconds"`
	DurationDays       int64   `json:"duration_days"`
	DownloadSpeed      int64   `json:"download_speed"`
	UploadSpeed        int64   `json:"upload_speed"`
	MaxSessions        int64   `json:"max_sessions"`
	FupDataQuotaMb     int64   `json:"fup_data_quota_mb"`
	FupDownloadSpeed   int64   `json:"fup_download_speed"`
	FupUploadSpeed     int64   `json:"fup_upload_speed"`
	MarketingTagline   string  `json:"marketing_tagline,omitempty"`
}

func (s *Service) validateGatewayCurrency(ctx context.Context, gatewayCode, currency string) error {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM payments_gatewaysupportedcurrency gsc
		JOIN payments_paymentgateway pg ON pg.id = gsc.gateway_id
		JOIN services_supportedcurrency sc ON sc.id = gsc.currency_id
		WHERE pg.gateway_code = ? AND sc.code = ? AND gsc.is_active = 1 AND sc.is_active = 1
	`, gatewayCode, currency).Scan(&count)
	if err != nil {
		return fmt.Errorf("currency validation failed: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("currency %s not supported for gateway %s", currency, gatewayCode)
	}
	return nil
}

// resolveGatewayCode returns the gateway_code for a given gateway_id
func (s *Service) resolveGatewayCode(ctx context.Context, gatewayID int64) (string, error) {
	var code string
	err := s.db.QueryRowContext(ctx, `
		SELECT gateway_code FROM payments_paymentgateway WHERE id = ?
	`, gatewayID).Scan(&code)
	if err != nil {
		return "", fmt.Errorf("gateway not found: %w", err)
	}
	return code, nil
}

// resolveGatewayOrgID returns the organization_id from the gateway (0 if null).
func (s *Service) resolveGatewayOrgID(ctx context.Context, gatewayCode string, gatewayID *int64) (int64, error) {
	var orgID sql.NullInt64
	if gatewayID != nil {
		err := s.db.QueryRowContext(ctx, `
			SELECT organization_id FROM payments_paymentgateway WHERE id = ?
		`, *gatewayID).Scan(&orgID)
		if err != nil {
			return 0, err
		}
	} else {
		err := s.db.QueryRowContext(ctx, `
			SELECT organization_id FROM payments_paymentgateway WHERE gateway_code = ?
		`, gatewayCode).Scan(&orgID)
		if err != nil {
			return 0, err
		}
	}
	if orgID.Valid {
		return orgID.Int64, nil
	}
	return 0, nil
}

// calculateFee looks up the best-matching FeeSchedule and computes the fee for the amount.
func (s *Service) calculateFee(ctx context.Context, gatewayCode string, gatewayID *int64, amount float64) float64 {
	// Try org-specific, then gateway-specific, then platform-wide default
	// Order by priority DESC, take first match
	rows, err := s.db.QueryContext(ctx, `
		SELECT fs.fee_type, fs.percentage, fs.flat_amount
		FROM payments_feeschedule fs
		LEFT JOIN payments_paymentgateway g ON fs.gateway_id = g.id
		WHERE fs.is_active = 1
		  AND (fs.gateway_id IS NULL OR g.gateway_code = ?)
		ORDER BY fs.priority DESC
		LIMIT 1
	`, gatewayCode)
	if err != nil {
		return 0
	}
	defer rows.Close()

	if rows.Next() {
		var feeType string
		var percentage, flatAmount float64
		if err := rows.Scan(&feeType, &percentage, &flatAmount); err != nil {
			return 0
		}
		if feeType == "percentage" {
			return amount * percentage / 100.0
		}
		return flatAmount
	}
	return 0
}

func (s *Service) validatePlanAmount(ctx context.Context, planID, amount int64, currency string) error {
	var price int64
	var planCurrency string
	err := s.db.QueryRowContext(ctx, `
		SELECT price, currency FROM services_tariffplan WHERE id = ? AND is_active = 1
	`, planID).Scan(&price, &planCurrency)
	if err == sql.ErrNoRows {
		return fmt.Errorf("plan not found: %d", planID)
	}
	if err != nil {
		return fmt.Errorf("plan lookup failed: %w", err)
	}
	if price != amount {
		return fmt.Errorf("amount %d does not match plan price %d", amount, price)
	}
	if planCurrency != "" && currency != planCurrency {
		return fmt.Errorf("currency %s does not match plan currency %s", currency, planCurrency)
	}
	return nil
}

// ListPlans returns all active tariff plans ordered by price
func (s *Service) ListPlans(ctx context.Context) ([]*Plan, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id,
			COALESCE(NULLIF(package_label, ''), description, ''),
			price, currency, seconds, duration_days,
			download_speed, upload_speed, max_sessions,
			fup_data_quota_mb, fup_download_speed, fup_upload_speed,
			COALESCE(marketing_tagline, '')
		FROM services_tariffplan
		WHERE is_active = 1 AND (plan_kind IS NULL OR plan_kind != 'smoke')
		ORDER BY price ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query plans: %w", err)
	}
	defer rows.Close()

	var plans []*Plan
	for rows.Next() {
		var p Plan
		var label string
		err := rows.Scan(
			&p.ID, &label, &p.Price, &p.Currency, &p.DurationSeconds, &p.DurationDays,
			&p.DownloadSpeed, &p.UploadSpeed, &p.MaxSessions,
			&p.FupDataQuotaMb, &p.FupDownloadSpeed, &p.FupUploadSpeed,
			&p.MarketingTagline,
		)
		if err != nil {
			slog.Warn("failed to scan plan", slog.Any("error", err))
			continue
		}
		if label != "" {
			p.Name = label
		} else {
			p.Name = fmt.Sprintf("Plan %d", p.Price)
		}
		if p.Currency == "" {
			p.Currency = s.defaultCurrency
		}
		p.DisplayAmount = float64(p.Price) / 100.0
		plans = append(plans, &p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating plans: %w", err)
	}

	return plans, nil
}

// TriggerFulfillment exposes triggerFulfillment for external callers (e.g., poller).
func (s *Service) TriggerFulfillment(transactionID int64) {
	s.triggerFulfillment(transactionID)
}

// notifyDjangoCompletion POSTs to Django webhook to trigger commission processing.
func (s *Service) notifyDjangoCompletion(transactionID int64, referralCode string) {
	ctx := context.Background()

	// Look up customer_id for this transaction
	var customerID sql.NullInt64
	_ = s.db.QueryRowContext(ctx, `
		SELECT customer_id FROM payments_paymenttransaction WHERE id = ?
	`, transactionID).Scan(&customerID)

	payload := map[string]interface{}{
		"transaction_id": transactionID,
		"state":          "completed",
		"referral_code":  referralCode,
	}
	if customerID.Valid {
		payload["customer_id"] = customerID.Int64
	}

	body, _ := json.Marshal(payload)

	djangoBaseURL := os.Getenv("DJANGO_BASE_URL")
	if djangoBaseURL == "" {
		djangoBaseURL = "http://flash-api:8000"
	}
	internalKey := os.Getenv("DJANGO_INTERNAL_API_KEY")

	url := djangoBaseURL + "/api/sales-agents/webhooks/payment-completed/"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		slog.Error("webhook: failed to create request", slog.Any("error", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-API-Key", internalKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Error("webhook: failed to notify Django",
			slog.Int64("transaction_id", transactionID),
			slog.Any("error", err),
		)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Error("webhook: Django returned error",
			slog.Int64("transaction_id", transactionID),
			slog.Int("status", resp.StatusCode),
		)
		return
	}

	slog.Info("webhook: Django notified of payment completion",
		slog.Int64("transaction_id", transactionID),
		slog.Int("status", resp.StatusCode),
	)
}

// MethodDetails represents the method object nested in PaymentMethod
type MethodDetails struct {
	Code             string `json:"code"`
	DisplayName      string `json:"display_name"`
	RequiresPhone    bool   `json:"requires_phone"`
	RequiresRedirect bool   `json:"requires_redirect"`
}

// PaymentMethod represents a payment method from the database
type PaymentMethod struct {
	GatewayCode string          `json:"gateway_code"`
	Method      MethodDetails   `json:"method"`
}

// ListPaymentMethods returns active payment methods from the database, optionally filtered by gateway
func (s *Service) ListPaymentMethods(ctx context.Context, gatewayCode string) ([]*PaymentMethod, error) {
	query := `
		SELECT 
			pg.gateway_code,
			pm.method_code,
			pm.display_name,
			JSON_EXTRACT(pm.metadata, '$.requires_phone') = 'true' as requires_phone,
			JSON_EXTRACT(pm.metadata, '$.requires_redirect') = 'true' as requires_redirect
		FROM payments_paymentmethod pm
		JOIN payments_paymentgateway pg ON pm.gateway_id = pg.id
		WHERE pm.is_active = 1 AND pg.is_active = 1
	`
	
	var args []interface{}
	if gatewayCode != "" {
		query += " AND pg.gateway_code = ?"
		args = append(args, gatewayCode)
	}
	
	query += " ORDER BY pm.display_order ASC"
	
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query payment methods: %w", err)
	}
	defer rows.Close()

	var methods []*PaymentMethod
	for rows.Next() {
		var m PaymentMethod
		var requiresPhone, requiresRedirect sql.NullBool
		err := rows.Scan(
			&m.GatewayCode, &m.Method.Code, &m.Method.DisplayName,
			&requiresPhone, &requiresRedirect,
		)
		if err != nil {
			slog.Warn("failed to scan payment method", slog.Any("error", err))
			continue
		}
		m.Method.RequiresPhone = requiresPhone.Valid && requiresPhone.Bool
		m.Method.RequiresRedirect = requiresRedirect.Valid && requiresRedirect.Bool
		methods = append(methods, &m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating payment methods: %w", err)
	}

	return methods, nil
}

// GatewayFees represents the fee structure for a gateway
type GatewayFees struct {
	Percentage float64 `json:"percentage"`
	Fixed      float64 `json:"fixed"`
	Currency   string  `json:"currency"`
}

// Gateway represents a payment gateway from the database
type Gateway struct {
	ID            int64           `json:"id"` // F3.8: explicit gateway ID for org-scoped resolution
	Code          string          `json:"code"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Fees          GatewayFees     `json:"fees"`
	Features      []string        `json:"features"`
	IsActive      bool            `json:"is_active"`
}

// ListGateways returns active payment gateways with their fee structures
func (s *Service) ListGateways(ctx context.Context) ([]*Gateway, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT 
			id, gateway_code,
			name,
			description,
			configuration
		FROM payments_paymentgateway
		WHERE is_active = 1
		ORDER BY name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query gateways: %w", err)
	}
	defer rows.Close()

	var gateways []*Gateway
	for rows.Next() {
		var g Gateway
		var configJSON []byte
		err := rows.Scan(
			&g.ID, &g.Code, &g.Name, &g.Description, &configJSON,
		)
		if err != nil {
			slog.Warn("failed to scan gateway", slog.Any("error", err))
			continue
		}
		
		// Parse configuration JSON to extract fees and features
		if len(configJSON) > 0 {
			var config map[string]interface{}
			if err := json.Unmarshal(configJSON, &config); err == nil {
				// Extract fees
				if feesData, ok := config["fees"].(map[string]interface{}); ok {
					if pct, ok := feesData["percentage"].(float64); ok {
						g.Fees.Percentage = pct
					}
					if fixed, ok := feesData["fixed"].(float64); ok {
						g.Fees.Fixed = fixed
					}
					if curr, ok := feesData["currency"].(string); ok {
						g.Fees.Currency = curr
					}
				}
				// Extract features
				if featuresData, ok := config["features"].([]interface{}); ok {
					for _, f := range featuresData {
						if feat, ok := f.(string); ok {
							g.Features = append(g.Features, feat)
						}
					}
				}
			}
		}
		g.IsActive = true
		gateways = append(gateways, &g)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating gateways: %w", err)
	}

	return gateways, nil
}

// RefundRequest represents a refund request
// Refund processes a refund for a completed transaction.
func (s *Service) Refund(ctx context.Context, transactionID int64) error {
	// Fetch transaction details
	var externalRef, gatewayCode, currentState, voucherPin, currency string
	var amountFloat float64
	err := s.db.QueryRowContext(ctx, `
		SELECT t.external_reference, g.gateway_code, t.state, t.voucher_pin, t.amount, t.currency
		FROM payments_paymenttransaction t
		JOIN payments_paymentgateway g ON g.id = t.gateway_id
		WHERE t.id = ?
	`, transactionID).Scan(&externalRef, &gatewayCode, &currentState, &voucherPin, &amountFloat, &currency)

	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("transaction not found: %d", transactionID)
		}
		return fmt.Errorf("failed to query transaction: %w", err)
	}

	if currentState != "completed" {
		return fmt.Errorf("cannot refund transaction in state %s", currentState)
	}

	// Resolve gateway and check capabilities
	gateway, ok := s.registry.Resolve(gatewayCode)
	if !ok {
		return fmt.Errorf("gateway not found: %s", gatewayCode)
	}

	if !gateway.Capabilities().SupportsRefund {
		return fmt.Errorf("gateway %s does not support refunds", gatewayCode)
	}

	// Call gateway refund
	amountCents := int64(amountFloat * 100)
	_, err = gateway.Refund(ctx, externalRef, gateways.Money{
		Amount:   amountCents,
		Currency: currency,
	})
	if err != nil {
		return fmt.Errorf("gateway refund failed: %w", err)
	}

	// Transition state completed -> refunded
	changed, err := s.stateMachine.Transition(ctx, s.db, transactionID, StateCompleted, StateRefunded)
	if err != nil {
		return fmt.Errorf("state transition failed: %w", err)
	}
	if !changed {
		return fmt.Errorf("transaction already refunded")
	}

	// Remove radcheck rows (disable the voucher)
	if voucherPin != "" {
		if rbErr := s.fulfillmentService.Rollback(ctx, voucherPin); rbErr != nil {
			slog.Error("refund: failed to rollback radcheck",
				slog.Int64("transaction_id", transactionID),
				slog.Any("error", rbErr),
			)
			// Continue — radcheck cleanup failure is not fatal to refund
		}

		// Disconnect any active sessions using this voucher
		if s.djangoClient != nil {
			if discErr := s.disconnectSessions(ctx, voucherPin); discErr != nil {
				slog.Error("refund: CoA disconnect failed",
					slog.Int64("transaction_id", transactionID),
					slog.Any("error", discErr),
				)
				// Continue — disconnect failure is not fatal to refund
			}
		}
	}

	slog.Info("refund processed",
		slog.Int64("transaction_id", transactionID),
		slog.String("gateway", gatewayCode),
	)
	return nil
}

// Cancel cancels an initiated or pending transaction.
func (s *Service) Cancel(ctx context.Context, transactionID int64) error {
	var externalRef, gatewayCode, currentState string
	err := s.db.QueryRowContext(ctx, `
		SELECT t.external_reference, g.gateway_code, t.state
		FROM payments_paymenttransaction t
		JOIN payments_paymentgateway g ON g.id = t.gateway_id
		WHERE t.id = ?
	`, transactionID).Scan(&externalRef, &gatewayCode, &currentState)

	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("transaction not found: %d", transactionID)
		}
		return fmt.Errorf("failed to query transaction: %w", err)
	}

	if State(currentState).IsTerminal() {
		return fmt.Errorf("cannot cancel transaction in terminal state %s", currentState)
	}

	// Optional: notify gateway of cancellation if it supports such an operation.
	// The registry interface does not have Cancel, so we just transition state.

	changed, err := s.stateMachine.Transition(ctx, s.db, transactionID, State(currentState), StateCancelled)
	if err != nil {
		return fmt.Errorf("state transition failed: %w", err)
	}
	if !changed {
		return fmt.Errorf("transaction already cancelled")
	}

	slog.Info("transaction cancelled",
		slog.Int64("transaction_id", transactionID),
		slog.String("gateway", gatewayCode),
	)
	return nil
}

// disconnectSessions finds active radacct sessions for a voucher PIN and issues CoA disconnects.
func (s *Service) disconnectSessions(ctx context.Context, voucherPin string) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT nasipaddress FROM radacct
		WHERE username = ? AND acctstoptime IS NULL
	`, voucherPin)
	if err != nil {
		return fmt.Errorf("failed to query active sessions: %w", err)
	}
	defer rows.Close()

	var lastErr error
	for rows.Next() {
		var nasIP string
		if scanErr := rows.Scan(&nasIP); scanErr != nil {
			continue
		}
		if discErr := s.djangoClient.DisconnectUser(ctx, voucherPin, nasIP); discErr != nil {
			lastErr = discErr
		}
	}
	return lastErr
}

// GetWebhookLogs returns webhook audit log entries for a transaction.
func (s *Service) GetWebhookLogs(ctx context.Context, transactionID int64) ([]map[string]interface{}, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			id, gateway_code, signature_valid, processed, error,
			received_at, processed_at, external_reference
		FROM payments_paymentwebhooklog
		WHERE transaction_id = ?
		ORDER BY received_at DESC
	`, transactionID)
	if err != nil {
		return nil, fmt.Errorf("failed to query webhook logs: %w", err)
	}
	defer rows.Close()

	var logs []map[string]interface{}
	for rows.Next() {
		var id int64
		var gatewayCode, externalRef, errMsg string
		var sigValid, processed bool
		var receivedAt, processedAt sql.NullTime
		if scanErr := rows.Scan(&id, &gatewayCode, &sigValid, &processed, &errMsg, &receivedAt, &processedAt, &externalRef); scanErr != nil {
			continue
		}
		entry := map[string]interface{}{
			"id":                id,
			"gateway_code":      gatewayCode,
			"signature_valid":   sigValid,
			"processed":         processed,
			"error":             errMsg,
			"external_reference": externalRef,
		}
		if receivedAt.Valid {
			entry["received_at"] = receivedAt.Time
		}
		if processedAt.Valid {
			entry["processed_at"] = processedAt.Time
		}
		logs = append(logs, entry)
	}
	return logs, nil
}

// logWebhook writes an audit entry to PaymentWebhookLog.
func (s *Service) logWebhook(ctx context.Context, gatewayCode string, event gateways.WebhookEvent, rawBody []byte, headers http.Header, transactionID int64, signatureValid bool) error {
	headersJSON, _ := json.Marshal(headers)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO payments_paymentwebhooklog (
			gateway_code, raw_body, headers, signature_valid, processed,
			error, received_at, processed_at, transaction_id, external_reference
		)
		VALUES (?, ?, ?, ?, ?, ?, NOW(), NOW(), ?, ?)
	`, gatewayCode, rawBody, headersJSON, signatureValid, true, "", transactionID, event.ExternalReference)
	return err
}
