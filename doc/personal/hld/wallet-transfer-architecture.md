# Wallet-to-Wallet Transfer — Architecture Diagram

```
            POST /v1/wallet/balance_transfer
 Client ──────────────────────────────────────► Gateway (HTTP)
   ▲                                               │ produce TransferRequest
   │ 200/422 (push) or 202 + poll                  ▼
   │                                        [wallet.transfers]──► Saga Coordinator ◄──┐
   │  push registry (same process)                  │  emit leg Command               │
   └────────────────────────────────────────────────┤  (DEBIT → CREDIT → COMPENSATE)  │
                                                    ▼                                 │
                                            [wallet.commands]                         │
                                                    │ consume (keyed by account)      │
                                                    ▼                                 │
                                          Command Processor                           │
                                  ┌─────────────────┴──────────────────┐              │
                                  │ ONE Postgres tx, per-account lock: │              │
                                  │  dedup + balance change + outbox   │              │
                                  └─────────────────┬──────────────────┘              │
                                                    │ publish Event                   │
                                                    ▼                                 │
                                             [wallet.events] ─────────────────────────┘
                                                    │ consume                (advance saga)
                                                    ▼
                                                Projector ──► ledger_entries (read view)

  [wallet.dlq] ◄── any consumer, for poison (undecodable) messages

  Postgres tables: accounts (authoritative balances) · processed_commands (dedup)
                   event_outbox · saga_transactions · transaction_outcomes · ledger_entries
```

Saga status lifecycle:

```
PENDING ──debit rejected──► FAILED
PENDING ──debit ok, credit ok──► COMPLETED
PENDING ──credit rejected──► COMPENSATING ──refund applied──► COMPENSATED
```

## Happy Path Sequence (A → B, amount 30)

```
 Client        Gateway           Kafka                Saga            Command Processor      Projector
   │              │                │                    │                     │                  │
   │ POST transfer│                │                    │                     │                  │
   ├─────────────►│                │                    │                     │                  │
   │              │ check idempotency ──────────────────────────────────────────────────────────┼─── reads: saga_transactions,
   │              │ register push waiter (txID → chan)  │                     │                  │           transaction_outcomes
   │              │ TransferRequest│                    │                     │                  │
   │              ├───────────────►│ wallet.transfers   │                     │                  │
   │              │ (blocks ≤ 5s)  ├───────────────────►│                     │                  │
   │              │                │                    │ INSERT saga PENDING ┼──────────────────┼─── writes: saga_transactions
   │              │                │  Command{DEBIT, A} │                     │                  │
   │              │                │◄───────────────────┤                     │                  │
   │              │                │ wallet.commands    │                     │                  │
   │              │                ├─────────────────────────────────────────►│                  │
   │              │                │                    │     ONE tx: claim tx:DEBIT,            │
   │              │                │                    │     lock A, A -= 30, outbox ───────────┼─── writes: processed_commands,
   │              │                │                    │                     │                  │     accounts, event_outbox
   │              │                │  ACCOUNT_DEBITED   │                     │                  │
   │              │                │◄──────────────────────────────────────── ┤ (mark published) │
   │              │                │ wallet.events      │                     │                  │
   │              │                ├───────────────────►│                     ├─────────────────►│
   │              │                │                    │ CAS debit=DONE ─────┼──────────────────┼─── updates: saga_transactions
   │              │                │  Command{CREDIT, B}│                     │ ledger entry ────┼─── writes: ledger_entries (DEBIT)
   │              │                │◄───────────────────┤                     │                  │
   │              │                │ wallet.commands    │                     │                  │
   │              │                ├─────────────────────────────────────────►│                  │
   │              │                │                    │     ONE tx: claim tx:CREDIT,           │
   │              │                │                    │     lock B, B += 30, outbox ───────────┼─── writes: processed_commands,
   │              │                │                    │                     │                  │     accounts, event_outbox
   │              │                │  ACCOUNT_CREDITED  │                     │                  │
   │              │                │◄──────────────────────────────────────── ┤                  │
   │              │                │ wallet.events      │                     │                  │
   │              │                ├───────────────────►│                     ├─────────────────►│
   │              │                │                    │ CAS status=COMPLETED┼──────────────────┼─── updates: saga_transactions
   │              │                │                    │ upsert SUCCESS ─────┼──────────────────┼─── writes: transaction_outcomes
   │              │◄─ resolve waiter (push registry) ───┤                     │ ledger entry ────┼─── writes: ledger_entries (CREDIT)
   │ 200 success  │                │                    │                     │                  │
   │◄─────────────┤                │                    │                     │                  │
   │              │                │                    │                     │                  │
```

Tables touched, in order:

| Step | Actor | Table | Operation |
|---|---|---|---|
| 1 | Gateway | `saga_transactions`, `transaction_outcomes` | read (idempotency check) |
| 2 | Saga | `saga_transactions` | INSERT … ON CONFLICT DO NOTHING (status PENDING) |
| 3 | Cmd Processor | `processed_commands` | INSERT `tx:DEBIT` (dedup claim) — same tx as 4–5 |
| 4 | Cmd Processor | `accounts` | SELECT … FOR UPDATE on A, then `balance -= 30`, `version += 1` |
| 5 | Cmd Processor | `event_outbox` | INSERT ACCOUNT_DEBITED; UPDATE published=TRUE after Kafka ack |
| 6 | Saga | `saga_transactions` | CAS UPDATE debit_status=DONE |
| 7 | Projector | `ledger_entries` | INSERT DEBIT row (ON CONFLICT event_id DO NOTHING) |
| 8 | Cmd Processor | `processed_commands`, `accounts`, `event_outbox` | same as 3–5 for `tx:CREDIT` on B (`balance += 30`) |
| 9 | Saga | `saga_transactions` | CAS UPDATE credit_status=DONE, status=COMPLETED |
| 10 | Saga | `transaction_outcomes` | INSERT SUCCESS (first writer wins) |
| 11 | Projector | `ledger_entries` | INSERT CREDIT row |

## Detailed Success Flow (step by step)

Transfer: `POST /v1/wallet/balance_transfer` with `{from_account: A, to_account: B, amount: "30", currency: "USD", transaction_id: T}`.

### Phase 1 — Gateway accepts the request

1. The middleware chain runs first: a request id is generated (or propagated from the
   `X-Request-ID` header), panics are trapped, the request is logged, and the body is
   capped at 64 KiB.
2. The handler decodes the JSON body and validates it at the transport level:
   account ids non-empty and ≤ 64 chars, `A ≠ B`, currency is a 3-letter alpha code,
   `amount` is parsed from **string** into int64 minor units (`"30"` → 3000 for USD —
   no floating point anywhere), and `transaction_id` is a valid non-nil UUID.
3. The service checks **idempotency**: it looks up `saga_transactions` by `T`.
   - Found with a *different* body → `409 IDEMPOTENCY_CONFLICT`.
   - Found and already settled (row in `transaction_outcomes`) → return the original
     outcome immediately, nothing is re-produced.
   - Found but in flight → skip producing, just wait for the outcome (step 5).
   - Not found (our case) → proceed.
4. The service **registers a push waiter** in the in-process registry: a buffered
   channel keyed by `T`. This happens *before* producing, so the saga's resolve can
   never race ahead of the registration.
5. The service encodes a `TransferRequest{T, A, B, 3000, USD, now}` and produces it to
   the **`wallet.transfers`** topic (keyed by `T`). The producer is synchronous with
   `acks=all`, so once `Publish` returns the request is durable in Kafka.
6. The handler now **blocks** on the waiter channel for up to `GATEWAY_WAIT_TIMEOUT`
   (default 5 s). The rest of the flow happens asynchronously in other roles.

### Phase 2 — Saga starts the transfer (DEBIT leg)

7. The saga coordinator's transfers consumer fetches the `TransferRequest` and calls
   `Start`: it inserts a row into `saga_transactions` — `{saga_id: S (new), transaction_id:
   T, status: PENDING, debit_status: PENDING, credit_status: PENDING, compensate_status:
   NA}` — using `ON CONFLICT (transaction_id) DO NOTHING`. If a redelivery races, the
   existing row is loaded instead and no second transfer starts.
8. The saga emits the first leg: `Command{TransactionID: T, SagaID: S, Leg: DEBIT,
   Account: A, Counterparty: B, AmountMinor: 3000, Currency: USD}` to the
   **`wallet.commands`** topic, **keyed by A** — so all commands for account A land on
   the same partition, in FIFO order.
9. Only after the saga's handler returns successfully does the consumer **commit the
   Kafka offset** (manual commit). A crash before this point redelivers the transfer,
   which step 7's conflict handling absorbs.

### Phase 3 — Command processor applies the debit

10. The command processor fetches the DEBIT command and runs `ApplyCommand` — **one
    Postgres transaction** containing four writes:
    1. **Claim**: `INSERT INTO processed_commands (command_key) VALUES ('T:DEBIT') ON
       CONFLICT DO NOTHING`. If the row already exists (a Kafka redelivery), the
       previously recorded outbox event is loaded and re-published instead of
       re-applying — this is the exactly-once guard.
    2. **Lock**: `SELECT balance_minor, currency, version FROM accounts WHERE
       account_id = 'A' FOR UPDATE`. The row lock serializes all writers of A across
       every processor instance.
    3. **Decide** (pure domain logic): amount > 0 ✓, currency matches ✓, balance ≥ 3000 ✓
       → decision is `ACCOUNT_DEBITED`. The event is built with a **deterministic
       event id** = UUIDv5("T:DEBIT") and `Seq = version + 1`.
    4. **Apply + outbox**: `UPDATE accounts SET balance_minor = balance_minor - 3000,
       version = Seq WHERE account_id = 'A'`, then `INSERT INTO event_outbox (event_id,
       command_key, account_id, payload)` with the JSON-encoded event.
11. The transaction commits — the balance change, the dedup row, and the event are now
    durable **atomically** (no dual-write gap).
12. The processor publishes the event to the **`wallet.events`** topic (keyed by A) and
    marks the outbox row `published = TRUE`. If it crashes between commit and publish,
    the relay goroutine picks the row up (unpublished, older than 5 s) and publishes it;
    duplicates are deduped downstream by `event_id`.
13. The Kafka offset for the command is committed.

### Phase 4 — Saga advances to the CREDIT leg

14. The saga's events consumer receives `ACCOUNT_DEBITED{T, S, leg: DEBIT}` and runs the
    CAS transition: `UPDATE saga_transactions SET debit_status='DONE' WHERE saga_id=S
    AND debit_status='PENDING' RETURNING …`. Exactly one saga instance wins this update;
    any concurrent or redelivered attempt affects 0 rows and no-ops.
15. The winning instance emits the second leg: `Command{T, S, Leg: CREDIT, Account: B,
    Counterparty: A, 3000, USD}` to `wallet.commands`, **keyed by B**.

### Phase 5 — Command processor applies the credit

16. Same single-transaction sequence as Phase 3, with key `'T:CREDIT'` and a lock on
    B's row: claim → lock → Decide (credits always succeed once the currency check
    passes; if B doesn't exist yet, the row is created and adopts USD) →
    `balance_minor += 3000` → outbox insert.
17. `ACCOUNT_CREDITED` is published to `wallet.events` (keyed by B), outbox row marked
    published, offset committed.

### Phase 6 — Saga completes and the client is answered

18. The saga receives `ACCOUNT_CREDITED` and runs the terminal CAS: `UPDATE
    saga_transactions SET credit_status='DONE', status='COMPLETED' WHERE saga_id=S AND
    status='PENDING' AND credit_status='PENDING'`.
19. It **finalizes**: `INSERT INTO transaction_outcomes (transaction_id, status) VALUES
    (T, 'SUCCESS') ON CONFLICT DO NOTHING` — outcomes are immutable, first writer wins.
20. It **resolves the push waiter** for `T` in the registry (same-process deployment).
    The gateway handler, blocked since step 6, wakes up and returns **`200 {status:
    "success", transaction_id: T}`**. If gateway and saga run in separate processes,
    the resolve is a no-op; the handler times out at 5 s, returns `202 pending`, and the
    client polls `GET /v1/wallet/transaction/T`, which now reads SUCCESS from
    `transaction_outcomes`.

### Phase 7 — Projector builds the ledger (parallel, eventually consistent)

21. Independently, the projector consumes both events from `wallet.events` and inserts
    one row each into `ledger_entries`: a DEBIT row for A and a CREDIT row for B, both
    `ON CONFLICT (event_id) DO NOTHING`. The double-entry view is complete: every
    transfer is exactly one DEBIT plus one CREDIT, rebuildable at any time by replaying
    the topic.

End state: `accounts` — A down 3000, B up 3000, versions bumped; `processed_commands` —
`T:DEBIT`, `T:CREDIT`; `event_outbox` — two published rows; `saga_transactions` —
COMPLETED; `transaction_outcomes` — SUCCESS; `ledger_entries` — one DEBIT + one CREDIT.

## Table Schemas

```
accounts             (account_id, balance_minor, currency, version, created_at, updated_at)
processed_commands   (command_key, account_id, applied_at)
event_outbox         (id, event_id, command_key, account_id, payload, published, created_at)
ledger_entries       (entry_id, event_id, wallet_id, transfer_id, entry_type, amount_minor, created_at)
saga_transactions    (saga_id, transaction_id, from_account, to_account, amount_minor, currency,
                      status, debit_status, credit_status, compensate_status, failure_reason,
                      created_at, updated_at)
transaction_outcomes (transaction_id, status, failure_reason, settled_at)
```

## Purpose of Each Table

| Table | Written by | Purpose |
|---|---|---|
| `accounts` | command-processor | The **authoritative balance** of every wallet, updated under a `SELECT … FOR UPDATE` row lock so per-account writes are serialized across all instances. `version` is the per-account monotonic sequence of balance-changing events. The gateway's balance query reads this directly (read-your-writes correct). |
| `processed_commands` | command-processor | **Write-side idempotency ledger.** `command_key` (`transaction_id:LEG`) is inserted in the *same transaction* as the balance change, so a redelivered Kafka command (at-least-once delivery) hits the primary key and is never applied twice. |
| `event_outbox` | command-processor | **Transactional outbox.** The event is recorded atomically with the balance change (no dual-write gap between Postgres and Kafka). The hot path publishes it right after commit; a relay goroutine republishes any row left unpublished by a crash. |
| `ledger_entries` | projector | **Double-entry ledger read view**, built from the event log. Exactly one DEBIT and one CREDIT row per transfer (a seed deposit is a single CREDIT). Idempotent via the unique `event_id`; fully rebuildable by replaying `wallet.events`. |
| `saga_transactions` | saga coordinator | **Phase-status table of every transfer**: overall status plus per-leg statuses (debit / credit / compensate). Advanced only by atomic CAS updates, so concurrent saga instances can't lose transitions. `transaction_id` unique = client retry idempotency; `updated_at` lets the sweeper find stale in-flight transfers to re-drive. |
| `transaction_outcomes` | saga coordinator | **Terminal result per transaction** (SUCCESS / FAILED + reason), immutable once written. The gateway reads it to answer client retries and the `202 pending` poll path after a push-timeout. |

## Cross-process push: the in-process registry and its alternatives

The push waiter (step 4 / step 20) is an **in-process registry**: a `map[transaction_id]chan`
in the gateway's heap, resolved by a direct channel send when the saga settles. A Go channel
is a pointer into one process's memory, so this only wakes the handler when the gateway and
the saga run in the **same process** (the `all` run-mode). In a split deployment the saga's
`Resolve` finds no waiter in *its* map and is a no-op; the handler blocks the full
`GATEWAY_WAIT_TIMEOUT`, returns `202 pending`, and the client polls
`GET /v1/wallet/transaction/T`.

This is a **correctness-neutral latency optimization**: the durable source of truth is always
`transaction_outcomes` in Postgres, which the poll path reads. The push only saves the client
from waiting out the 5 s timeout. So any cross-process wakeup mechanism may lose a notification
freely — the poll is the backstop.

Two mechanisms were considered to carry the wakeup across machines:

**(a) Redis Pub/Sub channel per transaction (the option we could have followed).** This is the
natural fit for *dynamic per-transaction* wakeup, which Kafka cannot express:

```
  Gateway (machine 1)                         Saga coordinator (machine 2)
  ───────────────────                         ────────────────────────────
  SUBSCRIBE outcome.<T>   ◄── before producing the transfer (no resolve can race ahead)
  produce TransferRequest ──────Kafka──────►  … run saga, settle transaction_outcomes …
  block on handler (≤5s)                      PUBLISH outcome.<T> {success, reason}
        ▲                                            │
        └──────────── Redis routes only to the subscriber of outcome.<T> ──────────────┘
  wake → 200, then UNSUBSCRIBE
```

- The gateway `SUBSCRIBE outcome.<T>` when it registers the waiter; the saga
  `PUBLISH outcome.<T>` when it writes `transaction_outcomes`. Redis delivers straight to the
  one gateway subscribed to that channel — **per-transaction routing, no broadcast, no
  filtering, low per-gateway bandwidth.**
- Channels are **dynamic and unbounded** and cost ~nothing when idle, so a channel per live
  transaction is fine (unlike Kafka partitions, which are a bounded, expensive resource — one
  partition per transaction is infeasible).
- Cleanup is automatic: the channel disappears on `UNSUBSCRIBE`/disconnect (handler wake or
  timeout).
- **Caveat — at-most-once.** Pub/Sub is fire-and-forget: if no subscriber is connected at
  publish time (publish-before-subscribe race, or a gateway reconnecting), the message is
  dropped. We make this safe by subscribing *before* producing (same "register before produce"
  rule as the in-process registry) and by leaning on the Postgres poll backstop for anything
  still missed. Pub/Sub is therefore only ever the fast path, never the system of record.
- If losing the push on a reconnect were unacceptable, **Redis Streams** (`XADD`/`XREAD` on a
  per-transaction stream) would add persistence and at-least-once recovery — at the cost of
  per-key stream TTL/trimming. Given the poll backstop already recovers missed pushes, the
  extra machinery usually isn't worth it.
- Note: in **Redis Cluster**, plain Pub/Sub doesn't fan out across all nodes — use sharded
  Pub/Sub (`SSUBSCRIBE`/`SPUBLISH`) so publisher and subscriber needn't share a node.

**(b) A `wallet.outcomes` Kafka topic (broadcast).** The saga publishes the terminal outcome
(`{transaction_id, success, reason}`, keyed by `transaction_id`); every gateway consumes the
topic under a **unique consumer group** (so each instance is a sole member and is assigned all
partitions) starting at the **latest** offset, and replays each outcome into its local registry.
Routing falls out for free: `registry.Resolve` wakes the one waiter that matches and is a no-op
elsewhere. No new infrastructure, but **every gateway reads the entire system-wide outcome
stream** to catch its own few — Kafka has no per-key subscription (a key only selects a
partition via `hash(key) % N`, and a partition carries many keys).

The implemented design keeps the in-process registry for `all` mode and the `202 + poll`
fallback for split deployments. Either (a) or (b) is a drop-in upgrade for the split case
because `saga.Resolver` is already an interface and the gateway already owns the registry whose
`Resolve` is safe to call for unknown ids — only the resolver implementation and a gateway-side
listener change.
