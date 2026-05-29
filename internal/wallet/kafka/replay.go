package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	kafkago "github.com/segmentio/kafka-go"
)

// idleEnd is how long a partition reader waits for the next message before
// concluding it has drained all currently-available data. Replay runs against a
// static log (at startup, before live traffic), so a short gap reliably means
// "end of partition".
const idleEnd = 1500 * time.Millisecond

// ReplayedMessage is one event yielded during a whole-topic replay.
type ReplayedMessage struct {
	Key       string
	Value     []byte
	Partition int
	Offset    int64
}

// ReplayTopic reads every message currently in `topic`, in per-partition offset
// order, from the given start offset to the end, invoking fn for each. It uses
// partition readers WITHOUT a consumer group, so it never commits offsets and
// never interferes with the live consumers.
//
// This is the mechanism behind reproducibility: replaying wallet.events through
// the pure Apply function reconstructs balances deterministically. The
// command-processor uses it at startup to rebuild authoritative in-memory state.
//
// startOffsets, if non-nil, maps partition -> first offset to read (e.g. derived
// from a snapshot). Partitions absent from the map are read from the beginning.
func ReplayTopic(
	ctx context.Context,
	brokers []string,
	topic string,
	startOffsets map[int]int64,
	fn func(ReplayedMessage) error,
) error {
	conn, err := kafkago.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return fmt.Errorf("kafka: replay dial: %w", err)
	}
	parts, err := conn.ReadPartitions(topic)
	_ = conn.Close()
	if err != nil {
		return fmt.Errorf("kafka: replay read partitions: %w", err)
	}

	for _, p := range parts {
		if err := replayPartition(ctx, brokers, topic, p.ID, startOffsets, fn); err != nil {
			return err
		}
	}
	return nil
}

func replayPartition(
	ctx context.Context,
	brokers []string,
	topic string,
	partition int,
	startOffsets map[int]int64,
	fn func(ReplayedMessage) error,
) error {
	start := kafkago.FirstOffset
	if startOffsets != nil {
		if o, ok := startOffsets[partition]; ok {
			start = o
		}
	}

	r := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:   brokers,
		Topic:     topic,
		Partition: partition,
		MinBytes:  1,
		MaxBytes:  10 << 20,
	})
	defer func() { _ = r.Close() }()
	if err := r.SetOffset(start); err != nil {
		return fmt.Errorf("kafka: replay seek p%d: %w", partition, err)
	}

	for {
		// A bounded wait: if no message arrives within idleEnd, the partition is
		// drained. (Replay runs before live traffic, so this is safe.)
		rctx, cancel := context.WithTimeout(ctx, idleEnd)
		m, err := r.ReadMessage(rctx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil // drained
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("kafka: replay read p%d: %w", partition, err)
		}
		if err := fn(ReplayedMessage{
			Key:       string(m.Key),
			Value:     m.Value,
			Partition: m.Partition,
			Offset:    m.Offset,
		}); err != nil {
			return err
		}
	}
}
