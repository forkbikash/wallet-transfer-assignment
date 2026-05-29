package saga

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
	walletkafka "github.com/Robustrade/wallet-transfer-assignment/internal/wallet/kafka"
)

const (
	sweepInterval = 10 * time.Second // how often the sweeper runs
	staleAfter    = 30 * time.Second // a non-terminal saga idle this long is re-driven
	sweepBatch    = 100              // max sagas claimed per sweep
)

// Run drives the coordinator: it consumes the transfers topic (to Start
// transfers) and the events topic (to advance them), and runs a periodic sweeper
// that re-drives stale in-flight sagas — all until ctx is cancelled.
func (c *Coordinator) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return c.consumeTransfers(ctx) })
	g.Go(func() error { return c.consumeEvents(ctx) })
	g.Go(func() error { return c.runSweeper(ctx) })
	return g.Wait()
}

// runSweeper periodically claims non-terminal sagas that have gone stale (a leg
// event lost, a coordinator crash mid-flight) and re-drives the pending leg.
// SweepStale uses FOR UPDATE SKIP LOCKED so multiple saga instances share the
// work without colliding, and bumps updated_at so a saga isn't re-driven every
// tick. Re-emits are idempotent downstream.
func (c *Coordinator) runSweeper(ctx context.Context) error {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			stale, err := c.sagas.SweepStale(ctx, staleAfter, sweepBatch)
			if err != nil {
				c.logger.WarnContext(ctx, "saga: sweep failed", "error", err)
				continue
			}
			for _, s := range stale {
				c.logger.InfoContext(ctx, "saga: re-driving stale transfer",
					"tx", s.TransactionID, "status", s.Status)
				if err := c.driveNext(ctx, s); err != nil {
					c.logger.WarnContext(ctx, "saga: re-drive failed", "tx", s.TransactionID, "error", err)
				}
			}
		}
	}
}

func (c *Coordinator) consumeTransfers(ctx context.Context) error {
	cons := walletkafka.NewConsumer(c.brokers, walletkafka.TopicTransfers, walletkafka.GroupSaga+"-transfers")
	defer func() { _ = cons.Close() }()
	return cons.Process(ctx, c.onTransferMessage)
}

func (c *Coordinator) onTransferMessage(ctx context.Context, msg walletkafka.Message) error {
	req, err := domain.DecodeTransferRequest(msg.Value)
	if err != nil {
		return walletkafka.ErrPoison
	}
	return c.Start(ctx, req)
}

func (c *Coordinator) consumeEvents(ctx context.Context) error {
	cons := walletkafka.NewConsumer(c.brokers, walletkafka.TopicEvents, walletkafka.GroupSaga)
	defer func() { _ = cons.Close() }()
	return cons.Process(ctx, c.onEventMessage)
}

func (c *Coordinator) onEventMessage(ctx context.Context, msg walletkafka.Message) error {
	evt, err := domain.DecodeEvent(msg.Value)
	if err != nil {
		return walletkafka.ErrPoison
	}
	return c.OnEvent(ctx, evt)
}
