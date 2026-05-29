// Package command implements the write-side processor. It is STATELESS: each
// command is applied to the authoritative `accounts` row under a DB row lock
// (in CommandStore.ApplyCommand), with the resulting event written to a
// transactional outbox in the same transaction. The processor then publishes the
// event to the event log.
//
// Because correctness comes from the per-account row lock + command dedup (not
// from in-memory state or partition ownership), instances scale horizontally and
// Kafka consumer-group rebalancing is harmless.
package command

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
	walletkafka "github.com/Robustrade/wallet-transfer-assignment/internal/wallet/kafka"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/repo/iface"
)

const (
	relayInterval = 2 * time.Second // backstop relay tick
	relayGrace    = 5 * time.Second // only republish outbox rows older than this
	relayBatch    = 200             // max events per relay tick
)

// Publisher is the event-publishing port the processor depends on (DIP).
type Publisher interface {
	Publish(ctx context.Context, topic, key string, value []byte) error
}

// Processor consumes leg commands and turns each into a durable balance change +
// event via the CommandStore.
type Processor struct {
	producer Publisher
	store    iface.CommandApplier
	relay    iface.OutboxRelay
	consumer *walletkafka.Consumer
	logger   *slog.Logger
}

// NewProcessor constructs a Processor. store applies commands; relay drains the
// outbox as a crash backstop (both implemented by *postgres.CommandStore).
func NewProcessor(brokers []string, producer Publisher, store iface.CommandApplier, relay iface.OutboxRelay, logger *slog.Logger) *Processor {
	return &Processor{
		producer: producer,
		store:    store,
		relay:    relay,
		consumer: walletkafka.NewConsumer(brokers, walletkafka.TopicCommands, walletkafka.GroupSagaCmd),
		logger:   logger,
	}
}

// Run consumes commands and runs the outbox relay backstop until ctx is cancelled.
func (p *Processor) Run(ctx context.Context) error {
	defer func() { _ = p.consumer.Close() }()
	go p.runRelay(ctx)
	return p.consumer.Process(ctx, p.onMessage)
}

// onMessage applies one command and publishes its event. A poison message is
// skipped (Process routes it to the DLQ); a processing error stops the loop with
// the offset uncommitted so it is redelivered (then deduped by ApplyCommand).
func (p *Processor) onMessage(ctx context.Context, msg walletkafka.Message) error {
	cmd, err := domain.DecodeCommand(msg.Value)
	if err != nil {
		return walletkafka.ErrPoison // routed to DLQ + committed
	}
	evt, err := p.store.ApplyCommand(ctx, cmd)
	if err != nil {
		return err
	}
	if evt.EventID == uuid.Nil {
		// Already handled and its event already published+purged — nothing to do.
		return nil
	}
	return p.publish(ctx, evt)
}

// publish sends the event to the log and marks the outbox row published.
func (p *Processor) publish(ctx context.Context, evt domain.Event) error {
	value, err := evt.Encode()
	if err != nil {
		return err
	}
	if err := p.producer.Publish(ctx, walletkafka.TopicEvents, walletkafka.PartitionKey(evt.Account), value); err != nil {
		return err
	}
	return p.store.MarkPublished(ctx, evt.EventID)
}

// runRelay is the transactional-outbox backstop: it periodically publishes any
// outbox rows the hot path left unpublished (e.g. a crash after commit but
// before publish). It only touches rows older than relayGrace so it doesn't race
// the hot path on fresh rows; SKIP LOCKED (in ClaimUnpublished) shards work
// across instances. Re-publishes are deduped downstream by event_id.
func (p *Processor) runRelay(ctx context.Context) {
	t := time.NewTicker(relayInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			events, err := p.relay.ClaimUnpublished(ctx, relayGrace, relayBatch)
			if err != nil {
				p.logger.WarnContext(ctx, "outbox relay claim failed", "error", err)
				continue
			}
			for _, e := range events {
				if err := p.publish(ctx, e); err != nil {
					p.logger.WarnContext(ctx, "outbox relay publish failed", "error", err, "event_id", e.EventID)
					break
				}
			}
		}
	}
}
