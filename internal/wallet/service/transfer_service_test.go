package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	apperr "github.com/Robustrade/wallet-transfer-assignment/common/error"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/registry"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/repo/iface"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/service"
)

// --- fakes (each implements one narrow, role-segregated interface) ---

type fakeProducer struct {
	calls     int
	onPublish func(domain.TransferRequest)
}

func (f *fakeProducer) Publish(_ context.Context, _, _ string, value []byte) error {
	f.calls++
	if f.onPublish != nil {
		req, _ := domain.DecodeTransferRequest(value)
		f.onPublish(req)
	}
	return nil
}

type fakeAccountReader struct {
	bal   domain.Balance
	found bool
}

func (f fakeAccountReader) Get(_ context.Context, _ string) (domain.Balance, bool, error) {
	return f.bal, f.found, nil
}

type fakeSagaFinder struct {
	s     iface.SagaState
	found bool
}

func (f fakeSagaFinder) FindByTransaction(_ context.Context, _ uuid.UUID) (iface.SagaState, bool, error) {
	return f.s, f.found, nil
}

type fakeOutcomeReader struct {
	o     iface.Outcome
	found bool
}

func (f fakeOutcomeReader) Get(_ context.Context, _ uuid.UUID) (iface.Outcome, bool, error) {
	return f.o, f.found, nil
}

func newSvc(p service.Producer, reg *registry.Registry, acc iface.AccountReader, sg iface.SagaFinder, out iface.OutcomeReader, wait time.Duration) service.TransferService {
	return service.New(service.Deps{
		Producer: p, Registry: reg, Accounts: acc, Sagas: sg, Outcomes: out, WaitTimeout: wait,
	})
}

func input() service.Input {
	return service.Input{TxID: uuid.New(), From: "alice", To: "bob", AmountMinor: 100, Currency: "USD"}
}

// --- tests ---

// Fresh transfer: produced, then the Saga resolves the waiter -> success.
// The fake producer resolves on publish, which the service does AFTER it has
// registered the waiter — so this is deterministic without sleeps.
func TestTransfer_FreshSuccess(t *testing.T) {
	reg := registry.New()
	p := &fakeProducer{onPublish: func(r domain.TransferRequest) { reg.Resolve(r.TransactionID, true, "") }}
	svc := newSvc(p, reg, fakeAccountReader{}, fakeSagaFinder{found: false}, fakeOutcomeReader{}, time.Second)

	res, err := svc.Transfer(context.Background(), input())
	require.NoError(t, err)
	require.Equal(t, service.StateSuccess, res.State)
	require.Equal(t, 1, p.calls)
}

func TestTransfer_FreshFailedResolvesWithReason(t *testing.T) {
	reg := registry.New()
	p := &fakeProducer{onPublish: func(r domain.TransferRequest) {
		reg.Resolve(r.TransactionID, false, domain.ReasonInsufficientFunds)
	}}
	svc := newSvc(p, reg, fakeAccountReader{}, fakeSagaFinder{found: false}, fakeOutcomeReader{}, time.Second)

	res, err := svc.Transfer(context.Background(), input())
	require.NoError(t, err)
	require.Equal(t, service.StateFailed, res.State)
	require.Equal(t, domain.ReasonInsufficientFunds, res.Reason)
}

// No resolution before the push deadline -> pending (client polls).
func TestTransfer_TimeoutPending(t *testing.T) {
	reg := registry.New()
	p := &fakeProducer{} // never resolves
	svc := newSvc(p, reg, fakeAccountReader{}, fakeSagaFinder{found: false}, fakeOutcomeReader{}, 20*time.Millisecond)

	res, err := svc.Transfer(context.Background(), input())
	require.NoError(t, err)
	require.Equal(t, service.StatePending, res.State)
}

// Same transaction_id, different body -> conflict, nothing produced.
func TestTransfer_IdempotencyConflict(t *testing.T) {
	reg := registry.New()
	p := &fakeProducer{}
	existing := iface.SagaState{FromAccount: "someone-else", ToAccount: "bob", AmountMinor: 100, Currency: "USD"}
	svc := newSvc(p, reg, fakeAccountReader{}, fakeSagaFinder{s: existing, found: true}, fakeOutcomeReader{}, time.Second)

	_, err := svc.Transfer(context.Background(), input())
	require.ErrorIs(t, err, apperr.ErrIdempotencyConflict)
	require.Equal(t, 0, p.calls, "must not produce on conflict")
}

// Replay of a settled transfer returns the original outcome without producing.
func TestTransfer_ReplaySettled(t *testing.T) {
	reg := registry.New()
	p := &fakeProducer{}
	in := input()
	existing := iface.SagaState{FromAccount: in.From, ToAccount: in.To, AmountMinor: in.AmountMinor, Currency: in.Currency}
	out := iface.Outcome{Status: iface.OutcomeSuccess}
	svc := newSvc(p, reg, fakeAccountReader{}, fakeSagaFinder{s: existing, found: true}, fakeOutcomeReader{o: out, found: true}, time.Second)

	res, err := svc.Transfer(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, service.StateSuccess, res.State)
	require.Equal(t, 0, p.calls, "replay must not produce")
}

func TestBalance_NotFound(t *testing.T) {
	svc := newSvc(&fakeProducer{}, registry.New(), fakeAccountReader{found: false}, fakeSagaFinder{}, fakeOutcomeReader{}, time.Second)
	_, err := svc.Balance(context.Background(), "ghost")
	require.ErrorIs(t, err, apperr.ErrAccountNotFound)
}

func TestStatus(t *testing.T) {
	// Settled -> outcome.
	svc := newSvc(&fakeProducer{}, registry.New(), fakeAccountReader{},
		fakeSagaFinder{}, fakeOutcomeReader{o: iface.Outcome{Status: iface.OutcomeFailed, FailureReason: "X"}, found: true}, time.Second)
	res, err := svc.Status(context.Background(), uuid.New())
	require.NoError(t, err)
	require.Equal(t, service.StateFailed, res.State)
	require.Equal(t, "X", res.Reason)

	// In flight (saga exists, no outcome) -> pending.
	svc = newSvc(&fakeProducer{}, registry.New(), fakeAccountReader{},
		fakeSagaFinder{found: true}, fakeOutcomeReader{found: false}, time.Second)
	res, err = svc.Status(context.Background(), uuid.New())
	require.NoError(t, err)
	require.Equal(t, service.StatePending, res.State)

	// Unknown -> error.
	svc = newSvc(&fakeProducer{}, registry.New(), fakeAccountReader{}, fakeSagaFinder{found: false}, fakeOutcomeReader{found: false}, time.Second)
	_, err = svc.Status(context.Background(), uuid.New())
	require.True(t, errors.Is(err, apperr.ErrAccountNotFound))
}
