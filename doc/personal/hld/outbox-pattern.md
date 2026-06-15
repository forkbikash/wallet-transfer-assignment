# Outbox Pattern

## What it solves: the dual-write problem

The **transactional outbox pattern** answers one hard question in distributed systems: *how do you update your database AND publish a message/event reliably, when those are two separate systems with no shared transaction?*

Naive code does two writes that are not atomic together:

```go
db.Save(transaction)          // 1. write to DB
kafka.Publish(transferEvent)  // 2. publish to Kafka
```

There is no transaction spanning the DB and Kafka, so a crash between the two leaves the system inconsistent:

- DB commit succeeds, app crashes before publish → **event lost** (consumers never learn the transfer happened).
- Publish succeeds, DB rolls back → **phantom event** (consumers act on something that never persisted).

This is the **dual-write problem**: you cannot atomically write to two independent systems.

## Why "reorder + retry" does NOT fix it

A tempting idea: publish first, then save, and rely on retry.

```go
kafka.Publish(transferEvent)  // 1. publish
db.Save(transaction)          // 2. save (with retry)
```

This does not work — it only moves the failure window:

- Publish succeeds → the event is already out in the world; consumers start reacting.
- Process **crashes** (or DB is down and retries exhaust) before `db.Save`.
- Result: a **phantom event** — downstream services believe a transfer happened that the source-of-truth DB has no record of. In a money system, a phantom credit is worse than a lost one.

### The core reason retry can't save either ordering

Retry only helps if the process **stays alive** to keep retrying. It does nothing against a crash, deploy/restart, OOM kill, or node failure right after the first write. The thing you'd retry lives **in memory**, and it's lost exactly when you need it most.

For retry to be reliable, the *intent to retry* must itself be **durably persisted in a place that shares a transaction with the business write**. That durable place is... the outbox table. So you end up back at the outbox pattern.

| Approach | Failure mode | Result |
|---|---|---|
| Save → Publish + retry | Crash after save, before publish | **Lost event** |
| Publish → Save + retry | Crash after publish, before save | **Phantom event** |
| **Outbox** | Crash anytime | **Safe** — business row + outbox row commit atomically; relay republishes after restart |

> Retry is for **transient failures while the process is alive** (broker briefly unreachable, network blip). It is **not** a mechanism for atomicity across two systems.

## The solution

Write to **one** system — your database — atomically, then publish asynchronously:

1. In the **same DB transaction** as the business change, insert a row into an `outbox` table describing the event.
2. A separate process (the **relay** / **message publisher**) reads unpublished outbox rows and publishes them to Kafka.
3. Mark rows as published (or delete them) once successfully sent.

```
┌─────────────────────────────────────┐
│   Single DB transaction (atomic)     │
│   ┌──────────────┐  ┌─────────────┐  │
│   │ transactions │  │   outbox    │  │
│   │   table      │  │   table     │  │
│   └──────────────┘  └─────────────┘  │
└─────────────────────────────────────┘
              │
              ▼ (relay polls / CDC tails the WAL)
        ┌───────────┐
        │   Kafka   │
        └───────────┘
```

```go
tx.Begin()
tx.Save(transaction)       // business change
tx.Insert(outboxRow)       // event intent — durable, same tx
tx.Commit()                // atomic: both or neither
// relay reads outboxRow, publishes, retries safely across restarts
```

Because the business write and the outbox write share one transaction, they either both commit or both roll back. The event exists **if and only if** the business change happened. The relay can retry forever because the outbox row is on disk, not in memory — crash and restart, the row is still there waiting.

## Two ways to run the relay

| Approach | How it works | Trade-off |
|---|---|---|
| **Polling publisher** | Background worker loops `SELECT ... WHERE published = false` | Simple, no extra infra; adds DB load and some latency |
| **CDC (Change Data Capture)** | A tool like Debezium tails the DB write-ahead log and streams outbox inserts to Kafka | Low latency, no polling load; more infra/operational complexity |

## Delivery guarantee: at-least-once

The outbox gives **at-least-once** delivery, not exactly-once. The relay may publish a row, crash before marking it published, and republish on restart.

Therefore **consumers must be idempotent** — typically by deduplicating on an event ID, or by using the wallet transaction ID as an idempotency key so a replayed event is a no-op.

## Relation to this codebase

This project implements wallet transfers with the **saga pattern over Kafka**. The outbox pattern is the natural companion: each saga step's local DB transaction writes its state change *and* the next command/event into an outbox atomically, so the saga cannot lose a step or emit an event for an uncommitted state.

Without it, a crash between "debit committed" and "publish debit-succeeded event" would stall or corrupt the saga — exactly the lost/phantom-event failure modes above. See also [[kafka-features-and-patterns]] and [[wallet-transfer-architecture]].

## How this repo actually implements it

There are two layers:

- **Command processor** (`internal/wallet/command/`) — owns account balances, applies one saga leg (debit / credit / compensation) at a time, and uses the outbox.
- **Saga coordinator** (`internal/wallet/saga/`) — orchestrates the multi-leg flow via a `saga_transactions` state machine, reacting to the events the processor emits.

The saga drives a leg by emitting a *command*; the processor executes it and emits an *event*; the saga consumes that event to decide the next leg. The outbox sits at the processor boundary.

### The atomic write

`internal/wallet/repo/postgres/command_store.go:35-67` — `ApplyCommand` wraps everything in one transaction, so the balance change and the outbox row commit together or not at all:

```go
err := s.db.Transaction(func(tx *gorm.DB) error {
    claimCommand(tx, cmd)        // dedup: INSERT processed_commands ON CONFLICT DO NOTHING
    lockBalance(tx, cmd.Account) // SELECT ... FOR UPDATE
    applyBalanceChange(tx, evt)  // UPDATE accounts
    writeOutbox(tx, evt)         // INSERT event_outbox (published = false)
})
```

If a command was already claimed (a redelivery), it does **not** error — it reloads the recorded event and returns it so the caller re-publishes the *same* `event_id` (downstream dedups). That is what makes the stateless processor effectively-once (`command_store.go:42-44`, `loadRecordedEvent` at `:86-96`).

### The publish hop (after commit)

`internal/wallet/command/processor.go:84-93` — publish happens *after* the DB commit, and `MarkPublished` is only reached on success:

```go
func (p *Processor) publish(ctx context.Context, evt domain.Event) error {
    value, err := evt.Encode()
    if err != nil {
        return err
    }
    if err := p.producer.Publish(ctx, walletkafka.TopicEvents, walletkafka.PartitionKey(evt.Account), value); err != nil {
        return err          // publish failed: return BEFORE MarkPublished
    }
    return p.store.MarkPublished(ctx, evt.EventID)   // only runs on success
}
```

### The relay (polling publisher backstop)

`internal/wallet/command/processor.go:100-121` — a goroutine started on boot (`go p.runRelay(ctx)`):

- Ticks every `relayInterval = 2s`.
- `ClaimUnpublished(relayGrace, relayBatch)` drains rows where `NOT published`, **only those older than `relayGrace = 5s`** (so it doesn't race the hot path on fresh rows), batch 200.
- Uses `FOR UPDATE SKIP LOCKED` so multiple processor instances shard the work without colliding.
- Republishes, then marks published.

No CDC/Debezium — this is the polling-publisher variant. CDC remains a future option (see table above).

## What happens when publish fails

The failure self-heals; correctness is never at risk. The money already moved and the event is already durably in `event_outbox` with `published = false` — only the Kafka hop failed.

**Where it can fail** — `internal/wallet/kafka/producer.go:36-45`, the synchronous `WriteMessages` (`BatchSize: 1`, `RequiredAcks: RequireAll`) returns an error if the broker is unreachable, a leader election is in progress, the ack never arrives, or the context times out:

```go
func (p *Producer) Publish(ctx context.Context, topic, key string, value []byte) error {
    if err := p.w.WriteMessages(ctx, kafkago.Message{
        Topic: topic, Key: []byte(key), Value: value,
    }); err != nil {
        return fmt.Errorf("kafka: publish to %s: %w", topic, err)  // publish failed
    }
    return nil
}
```

**How the error propagates:** `Publish` → `publish` (skips `MarkPublished`, row stays `published = false`) → `onMessage` returns the error (`processor.go:67-80`) → the Kafka consumer does **not** commit the offset, so the command can be redelivered.

```
publish fails
   │
   ▼
event still in outbox (published = false)   ← money is safe
   │
   ▼ (within a few seconds)
relay finds it ──▶ re-publishes to Kafka ──▶ marks published ✓
   │
   ▼
saga continues to the next leg
```

**Possible double-publish** (e.g. the message reached Kafka but `MarkPublished` failed, so the relay sends it again) is harmless: saga transitions are conditional CAS updates and `ledger_entries.event_id` is unique, so duplicates are no-ops. This is the at-least-once-delivery + idempotent-consumers rule in practice.

**Worst case:** if Kafka stays down, the relay keeps retrying every 2s and the transfer stays paused but correct (money recorded, no event lost) until Kafka returns, then resumes automatically.

## Further reading

- Chris Richardson, *Microservices Patterns*, ch. 3 (Transactional Outbox, Polling Publisher, Transaction Log Tailing)
- microservices.io — *Pattern: Transactional outbox*
- Debezium documentation — *Outbox Event Router*
- Martin Kleppmann, *Designing Data-Intensive Applications*, ch. 11 (stream processing, change data capture)
