package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// State represents a payment transaction state
type State string

const (
	StateInitiated State = "initiated"
	StatePending   State = "pending"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
	StateRefunded  State = "refunded"
)

// Valid returns true if the state is valid
func (s State) Valid() bool {
	switch s {
	case StateInitiated, StatePending, StateCompleted, StateFailed, StateCancelled, StateRefunded:
		return true
	}
	return false
}

// IsTerminal returns true if the state is terminal (no further transitions allowed)
func (s State) IsTerminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled, StateRefunded:
		return true
	}
	return false
}

// String returns the string representation
func (s State) String() string {
	return string(s)
}

// StateMachine handles valid state transitions
type StateMachine struct {
	// transitions maps current state -> allowed next states
	transitions map[State][]State
}

// NewStateMachine creates a new state machine with the standard transition rules
func NewStateMachine() *StateMachine {
	return &StateMachine{
		transitions: map[State][]State{
			StateInitiated: {StatePending, StateCompleted, StateFailed, StateCancelled},
			StatePending:   {StateCompleted, StateFailed, StateCancelled},
			StateCompleted: {StateRefunded, StateFailed}, // Failed allows rollback on fulfillment error
			StateFailed:    {},
			StateCancelled: {},
			StateRefunded:  {},
		},
	}
}

// CanTransition returns true if transitioning from -> to is valid
func (sm *StateMachine) CanTransition(from, to State) bool {
	allowed, ok := sm.transitions[from]
	if !ok {
		return false
	}
	
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// Transition attempts to transition a transaction to a new state
// It returns true if the transition was applied, false if it was already in that state
// Returns an error if the transition is invalid
func (sm *StateMachine) Transition(ctx context.Context, db *sql.DB, transactionID int64, from, to State) (bool, error) {
	// Validate states
	if !from.Valid() {
		return false, fmt.Errorf("invalid source state: %s", from)
	}
	if !to.Valid() {
		return false, fmt.Errorf("invalid target state: %s", to)
	}
	
	// Same-state is always a no-op
	if from == to {
		return false, nil
	}
	
	// Check if transition is valid
	if !sm.CanTransition(from, to) {
		return false, fmt.Errorf("invalid state transition: %s -> %s", from, to)
	}
	
	// Attempt the transition in the database
	// This is idempotent - if already in target state, no error
	result, err := sm.applyTransition(ctx, db, transactionID, from, to)
	if err != nil {
		return false, fmt.Errorf("failed to apply transition: %w", err)
	}
	
	return result, nil
}

// applyTransition applies the state transition atomically
// Returns true if state was changed, false if already in target state
func (sm *StateMachine) applyTransition(ctx context.Context, db *sql.DB, transactionID int64, from, to State) (bool, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	
	// Lock the row and get current state
	var currentState string
	var currentAmount int64
	err = tx.QueryRowContext(ctx, `
		SELECT state, CAST(amount * 100 AS SIGNED) as amount_cents
		FROM payments_paymenttransaction 
		WHERE id = ? 
		FOR UPDATE
	`, transactionID).Scan(&currentState, &currentAmount)
	
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("transaction not found: %d", transactionID)
		}
		return false, fmt.Errorf("failed to query transaction: %w", err)
	}
	
	// If already in target state, return false (no change needed)
	if State(currentState) == to {
		return false, nil
	}
	
	// Verify we're transitioning from the expected state
	if State(currentState) != from {
		return false, fmt.Errorf("transaction state mismatch: expected %s, got %s", from, currentState)
	}
	
	// Build update query
	updateQuery := `
		UPDATE payments_paymenttransaction 
		SET state = ?, status = ?, updated_at = NOW()`
	args := []interface{}{to.String(), to.String()}
	
	// Set completed_at for terminal success states
	if to == StateCompleted {
		updateQuery += `, completed_at = NOW()`
	}
	
	updateQuery += ` WHERE id = ?`
	args = append(args, transactionID)
	
	// Execute update
	result, err := tx.ExecContext(ctx, updateQuery, args...)
	if err != nil {
		return false, fmt.Errorf("failed to update transaction state: %w", err)
	}
	
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}
	
	if rowsAffected != 1 {
		return false, fmt.Errorf("unexpected rows affected: %d", rowsAffected)
	}
	
	// Commit transaction
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit transaction: %w", err)
	}
	
	slog.Info("state transition applied",
		slog.Int64("transaction_id", transactionID),
		slog.String("from", from.String()),
		slog.String("to", to.String()),
	)
	
	return true, nil
}

// GetState retrieves the current state of a transaction
func (sm *StateMachine) GetState(ctx context.Context, db *sql.DB, transactionID int64) (State, error) {
	var state string
	err := db.QueryRowContext(ctx, `
		SELECT state 
		FROM payments_paymenttransaction 
		WHERE id = ?
	`, transactionID).Scan(&state)
	
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("transaction not found: %d", transactionID)
		}
		return "", fmt.Errorf("failed to query state: %w", err)
	}
	
	return State(state), nil
}

// IsValidTransition returns true if the transition is valid without applying it
func (sm *StateMachine) IsValidTransition(from, to State) bool {
	return sm.CanTransition(from, to)
}

// GetAllowedTransitions returns all states that can be transitioned to from the given state
func (sm *StateMachine) GetAllowedTransitions(from State) []State {
	allowed, ok := sm.transitions[from]
	if !ok {
		return nil
	}
	
	// Return a copy to prevent external modification
	result := make([]State, len(allowed))
	copy(result, allowed)
	return result
}

// TransactionStateInfo contains state information for a transaction
type TransactionStateInfo struct {
	ID            int64     `json:"id"`
	State         State     `json:"state"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	LastPolledAt  *time.Time `json:"last_polled_at,omitempty"`
}

// GetTransactionStateInfo retrieves full state information for a transaction
func GetTransactionStateInfo(ctx context.Context, db *sql.DB, transactionID int64) (*TransactionStateInfo, error) {
	var info TransactionStateInfo
	var completedAt, lastPolledAt sql.NullTime
	
	err := db.QueryRowContext(ctx, `
		SELECT id, state, created_at, updated_at, completed_at, last_polled_at
		FROM payments_paymenttransaction 
		WHERE id = ?
	`, transactionID).Scan(
		&info.ID,
		&info.State,
		&info.CreatedAt,
		&info.UpdatedAt,
		&completedAt,
		&lastPolledAt,
	)
	
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("transaction not found: %d", transactionID)
		}
		return nil, fmt.Errorf("failed to query transaction: %w", err)
	}
	
	if completedAt.Valid {
		info.CompletedAt = &completedAt.Time
	}
	if lastPolledAt.Valid {
		info.LastPolledAt = &lastPolledAt.Time
	}
	
	return &info, nil
}
