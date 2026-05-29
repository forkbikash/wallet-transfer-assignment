# Digital Wallet — Kafka + CQRS + Saga (distributed, DB-authoritative)

A backend for cross-wallet balance transfers. It started from the **ByteByteGo Ch.28 "Design A
Digital Wallet"** design (`doc/digitalwalletdesign.md`) and was then hardened for a **distributed,
multi-instance production** deployment:

- **Kafka** carries commands and an immutable event log; **Postgres** holds authoritative state.
- **Balances are DB-authoritative** — written under a per-account row lock (`SELECT … FOR UPDATE`),
  with the event recorded atomically via a **transactional outbox**. Every worker is stateless, so it
  scales horizontally and Kafka rebalancing is harmless.

> Full rewrite of the earlier synchronous service; that design is gone.

## Why this shape

- **DB-authoritative write side** → a per-account row lock serializes writers across *all* instances,
  giving strong consistency and no-double-spend without depending on partition ownership.
- **Event log (via outbox)** → one event per balance change drives the saga, the ledger view, audit,
  and replay-reproducibility (balance = sum of deltas, order-independent).
- **CQRS** → the double-entry `ledger_entries` view is built from the log, independent of the write path.
- **Saga** → a transfer touches two accounts (possibly different partitions), so there is no single DB
  transaction; the Saga makes it atomic by compensation, advanced via atomic compare-and-set.
- **Kafka durability** (RF / `acks=all` / `min.insync.replicas`) replaces the chapter's file+Raft.

## Architecture

```
   client ─POST─▶ GATEWAY ─TransferRequested▶ [wallet.transfers] ─▶ SAGA
                    ▲                                                 │ DEBIT/CREDIT cmd
                    │ resolve(outcome)                                ▼
                    │                                          [wallet.commands]
                    │                                                 │
              (push registry, all-mode)                              ▼
                    │                                         COMMAND-PROCESSOR
                    │                                         Decide(cmd,balance)→Event
                    │                                                 │ emit
                    │                                          [wallet.events]  ◀── immutable log
                    │                                            │          │
                    │                              consume ◀─────┘          └────▶ consume
                    │                            PROJECTOR                        SAGA (advance)
                    └───────────────── GET balance ◀── accounts (read model) ── POSTGRES
```

| Role (`--mode`) | Responsibility |
|---|---|
| `gateway` | HTTP API + reverse proxy (push model: block for the outcome, fall back to 202 + poll) |
| `command-processor` | **Stateless** write side: validate a leg command against the `accounts` row under `SELECT … FOR UPDATE`, update the balance, dedup the command, and write the event to a transactional **outbox** — all in one transaction — then publish it |
| `projector` | CQRS read side: build the double-entry `ledger_entries` view from the event log (idempotent) |
| `saga` | Orchestrate a transfer: debit→credit, compensation on failure; phase-status advanced by atomic **compare-and-set**; a `SKIP LOCKED` sweeper re-drives stale sagas |

One binary, `--mode` selects the role; `all` runs every role in one process (dev/tests) with an
in-process push registry so a POST resolves synchronously.

> **Distributed-ready (multi-instance).** The write side is **DB-authoritative**: balances are
> strongly consistent (written under a per-account row lock), so every worker is stateless and any
> number of instances can run — Kafka consumer-group **rebalancing is harmless** because correctness
> comes from the row lock + command dedup, not partition ownership. The event log remains complete
> (one event per balance change, via the outbox) for the saga, the ledger view, audit, and replay.

**Layering (handler → service → repository → domain).** The gateway handler is transport-only
(decode, validate, map status). The transfer workflow — idempotency, producing onto the log, the
push-model wait — lives in `internal/wallet/service`. Repositories (`repo/postgres`) are
persistence-only. `domain` holds the entities and the `Decide`/`Apply` rules.

Dependencies are inverted onto **role-segregated interfaces** (ISP): the service on
`AccountReader`/`SagaFinder`/`OutcomeReader`, the projector on `LedgerWriter`, the command-processor
on `CommandApplier`/`OutboxRelay`, the saga on `SagaStore`/`OutcomeWriter` plus its own
`Producer`/`Resolver`. The Postgres adapters implement the relevant sets; the composition root
(`cmd/wallet`) wires concrete types in.

### State machine determinism

`internal/wallet/domain/statemachine.go` splits the machine into:

- `Decide(cmd, balance) → event` — validation (funds, currency); may reject.
- `Apply(balance, event) → balance` — **pure & deterministic**; the replay function. Folding it over
  the event log reproduces every balance exactly.

### Terminology — mapping to the ByteByteGo chapter

The chapter overloads the word **command** at two levels: the outside-world transfer request *and*
the per-account leg the Saga sends to a partition (*"the coordinator sends A-\$1 as a command to
Partition 1… validates the command. If valid, it is converted into an event."*). We give the outer
one its own name so the two don't collide:

| This code | Chapter term | Intent or fact? |
|---|---|---|
| **transfer** (`wallet.transfers`, `TransferRequested`) | the outside-world **command** (the transfer request) | intent |
| **command** (`domain.Command` — a DEBIT/CREDIT/COMPENSATE leg, `wallet.commands`) | the per-partition leg **command** (`A-$1`) | **intent** (still a command) |
| **event** (`domain.Event` — `ACCOUNT_DEBITED`/`CREDITED`/`TRANSFER_REJECTED`, `wallet.events`) | the **event** (`A:-$1`) | fact |

So the chain `transfer → command(leg) → event` in this code is exactly the chapter's
`command(transfer) → command(leg) → event`. Note that **`domain.Command` is *not* the chapter's
event** — it is the leg-level *command* (an intent), which `Decide` validates into an event. The
give-away: a `domain.Command` can be **rejected** (`Decide → TRANSFER_REJECTED`); a `domain.Event` is
a validated fact that already happened and is never rejected.

## Data

**Kafka topics** (key = account id): `wallet.transfers` (gateway→saga), `wallet.commands`
(saga→command-processor), `wallet.events` (command-processor→projector+saga; the audit/integration
log), `wallet.dlq` (poison messages).

**Postgres** (one migration, `migration/0001_init.up.sql`):
`accounts` (**authoritative** balance, written under a row lock), `processed_commands`
(write-side exactly-once dedup), `event_outbox` (transactional outbox), `ledger_entries`
(double-entry view — one DEBIT + one CREDIT per transfer, `UNIQUE(transfer_id, entry_type)`),
`saga_transactions` (phase-status, advanced by CAS), `transaction_outcomes` (terminal results).

## API

```
POST /v1/wallet/balance_transfer
{ "from_account":"alice", "to_account":"bob", "amount":"30.00",   // amount is a STRING (no float)
  "currency":"USD", "transaction_id":"<uuid>" }                   // ISO-4217; uuid = dedup key

→ 200 {"status":"success","transaction_id":...}
  422 {"status":"failed","reason":"INSUFFICIENT_FUNDS|CURRENCY_MISMATCH",...}
  202 {"status":"pending",...}     # push deadline elapsed — poll the status endpoint
  409                              # same transaction_id, different body (idempotency conflict)

GET /v1/wallet/transaction/{transaction_id}   # poll for the 202 fallback path
GET /v1/wallet/{account}/balance              # CQRS read model
GET /healthz
```

In a split deployment the gateway can't share the push registry with the saga, so it returns `202`
and the client polls `GET .../transaction/{id}` (served from `transaction_outcomes`). In `all` mode
the POST resolves synchronously.

## Failure handling (Saga)

- **Debit rejected** (insufficient funds / source currency): saga → FAILED, no money moved, `422`.
- **Credit rejected after debit committed**: saga → COMPENSATING → emit a compensating credit that
  refunds the source → COMPENSATED, `422`. Debit-before-credit means we never claw back money a
  third party could already have spent.
- **Duplicate / out-of-order leg events**: each saga transition is an atomic **compare-and-set**
  (`UPDATE … WHERE <precondition> RETURNING`); a redelivered or late event whose precondition no
  longer holds advances nothing, so concurrent saga instances and replays are safe no-ops.
- **Saga crash / stuck transfer**: `saga_transactions` is durable; a periodic **`SKIP LOCKED`
  sweeper** re-drives non-terminal sagas whose `updated_at` has gone stale (a lost leg event, a crash
  mid-flight) and surfaces ones stuck too long. `SKIP LOCKED` shards the work across instances.
- **Command-processor / projector crash**: Kafka redelivers; the write side dedups via
  `processed_commands` and the projector via `ledger_entries` unique keys, with commit-DB-then-offset
  making re-apply a no-op (effectively-once). The **outbox relay** re-publishes any event a crash left
  unpublished.

## Running locally

Requires Docker (Kafka KRaft + Postgres). Bring up the full stack:

```sh
make compose-up      # Kafka + Postgres + migrate + gateway/command-processor/projector/saga
make compose-seed    # seed demo accounts: alice=100.00 USD, bob=10.00 USD

# transfer (split-mode returns 202; poll the status endpoint)
curl -XPOST localhost:8080/v1/wallet/balance_transfer -H 'Content-Type: application/json' \
  -d '{"from_account":"alice","to_account":"bob","amount":"30.00","currency":"USD","transaction_id":"'$(uuidgen)'"}'

curl localhost:8080/v1/wallet/alice/balance   # {"account":"alice","balance":"70.00","currency":"USD"}
make compose-down
```

Run from source against just the infra:

```sh
make infra-up        # Kafka + Postgres + migrate only
make run-all         # every role in one process (POST resolves synchronously)
make seed            # seed demo accounts from the host
```

Config is via env: `KAFKA_BROKERS`, `KAFKA_PARTITIONS`, `KAFKA_REPLICATION_FACTOR`, `DATABASE_URL`,
`GATEWAY_WAIT_TIMEOUT`, `PORT`, `LOG_LEVEL`, `LOG_JSON`.

## Testing

```sh
make test       # unit: state-machine determinism, money parsing, saga happy/F1/F2,
                # service workflow (idempotency/replay/timeout), bounded dedup set — all with fakes
make test-int   # integration (testcontainers Kafka + Postgres): happy path, insufficient funds,
                # idempotent replay, idempotency conflict, compensation, double-entry ledger,
                # concurrent debits (no double-spend), reproducibility
```

`test-int` needs a Docker daemon. With colima/podman, point testcontainers at the socket, e.g.:

```sh
export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
make test-int
```

## Layout

```
cmd/wallet/                  # entrypoint, --mode dispatch
internal/wallet/
  domain/                    # Command, Event, Decide/Apply (deterministic core)
  kafka/                     # producer, manual-commit consumer (+ DLQ), offline replay
  command/                   # stateless write side (CommandStore + outbox relay)
  projector/                 # CQRS read-side projector (ledger_entries view)
  saga/                      # orchestrator + CAS transitions + SKIP LOCKED sweeper
  service/                   # transfer workflow (business layer, between handler and repos)
  gateway/                   # thin HTTP handlers (reverse proxy / push model)
  registry/                  # in-process pending-waiter registry
  repo/{iface,postgres}/     # role-segregated repo interfaces + Postgres adapters
  test/integration/          # testcontainers end-to-end tests (build tag: integration)
migration/                   # SQL migrations (golang-migrate)
common/, config/             # shared money/error/middleware/health + config + infra
```

## Distributed / production notes

- **Scale out**: every worker is stateless. `docker compose up -d --scale command-processor=3
  --scale saga=2` (or N replicas in k8s). Correctness holds under consumer-group rebalancing because
  the per-account row lock + `processed_commands` dedup serialize writers across instances.
- **Kafka durability**: **3 brokers, RF=3, min.insync.replicas=2, acks=all** in prod (local dev uses
  1 broker / RF=1). Pre-provision topics via IaC; `EnsureTopics` is a dev convenience.
- **Exactly-once-ish**: transactional outbox (event atomic with the balance) + deterministic
  `event_id` + downstream dedup (`ledger_entries.event_id`, saga CAS) + commit-DB-then-Kafka-offset.
- **Recovery**: a saga `SKIP LOCKED` sweeper re-drives stale in-flight transfers (a lost leg event,
  a crash mid-flight) and surfaces stuck ones; the outbox relay re-publishes events a crash left
  unpublished.
- **Health**: `/healthz` = liveness; `/readyz` = Postgres + Kafka reachable (gate the LB on it).
- **Poison messages** go to `wallet.dlq` instead of being dropped.
- `ledger_entries`/`event_outbox` grow with history; partition/archive by time (purge published
  outbox rows) in a long-running deployment.
- **Not yet implemented (documented in the plan):** metrics (Prometheus), distributed tracing
  (OpenTelemetry by `transaction_id`), and security (API authn/authz, Kafka mTLS, Postgres TLS, rate
  limiting). These are production prerequisites tracked as follow-ups, not part of the current code.
