// Package kafka wraps segmentio/kafka-go with the small surface this service
// needs: a keyed producer (acks=all), a manual-commit consumer group, and a
// whole-topic replay reader used to rebuild in-memory state and the read model.
package kafka

import (
	"context"
	"fmt"
	"time"

	kafkago "github.com/segmentio/kafka-go"
)

// Topic names. The partition key for both command and event topics is the
// account id, which gives per-account FIFO ordering — the doc's "one shard per
// Raft group", realized as a Kafka partition.
const (
	// TopicTransfers carries client transfer requests from the gateway to the
	// Saga coordinator. Keyed by transaction_id.
	TopicTransfers = "wallet.transfers"
	// TopicCommands carries per-account leg commands from the Saga coordinator
	// to the command-processor. Keyed by account.
	TopicCommands = "wallet.commands"
	// TopicEvents is the immutable event store: validated facts emitted by the
	// command-processor, consumed by the projector and the Saga. Keyed by account.
	TopicEvents = "wallet.events"
	// TopicDLQ receives messages a consumer could not process (poison), for
	// out-of-band inspection instead of silent drops.
	TopicDLQ = "wallet.dlq"
)

// Consumer group ids (one per role so each role tracks its own offsets).
const (
	GroupSaga      = "wallet-saga"
	GroupProjector = "wallet-projector"
	GroupSagaCmd   = "wallet-cmd-processor"
)

// PartitionKey returns the Kafka message key for an account. Same account ->
// same partition -> ordered, single-consumer processing.
func PartitionKey(account string) string { return account }

// Ping verifies broker reachability (a controller metadata round-trip). Used by
// the gateway's readiness probe so the LB stops routing when Kafka is down.
func Ping(ctx context.Context, brokers []string) error {
	conn, err := kafkago.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return fmt.Errorf("kafka: ping dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Controller(); err != nil {
		return fmt.Errorf("kafka: ping controller: %w", err)
	}
	return nil
}

// EnsureTopics creates the topics if they do not exist (idempotent). Local/dev
// convenience; production would provision topics out of band with RF=3 and
// min.insync.replicas=2. partitions controls the shard count.
func EnsureTopics(ctx context.Context, brokers []string, partitions, replicationFactor int) error {
	conn, err := kafkago.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return fmt.Errorf("kafka: dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	controller, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("kafka: controller: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cconn, err := kafkago.DialContext(cctx, "tcp", fmt.Sprintf("%s:%d", controller.Host, controller.Port))
	if err != nil {
		return fmt.Errorf("kafka: dial controller: %w", err)
	}
	defer func() { _ = cconn.Close() }()

	cfgs := make([]kafkago.TopicConfig, 0, 4)
	for _, t := range []string{TopicTransfers, TopicCommands, TopicEvents, TopicDLQ} {
		cfgs = append(cfgs, kafkago.TopicConfig{
			Topic:             t,
			NumPartitions:     partitions,
			ReplicationFactor: replicationFactor,
		})
	}
	if err := cconn.CreateTopics(cfgs...); err != nil {
		return fmt.Errorf("kafka: create topics: %w", err)
	}
	return nil
}
