// Package service is the business-logic layer between the HTTP handler and the
// repositories. It owns the transfer workflow: idempotency, producing the
// transfer onto the log, and the push-model wait for the outcome. Handlers stay
// thin (transport only); repositories stay persistence-only.
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	apperr "github.com/Robustrade/wallet-transfer-assignment/common/error"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
	walletkafka "github.com/Robustrade/wallet-transfer-assignment/internal/wallet/kafka"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/registry"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/repo/iface"
)

// State is the client-visible state of a transfer.
type State string

// Transfer states returned to the client.
const (
	StateSuccess State = "success"
	StateFailed  State = "failed"
	StatePending State = "pending"
)

// Result is the outcome of a transfer or status query.
type Result struct {
	State  State
	Reason string
}

// Input is a validated transfer request (parsed by the handler).
type Input struct {
	TxID        uuid.UUID
	From        string
	To          string
	AmountMinor int64
	Currency    string
}

// Producer is the subset of the Kafka producer this layer needs.
type Producer interface {
	Publish(ctx context.Context, topic, key string, value []byte) error
}

// TransferService is the business-logic contract the handler depends on.
type TransferService interface {
	Transfer(ctx context.Context, in Input) (Result, error)
	Balance(ctx context.Context, account string) (domain.Balance, error)
	Status(ctx context.Context, txID uuid.UUID) (Result, error)
}

type transferService struct {
	producer    Producer
	registry    *registry.Registry
	accounts    iface.AccountReader
	sagas       iface.SagaFinder
	outcomes    iface.OutcomeReader
	logger      *slog.Logger
	waitTimeout time.Duration
}

// Deps groups the service dependencies. Each is a narrow, read-only repository
// view (ISP): the service queries balances, looks up sagas for idempotency, and
// reads settled outcomes — it never writes through these.
type Deps struct {
	Producer    Producer
	Registry    *registry.Registry
	Accounts    iface.AccountReader
	Sagas       iface.SagaFinder
	Outcomes    iface.OutcomeReader
	Logger      *slog.Logger
	WaitTimeout time.Duration
}

// New constructs a TransferService.
func New(d Deps) TransferService {
	if d.WaitTimeout <= 0 {
		d.WaitTimeout = 5 * time.Second
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &transferService{
		producer:    d.Producer,
		registry:    d.Registry,
		accounts:    d.Accounts,
		sagas:       d.Sagas,
		outcomes:    d.Outcomes,
		logger:      d.Logger,
		waitTimeout: d.WaitTimeout,
	}
}

// Transfer runs the transfer workflow. A repeat of an existing transaction_id is
// handled idempotently: a mismatched body is a conflict, a settled one returns
// the original outcome, an in-flight one is awaited without re-producing.
func (s *transferService) Transfer(ctx context.Context, in Input) (Result, error) {
	if existing, found, err := s.sagas.FindByTransaction(ctx, in.TxID); err == nil && found {
		if existing.FromAccount != in.From || existing.ToAccount != in.To ||
			existing.AmountMinor != in.AmountMinor || existing.Currency != in.Currency {
			return Result{}, apperr.ErrIdempotencyConflict
		}
		if o, ok, _ := s.outcomes.Get(ctx, in.TxID); ok {
			return outcomeResult(o), nil
		}
		return s.await(ctx, in.TxID, false, domain.TransferRequest{})
	}
	return s.await(ctx, in.TxID, true, domain.TransferRequest{
		TransactionID: in.TxID,
		FromAccount:   in.From,
		ToAccount:     in.To,
		AmountMinor:   in.AmountMinor,
		Currency:      in.Currency,
		RequestedAt:   time.Now().UTC(),
	})
}

// await registers the push waiter (before producing, to avoid a resolve racing
// ahead of registration), optionally produces the transfer, then blocks for the
// outcome until the push deadline elapses (-> pending, client polls Status).
func (s *transferService) await(ctx context.Context, txID uuid.UUID, produce bool, req domain.TransferRequest) (Result, error) {
	ch := s.registry.Register(txID)

	if produce {
		value, err := req.Encode()
		if err != nil {
			s.registry.Cancel(txID)
			return Result{}, apperr.ErrInternal.WithWrap(err)
		}
		if err := s.producer.Publish(ctx, walletkafka.TopicTransfers, walletkafka.PartitionKey(txID.String()), value); err != nil {
			s.registry.Cancel(txID)
			return Result{}, apperr.ErrInternal.WithWrap(err)
		}
	}

	select {
	case o := <-ch:
		if o.Success {
			return Result{State: StateSuccess}, nil
		}
		return Result{State: StateFailed, Reason: o.Reason}, nil
	case <-time.After(s.waitTimeout):
		s.registry.Cancel(txID)
		return Result{State: StatePending}, nil
	case <-ctx.Done():
		s.registry.Cancel(txID)
		return Result{}, ctx.Err()
	}
}

// Balance returns an account's materialized balance from the read model.
func (s *transferService) Balance(ctx context.Context, account string) (domain.Balance, error) {
	bal, found, err := s.accounts.Get(ctx, account)
	if err != nil {
		return domain.Balance{}, err
	}
	if !found {
		return domain.Balance{}, apperr.ErrAccountNotFound
	}
	return bal, nil
}

// Status reports the settled outcome of a transaction, or pending if a saga
// exists but has not settled, for the 202 poll path.
func (s *transferService) Status(ctx context.Context, txID uuid.UUID) (Result, error) {
	if o, ok, err := s.outcomes.Get(ctx, txID); err == nil && ok {
		return outcomeResult(o), nil
	}
	if _, ok, err := s.sagas.FindByTransaction(ctx, txID); err == nil && ok {
		return Result{State: StatePending}, nil
	}
	return Result{}, apperr.ErrAccountNotFound.WithMessage("transaction not found")
}

func outcomeResult(o iface.Outcome) Result {
	if o.Status == iface.OutcomeSuccess {
		return Result{State: StateSuccess}
	}
	return Result{State: StateFailed, Reason: o.FailureReason}
}
