// Package saga implements the orchestration-mode Saga coordinator that makes a
// two-account transfer atomic across partitions without a distributed lock.
//
// It sequences the legs in valid order — DEBIT the source first, then CREDIT the
// destination — and compensates (refunds the source) if the credit is rejected
// after the debit committed. State lives in saga_transactions and every
// transition is an atomic compare-and-set in the store, so **multiple saga
// instances** consuming a transfer's two legs (on different partitions) cannot
// lose updates: exactly one instance's CAS advances the saga, the rest no-op.
package saga

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
	walletkafka "github.com/Robustrade/wallet-transfer-assignment/internal/wallet/kafka"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/repo/iface"
)

// Producer is the subset of the Kafka producer the coordinator needs.
type Producer interface {
	Publish(ctx context.Context, topic, key string, value []byte) error
}

// Resolver wakes a blocked gateway handler with a terminal outcome (push model).
// Optional — nil in split deployments, where the gateway polls instead.
type Resolver interface {
	Resolve(txID uuid.UUID, success bool, reason string)
}

// Coordinator orchestrates transfers.
type Coordinator struct {
	brokers  []string
	sagas    iface.SagaStore
	outcomes iface.OutcomeWriter
	producer Producer
	resolver Resolver
	logger   *slog.Logger
}

// Deps groups the coordinator's dependencies.
type Deps struct {
	Brokers  []string
	Sagas    iface.SagaStore
	Outcomes iface.OutcomeWriter
	Producer Producer
	Resolver Resolver // may be nil
	Logger   *slog.Logger
}

// New constructs a Coordinator.
func New(d Deps) *Coordinator {
	return &Coordinator{
		brokers:  d.Brokers,
		sagas:    d.Sagas,
		outcomes: d.Outcomes,
		producer: d.Producer,
		resolver: d.Resolver,
		logger:   d.Logger,
	}
}

// Start begins (or idempotently re-observes) a transfer. A retry with the same
// transaction_id never starts a second transfer.
func (c *Coordinator) Start(ctx context.Context, req domain.TransferRequest) error {
	s := iface.SagaState{
		SagaID:        uuid.New(),
		TransactionID: req.TransactionID,
		FromAccount:   req.FromAccount,
		ToAccount:     req.ToAccount,
		AmountMinor:   req.AmountMinor,
		Currency:      req.Currency,
	}
	existing, created, err := c.sagas.Create(ctx, s)
	if err != nil {
		return err
	}
	if !created {
		c.handleExisting(existing)
		return nil
	}
	return c.emit(ctx, existing, domain.LegDebit, existing.FromAccount, existing.ToAccount)
}

// handleExisting handles a duplicate Start: re-deliver a terminal outcome; leave
// in-flight sagas to their own events / the sweeper.
func (c *Coordinator) handleExisting(s iface.SagaState) {
	switch s.Status {
	case iface.SagaCompleted:
		c.resolve(s.TransactionID, true, "")
	case iface.SagaFailed, iface.SagaCompensated:
		c.resolve(s.TransactionID, false, s.FailureReason)
	default:
		// in flight — nothing to do
	}
}

// OnEvent advances the saga in response to a leg event via atomic CAS.
func (c *Coordinator) OnEvent(ctx context.Context, e domain.Event) error {
	if e.SagaID == uuid.Nil {
		return nil // not part of a transfer (e.g. a seed credit)
	}
	switch e.Leg {
	case domain.LegDebit:
		return c.onDebit(ctx, e)
	case domain.LegCredit:
		return c.onCredit(ctx, e)
	case domain.LegCompensate:
		return c.onCompensate(ctx, e)
	}
	return nil
}

func (c *Coordinator) onDebit(ctx context.Context, e domain.Event) error {
	if e.Type == domain.EventRejected {
		s, advanced, err := c.sagas.FailDebit(ctx, e.SagaID, e.Reason)
		if err != nil || !advanced {
			return err
		}
		return c.finalize(ctx, s, iface.OutcomeFailed, e.Reason)
	}
	s, advanced, err := c.sagas.MarkDebitDone(ctx, e.SagaID)
	if err != nil || !advanced {
		return err
	}
	return c.emit(ctx, s, domain.LegCredit, s.ToAccount, s.FromAccount)
}

func (c *Coordinator) onCredit(ctx context.Context, e domain.Event) error {
	if e.Type == domain.EventRejected {
		s, advanced, err := c.sagas.BeginCompensation(ctx, e.SagaID, e.Reason)
		if err != nil || !advanced {
			return err
		}
		return c.emit(ctx, s, domain.LegCompensate, s.FromAccount, s.ToAccount)
	}
	s, advanced, err := c.sagas.Complete(ctx, e.SagaID)
	if err != nil || !advanced {
		return err
	}
	return c.finalize(ctx, s, iface.OutcomeSuccess, "")
}

func (c *Coordinator) onCompensate(ctx context.Context, e domain.Event) error {
	if e.Type == domain.EventRejected {
		// A compensating credit should never be rejected; surface loudly.
		c.logger.ErrorContext(ctx, "saga: compensation rejected — manual intervention required",
			"tx", e.TransactionID, "reason", e.Reason)
		return nil
	}
	s, advanced, err := c.sagas.MarkCompensated(ctx, e.SagaID)
	if err != nil || !advanced {
		return err
	}
	return c.finalize(ctx, s, iface.OutcomeFailed, s.FailureReason)
}

// finalize records the terminal outcome and resolves the waiting caller. The CAS
// transition already persisted the saga, so there is no separate save.
func (c *Coordinator) finalize(ctx context.Context, s iface.SagaState, status iface.OutcomeStatus, reason string) error {
	if err := c.settle(ctx, s.TransactionID, status, reason); err != nil {
		return err
	}
	c.resolve(s.TransactionID, status == iface.OutcomeSuccess, reason)
	return nil
}

// driveNext re-emits whichever leg an in-flight saga is waiting on (used by the
// sweeper). Every emit is idempotent downstream.
func (c *Coordinator) driveNext(ctx context.Context, s iface.SagaState) error {
	switch {
	case s.DebitStatus == iface.LegPending:
		return c.emit(ctx, s, domain.LegDebit, s.FromAccount, s.ToAccount)
	case s.DebitStatus == iface.LegDone && s.CreditStatus == iface.LegPending:
		return c.emit(ctx, s, domain.LegCredit, s.ToAccount, s.FromAccount)
	case s.CompStatus == iface.CompPending:
		return c.emit(ctx, s, domain.LegCompensate, s.FromAccount, s.ToAccount)
	}
	return nil
}

// emit produces a leg command keyed by the account it mutates.
func (c *Coordinator) emit(ctx context.Context, s iface.SagaState, leg domain.Leg, account, counterparty string) error {
	cmd := domain.Command{
		TransactionID: s.TransactionID,
		SagaID:        s.SagaID,
		Leg:           leg,
		Account:       account,
		Counterparty:  counterparty,
		AmountMinor:   s.AmountMinor,
		Currency:      s.Currency,
		IssuedAt:      time.Now().UTC(),
	}
	value, err := cmd.Encode()
	if err != nil {
		return err
	}
	return c.producer.Publish(ctx, walletkafka.TopicCommands, walletkafka.PartitionKey(account), value)
}

func (c *Coordinator) settle(ctx context.Context, txID uuid.UUID, status iface.OutcomeStatus, reason string) error {
	return c.outcomes.Upsert(ctx, iface.Outcome{TransactionID: txID, Status: status, FailureReason: reason})
}

func (c *Coordinator) resolve(txID uuid.UUID, success bool, reason string) {
	if c.resolver != nil {
		c.resolver.Resolve(txID, success, reason)
	}
}
