package kafka

import (
	"context"
	"fmt"

	kafkago "github.com/segmentio/kafka-go"
)

// Producer publishes keyed messages with acks=all. A single Writer multiplexes
// across topics (Topic is set per-message). acks=all + (in prod) RF=3 /
// min.insync.replicas=2 is what makes a published event durable — this is the
// reliability guarantee that replaces the chapter's Raft-replicated event file.
type Producer struct {
	w *kafkago.Writer
}

// NewProducer constructs a Producer against the given brokers.
func NewProducer(brokers []string) *Producer {
	return &Producer{
		w: &kafkago.Writer{
			Addr: kafkago.TCP(brokers...),
			// Hash balancer: route by key so a given account always lands on
			// the same partition (per-account ordering).
			Balancer:     &kafkago.Hash{},
			RequiredAcks: kafkago.RequireAll,
			// Synchronous writes: Publish returns only after the broker acks,
			// so the caller can sequence "produce, then update state".
			BatchSize:    1,
			BatchTimeout: 0,
		},
	}
}

// Publish writes one keyed message to a topic and waits for the broker ack.
func (p *Producer) Publish(ctx context.Context, topic, key string, value []byte) error {
	if err := p.w.WriteMessages(ctx, kafkago.Message{
		Topic: topic,
		Key:   []byte(key),
		Value: value,
	}); err != nil {
		return fmt.Errorf("kafka: publish to %s: %w", topic, err)
	}
	return nil
}

// Close flushes and closes the writer.
func (p *Producer) Close() error { return p.w.Close() }
