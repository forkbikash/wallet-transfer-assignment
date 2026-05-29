package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	kafkago "github.com/segmentio/kafka-go"
)

// ErrPoison signals that a message cannot be processed (e.g. undecodable). A
// handler returns it to have Process route the raw message to the DLQ and commit
// (skip) instead of stalling the partition or silently dropping it.
var ErrPoison = errors.New("poison message")

// Consumer is a manual-commit consumer-group reader. Offsets are committed only
// AFTER the message has been durably processed (event applied + DB committed),
// which together with idempotent application gives effectively-once semantics
// across crashes.
type Consumer struct {
	r   *kafkago.Reader
	dlq *Producer // for poison messages; lazily used
}

// Message is a consumed Kafka message handed to the processing loop.
type Message struct {
	Key       string
	Value     []byte
	Partition int
	Offset    int64

	raw kafkago.Message
}

// NewConsumer joins groupID on topic and starts at the earliest offset for a
// new group (so a fresh group can replay history if needed).
func NewConsumer(brokers []string, topic, groupID string) *Consumer {
	return &Consumer{
		r: kafkago.NewReader(kafkago.ReaderConfig{
			Brokers: brokers,
			Topic:   topic,
			GroupID: groupID,
			// Manual commit only: we never auto-commit, so a crash before the
			// DB write re-delivers the message.
			CommitInterval: 0,
			StartOffset:    kafkago.FirstOffset,
			MinBytes:       1,
			MaxBytes:       10 << 20,
			// Explicit group timeouts (don't rely on library defaults) so
			// rebalances and liveness detection are predictable in production.
			SessionTimeout:    30 * time.Second,
			HeartbeatInterval: 3 * time.Second,
			RebalanceTimeout:  30 * time.Second,
		}),
		dlq: NewProducer(brokers),
	}
}

// Process runs the manual-commit consume loop until ctx is cancelled: fetch a
// message, hand it to fn, and commit its offset only if fn returns nil. This
// centralizes the loop mechanics (fetch / commit / context handling) so each
// worker supplies only its message handler.
//
// Handler contract:
//   - return nil  -> commit and advance (also the way to skip a poison message
//     after logging it).
//   - return err  -> stop the loop with that error; the offset is NOT committed,
//     so the message is redelivered on restart.
func (c *Consumer) Process(ctx context.Context, fn func(context.Context, Message) error) error {
	for {
		msg, err := c.Fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("kafka: fetch: %w", err)
		}
		switch err := fn(ctx, msg); {
		case err == nil:
			// fall through to commit
		case errors.Is(err, ErrPoison):
			if derr := c.toDLQ(ctx, msg); derr != nil {
				return derr
			}
			// poison routed to DLQ; commit to advance past it
		default:
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := c.Commit(ctx, msg); err != nil {
			return err
		}
	}
}

// toDLQ republishes a poison message's raw bytes to the DLQ topic for
// out-of-band inspection (keyed by the original key to preserve partitioning).
func (c *Consumer) toDLQ(ctx context.Context, m Message) error {
	if err := c.dlq.Publish(ctx, TopicDLQ, m.Key, m.Value); err != nil {
		return fmt.Errorf("kafka: route to DLQ: %w", err)
	}
	return nil
}

// Fetch blocks for the next message without committing its offset.
func (c *Consumer) Fetch(ctx context.Context) (Message, error) {
	m, err := c.r.FetchMessage(ctx)
	if err != nil {
		return Message{}, err
	}
	return Message{
		Key:       string(m.Key),
		Value:     m.Value,
		Partition: m.Partition,
		Offset:    m.Offset,
		raw:       m,
	}, nil
}

// Commit acknowledges that the message has been fully processed.
func (c *Consumer) Commit(ctx context.Context, m Message) error {
	if err := c.r.CommitMessages(ctx, m.raw); err != nil {
		return fmt.Errorf("kafka: commit offset: %w", err)
	}
	return nil
}

// Close releases the reader and the DLQ producer.
func (c *Consumer) Close() error {
	if c.dlq != nil {
		_ = c.dlq.Close()
	}
	return c.r.Close()
}
