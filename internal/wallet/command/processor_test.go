package command

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
	walletkafka "github.com/Robustrade/wallet-transfer-assignment/internal/wallet/kafka"
)

// fakeStore implements iface.CommandApplier + iface.OutboxRelay.
type fakeStore struct {
	evt     domain.Event
	applied []domain.Command
	marked  []uuid.UUID
}

func (f *fakeStore) ApplyCommand(_ context.Context, cmd domain.Command) (domain.Event, error) {
	f.applied = append(f.applied, cmd)
	e := f.evt
	e.TransactionID = cmd.TransactionID
	e.Account = cmd.Account
	return e, nil
}
func (f *fakeStore) MarkPublished(_ context.Context, id uuid.UUID) error {
	f.marked = append(f.marked, id)
	return nil
}
func (f *fakeStore) ClaimUnpublished(context.Context, time.Duration, int) ([]domain.Event, error) {
	return nil, nil
}

type fakePub struct{ events []domain.Event }

func (f *fakePub) Publish(_ context.Context, _, _ string, value []byte) error {
	e, err := domain.DecodeEvent(value)
	if err != nil {
		return err
	}
	f.events = append(f.events, e)
	return nil
}

func newTestProcessor(store *fakeStore, pub Publisher) *Processor {
	return NewProcessor([]string{"localhost:0"}, pub, store, store,
		slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
}

// onMessage applies a command via the store and publishes + marks the event.
func TestProcessorOnMessage(t *testing.T) {
	store := &fakeStore{evt: domain.Event{EventID: uuid.New(), Type: domain.EventDebited, AmountMinor: 30, Currency: "USD"}}
	pub := &fakePub{}
	p := newTestProcessor(store, pub)

	cmd := domain.Command{TransactionID: uuid.New(), SagaID: uuid.New(), Leg: domain.LegDebit, Account: "A", AmountMinor: 30, Currency: "USD"}
	value, err := cmd.Encode()
	require.NoError(t, err)

	require.NoError(t, p.onMessage(context.Background(), walletkafka.Message{Value: value}))
	require.Len(t, store.applied, 1)
	require.Len(t, pub.events, 1)
	require.Equal(t, store.evt.EventID, pub.events[0].EventID)
	require.Equal(t, []uuid.UUID{store.evt.EventID}, store.marked, "published event must be marked in the outbox")
}

// An undecodable command is poison -> routed to the DLQ (skipped) by Process.
func TestProcessorPoison(t *testing.T) {
	p := newTestProcessor(&fakeStore{}, &fakePub{})
	err := p.onMessage(context.Background(), walletkafka.Message{Value: []byte("not json")})
	require.True(t, errors.Is(err, walletkafka.ErrPoison))
}
