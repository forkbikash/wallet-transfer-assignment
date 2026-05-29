package saga_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/repo/iface"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/saga"
)

// --- in-memory fakes ---

type fakeSagaRepo struct {
	mu   sync.Mutex
	byID map[uuid.UUID]iface.SagaState
	byTx map[uuid.UUID]uuid.UUID
}

func newFakeSagaRepo() *fakeSagaRepo {
	return &fakeSagaRepo{byID: map[uuid.UUID]iface.SagaState{}, byTx: map[uuid.UUID]uuid.UUID{}}
}

func (f *fakeSagaRepo) Create(_ context.Context, s iface.SagaState) (iface.SagaState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.byTx[s.TransactionID]; ok {
		return f.byID[id], false, nil
	}
	s.Status = iface.SagaPending
	s.DebitStatus = iface.LegPending
	s.CreditStatus = iface.LegPending
	s.CompStatus = iface.CompNA
	f.byID[s.SagaID] = s
	f.byTx[s.TransactionID] = s.SagaID
	return s, true, nil
}

func (f *fakeSagaRepo) FindByTransaction(_ context.Context, tx uuid.UUID) (iface.SagaState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byTx[tx]
	if !ok {
		return iface.SagaState{}, false, nil
	}
	return f.byID[id], true, nil
}

// cas applies a guarded transition in-memory, mirroring the SQL CAS.
func (f *fakeSagaRepo) cas(id uuid.UUID, pre func(iface.SagaState) bool, mut func(*iface.SagaState)) (iface.SagaState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.byID[id]
	if !pre(s) {
		return s, false, nil
	}
	mut(&s)
	f.byID[id] = s
	return s, true, nil
}

func (f *fakeSagaRepo) MarkDebitDone(_ context.Context, id uuid.UUID) (iface.SagaState, bool, error) {
	return f.cas(id,
		func(s iface.SagaState) bool { return s.DebitStatus == iface.LegPending },
		func(s *iface.SagaState) { s.DebitStatus = iface.LegDone })
}

func (f *fakeSagaRepo) FailDebit(_ context.Context, id uuid.UUID, reason string) (iface.SagaState, bool, error) {
	return f.cas(id,
		func(s iface.SagaState) bool { return s.Status == iface.SagaPending },
		func(s *iface.SagaState) {
			s.DebitStatus = iface.LegRejected
			s.Status = iface.SagaFailed
			s.FailureReason = reason
		})
}

func (f *fakeSagaRepo) Complete(_ context.Context, id uuid.UUID) (iface.SagaState, bool, error) {
	return f.cas(id,
		func(s iface.SagaState) bool {
			return s.Status == iface.SagaPending && s.CreditStatus == iface.LegPending
		},
		func(s *iface.SagaState) {
			s.CreditStatus = iface.LegDone
			s.Status = iface.SagaCompleted
		})
}

func (f *fakeSagaRepo) BeginCompensation(_ context.Context, id uuid.UUID, reason string) (iface.SagaState, bool, error) {
	return f.cas(id,
		func(s iface.SagaState) bool { return s.CompStatus == iface.CompNA },
		func(s *iface.SagaState) {
			s.CreditStatus = iface.LegRejected
			s.Status = iface.SagaCompensating
			s.CompStatus = iface.CompPending
			s.FailureReason = reason
		})
}

func (f *fakeSagaRepo) MarkCompensated(_ context.Context, id uuid.UUID) (iface.SagaState, bool, error) {
	return f.cas(id,
		func(s iface.SagaState) bool { return s.Status == iface.SagaCompensating },
		func(s *iface.SagaState) {
			s.CompStatus = iface.CompDone
			s.Status = iface.SagaCompensated
		})
}

func (f *fakeSagaRepo) SweepStale(context.Context, time.Duration, int) ([]iface.SagaState, error) {
	return nil, nil
}

type fakeOutcomeRepo struct {
	mu sync.Mutex
	m  map[uuid.UUID]iface.Outcome
}

func newFakeOutcomeRepo() *fakeOutcomeRepo { return &fakeOutcomeRepo{m: map[uuid.UUID]iface.Outcome{}} }

func (f *fakeOutcomeRepo) Upsert(_ context.Context, o iface.Outcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.m[o.TransactionID]; !ok {
		f.m[o.TransactionID] = o
	}
	return nil
}

func (f *fakeOutcomeRepo) Get(_ context.Context, tx uuid.UUID) (iface.Outcome, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.m[tx]
	return o, ok, nil
}

type fakeProducer struct {
	mu   sync.Mutex
	cmds []domain.Command
}

func (f *fakeProducer) Publish(_ context.Context, _, _ string, value []byte) error {
	cmd, err := domain.DecodeCommand(value)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	return nil
}

func (f *fakeProducer) last() domain.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cmds[len(f.cmds)-1]
}

func (f *fakeProducer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cmds)
}

type resolved struct {
	success bool
	reason  string
	ok      bool
}

type fakeResolver struct {
	mu sync.Mutex
	r  map[uuid.UUID]resolved
}

func newFakeResolver() *fakeResolver { return &fakeResolver{r: map[uuid.UUID]resolved{}} }

func (f *fakeResolver) Resolve(tx uuid.UUID, success bool, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.r[tx] = resolved{success: success, reason: reason, ok: true}
}

func (f *fakeResolver) get(tx uuid.UUID) resolved {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.r[tx]
}

// --- harness ---

type harness struct {
	coord    *saga.Coordinator
	producer *fakeProducer
	outcomes *fakeOutcomeRepo
	resolver *fakeResolver
	sagas    *fakeSagaRepo
	tx       uuid.UUID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	p := &fakeProducer{}
	o := newFakeOutcomeRepo()
	r := newFakeResolver()
	s := newFakeSagaRepo()
	c := saga.New(saga.Deps{
		Sagas:    s,
		Outcomes: o,
		Producer: p,
		Resolver: r,
		Logger:   slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	return &harness{coord: c, producer: p, outcomes: o, resolver: r, sagas: s, tx: uuid.New()}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func (h *harness) start(t *testing.T) {
	t.Helper()
	err := h.coord.Start(context.Background(), domain.TransferRequest{
		TransactionID: h.tx, FromAccount: "A", ToAccount: "C", AmountMinor: 100, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// sagaID returns the saga id assigned for the transaction (read from the last
// produced command, which carries it).
func (h *harness) sagaID() uuid.UUID { return h.producer.last().SagaID }

func (h *harness) onEvent(t *testing.T, leg domain.Leg, typ domain.EventType, account, reason string) {
	t.Helper()
	err := h.coord.OnEvent(context.Background(), domain.Event{
		EventID: uuid.New(), TransactionID: h.tx, SagaID: h.sagaID(),
		Leg: leg, Type: typ, Account: account, AmountMinor: 100, Currency: "USD", Reason: reason,
	})
	if err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
}

// --- tests ---

func TestSagaHappyPath(t *testing.T) {
	h := newHarness(t)
	h.start(t)

	// First leg: debit the source.
	if got := h.producer.last(); got.Leg != domain.LegDebit || got.Account != "A" {
		t.Fatalf("first command = %+v, want DEBIT on A", got)
	}

	h.onEvent(t, domain.LegDebit, domain.EventDebited, "A", "")
	if got := h.producer.last(); got.Leg != domain.LegCredit || got.Account != "C" {
		t.Fatalf("second command = %+v, want CREDIT on C", got)
	}

	h.onEvent(t, domain.LegCredit, domain.EventCredited, "C", "")
	if r := h.resolver.get(h.tx); !r.ok || !r.success {
		t.Fatalf("expected success resolve, got %+v", r)
	}
	if o, ok, _ := h.outcomes.Get(context.Background(), h.tx); !ok || o.Status != iface.OutcomeSuccess {
		t.Fatalf("expected SUCCESS outcome, got %+v ok=%v", o, ok)
	}
}

func TestSagaDebitRejected(t *testing.T) {
	h := newHarness(t)
	h.start(t)

	h.onEvent(t, domain.LegDebit, domain.EventRejected, "A", domain.ReasonInsufficientFunds)

	// No second command should have been produced (no compensation needed).
	if h.producer.count() != 1 {
		t.Fatalf("expected only the debit command, got %d commands", h.producer.count())
	}
	if r := h.resolver.get(h.tx); !r.ok || r.success || r.reason != domain.ReasonInsufficientFunds {
		t.Fatalf("expected failed resolve with insufficient funds, got %+v", r)
	}
	if o, _, _ := h.outcomes.Get(context.Background(), h.tx); o.Status != iface.OutcomeFailed {
		t.Fatalf("expected FAILED outcome, got %+v", o)
	}
}

func TestSagaCreditRejectedCompensates(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.onEvent(t, domain.LegDebit, domain.EventDebited, "A", "")

	// Credit rejected -> saga must compensate (refund the source).
	h.onEvent(t, domain.LegCredit, domain.EventRejected, "C", domain.ReasonCurrencyMismatch)
	comp := h.producer.last()
	if comp.Leg != domain.LegCompensate || comp.Account != "A" {
		t.Fatalf("expected COMPENSATE on A, got %+v", comp)
	}

	// Compensating credit succeeds -> overall failure for the original reason.
	h.onEvent(t, domain.LegCompensate, domain.EventCredited, "A", "")
	if r := h.resolver.get(h.tx); !r.ok || r.success || r.reason != domain.ReasonCurrencyMismatch {
		t.Fatalf("expected failed resolve with currency mismatch, got %+v", r)
	}
	if o, _, _ := h.outcomes.Get(context.Background(), h.tx); o.Status != iface.OutcomeFailed {
		t.Fatalf("expected FAILED outcome, got %+v", o)
	}
}

// duplicate Start with the same transaction id must not start a second transfer.
func TestSagaIdempotentStart(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	before := h.producer.count()
	h.start(t) // retry
	if h.producer.count() != before {
		t.Fatalf("retry produced extra commands: %d -> %d", before, h.producer.count())
	}
}
