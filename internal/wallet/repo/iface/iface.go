// Package iface declares the persistence contracts (ports) for the wallet's
// state. Contracts are split by ROLE (Interface Segregation): each consumer
// depends only on what it uses. Concrete adapters live in repo/postgres; tests
// use in-memory fakes.
package iface

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
)

// SagaStatus is the overall lifecycle status of a transfer's saga.
type SagaStatus string

// Saga lifecycle statuses.
const (
	SagaPending      SagaStatus = "PENDING"
	SagaCompleted    SagaStatus = "COMPLETED"
	SagaFailed       SagaStatus = "FAILED"
	SagaCompensating SagaStatus = "COMPENSATING"
	SagaCompensated  SagaStatus = "COMPENSATED"
)

// LegStatus is the status of a single leg (debit or credit) of a transfer.
type LegStatus string

// Leg statuses.
const (
	LegPending  LegStatus = "PENDING"
	LegDone     LegStatus = "DONE"
	LegRejected LegStatus = "REJECTED"
)

// CompensateStatus is the status of the compensating (refund) step.
type CompensateStatus string

// Compensation statuses.
const (
	CompNA      CompensateStatus = "NA"
	CompPending CompensateStatus = "PENDING"
	CompDone    CompensateStatus = "DONE"
)

// SagaState is one row of the phase-status table. State transitions are applied
// as atomic compare-and-set UPDATEs in the store (see SagaStore), so concurrent
// saga instances consuming a transfer's two legs cannot lose updates.
type SagaState struct {
	SagaID        uuid.UUID
	TransactionID uuid.UUID
	FromAccount   string
	ToAccount     string
	AmountMinor   int64
	Currency      string
	Status        SagaStatus
	DebitStatus   LegStatus
	CreditStatus  LegStatus
	CompStatus    CompensateStatus
	FailureReason string
}

// OutcomeStatus is the terminal client-visible result of a transfer.
type OutcomeStatus string

// Terminal outcome statuses.
const (
	OutcomeSuccess OutcomeStatus = "SUCCESS"
	OutcomeFailed  OutcomeStatus = "FAILED"
)

// Outcome is one row of transaction_outcomes.
type Outcome struct {
	TransactionID uuid.UUID
	Status        OutcomeStatus
	FailureReason string
	SettledAt     time.Time
}

// --- write side (command-processor) ---

// CommandApplier atomically applies a leg command to the authoritative account
// (under a row lock), records command-dedup, and writes the event to the outbox,
// returning the event to publish. Implemented by *postgres.CommandStore.
type CommandApplier interface {
	ApplyCommand(ctx context.Context, cmd domain.Command) (domain.Event, error)
	MarkPublished(ctx context.Context, eventID uuid.UUID) error
}

// OutboxRelay drains the transactional outbox (backstop for crashes), claiming
// rows older than olderThan with SKIP LOCKED so multiple relays share the work.
type OutboxRelay interface {
	ClaimUnpublished(ctx context.Context, olderThan time.Duration, limit int) ([]domain.Event, error)
	MarkPublished(ctx context.Context, eventID uuid.UUID) error
}

// --- read side (gateway, projector) ---

// AccountReader reads the authoritative balance (gateway query side).
type AccountReader interface {
	Get(ctx context.Context, account string) (domain.Balance, bool, error)
}

// LedgerWriter builds the double-entry ledger view from events (projector),
// idempotently.
type LedgerWriter interface {
	AppendFromEvent(ctx context.Context, e domain.Event) error
}

// --- saga phase-status store (atomic CAS transitions) ---

// SagaStore persists the phase-status table. Each transition is an atomic
// conditional UPDATE that returns the row and whether it advanced (RowsAffected
// == 1), making the coordinator concurrency-safe and idempotent across instances.
type SagaStore interface {
	// Create inserts a new PENDING saga, or returns the existing row (created=false)
	// for a retried transaction_id.
	Create(ctx context.Context, s SagaState) (existing SagaState, created bool, err error)
	// FindByTransaction loads a saga by client transaction id (idempotency).
	FindByTransaction(ctx context.Context, txID uuid.UUID) (SagaState, bool, error)

	// CAS transitions: (state, advanced, error). advanced=false ⇒ already in/past
	// that state (idempotent no-op).
	MarkDebitDone(ctx context.Context, sagaID uuid.UUID) (SagaState, bool, error)
	FailDebit(ctx context.Context, sagaID uuid.UUID, reason string) (SagaState, bool, error)
	Complete(ctx context.Context, sagaID uuid.UUID) (SagaState, bool, error)
	BeginCompensation(ctx context.Context, sagaID uuid.UUID, reason string) (SagaState, bool, error)
	MarkCompensated(ctx context.Context, sagaID uuid.UUID) (SagaState, bool, error)

	// SweepStale claims up to limit non-terminal sagas idle longer than olderThan
	// (FOR UPDATE SKIP LOCKED), for re-drive across instances + stuck detection.
	SweepStale(ctx context.Context, olderThan time.Duration, limit int) ([]SagaState, error)
}

// SagaFinder is the read-only subset used by the gateway/service for idempotency.
type SagaFinder interface {
	FindByTransaction(ctx context.Context, txID uuid.UUID) (SagaState, bool, error)
}

// --- terminal transaction outcomes ---

// OutcomeWriter records terminal results (saga coordinator).
type OutcomeWriter interface {
	Upsert(ctx context.Context, o Outcome) error
}

// OutcomeReader reads terminal results (gateway/service).
type OutcomeReader interface {
	Get(ctx context.Context, txID uuid.UUID) (Outcome, bool, error)
}
