// Package projector implements the CQRS read side. Balances are now authoritative
// in `accounts` (written by the command-processor), so the projector's job is to
// build the **double-entry ledger view** from the event log. It is independent of
// the write path, idempotent (event_id unique keys), and rebuildable by replay.
package projector

import (
	"context"
	"log/slog"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
	walletkafka "github.com/Robustrade/wallet-transfer-assignment/internal/wallet/kafka"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/repo/iface"
)

// Projector consumes events and maintains the ledger view.
type Projector struct {
	consumer *walletkafka.Consumer
	ledger   iface.LedgerWriter
	logger   *slog.Logger
}

// Config configures a Projector.
type Config struct {
	Brokers []string
	Ledger  iface.LedgerWriter
	Logger  *slog.Logger
}

// New constructs a Projector.
func New(cfg Config) *Projector {
	return &Projector{
		consumer: walletkafka.NewConsumer(cfg.Brokers, walletkafka.TopicEvents, walletkafka.GroupProjector),
		ledger:   cfg.Ledger,
		logger:   cfg.Logger,
	}
}

// Run consumes events and appends ledger entries until ctx is cancelled.
func (p *Projector) Run(ctx context.Context) error {
	defer func() { _ = p.consumer.Close() }()
	return p.consumer.Process(ctx, p.onMessage)
}

// onMessage appends a ledger entry for a balance-changing event (idempotent). A
// processing error leaves the offset uncommitted so redelivery retries.
func (p *Projector) onMessage(ctx context.Context, msg walletkafka.Message) error {
	evt, err := domain.DecodeEvent(msg.Value)
	if err != nil {
		return walletkafka.ErrPoison
	}
	return p.ledger.AppendFromEvent(ctx, evt)
}
