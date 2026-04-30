# wallet-transfer-assignment

A wallet-to-wallet transfer service in Go, backed by PostgreSQL. The service
exposes a single endpoint:

```text
POST /transfers
{
  "idempotencyKey": "abc123",
  "fromWalletId":   "wallet_1",
  "toWalletId":     "wallet_2",
  "amount":         100
}
```

The implementation focuses on correctness, robustness, and clarity: idempotent
retries, atomic transfer execution, deadlock-free pessimistic locking, and a
double-entry ledger that always balances.

---

## Architecture

Layered, with each layer depending only on the layer below:

- Copilot automatic pull request review is configured in GitHub repository or organization settings, not purely through files in the repo.
- The `copilot-instructions.md` file included here provides repository-specific review guidance once Copilot review is enabled.
- The CI workflow is language-agnostic by default and expects you to set the `LINT_CMD`, `FORMAT_CHECK_CMD`, and `TEST_CMD` repository variables or replace the commands directly.

## How to Submit Assignment

1. **Fork this repository** to your own GitHub account.
2. Complete the assignment described in [`ASSIGNMENT.md`](./ASSIGNMENT.md).
3. **Raise a Pull Request** back to this repository (`main` branch) with your full solution.

Your PR branch should be named: `solution/<your-name>` (e.g., `solution/jane-doe`).

# PR README

```text
HTTP handler  ->  service (orchestration)  ->  repository (Postgres)  ->  database
```

- **Domain models** (`services/transfer/internal/data/model`) carry the
  state-machine logic for `Transfer` and the `Wallet` / `LedgerEntry` types.
- **Service** (`services/transfer/internal/svc/impls`) orchestrates idempotency,
  locking, validation, ledger posting, and balance updates inside a single
  transaction.
- **Repositories** (`services/transfer/internal/repo/postgres`) own SQL only.
- **Handler** (`services/transfer/internal/handler`) decodes JSON, calls the
  service, and maps results to HTTP responses. No business logic.

Money is stored as `int64` minor units (paisa). Floating-point arithmetic is
never used.

### Directory layout

```text
cmd/server/                                       boot, DI wiring, graceful shutdown
services/transfer/init                             service initializer
services/transfer/route                            HTTP route registration
services/transfer/internal/svc/{ifaces,impls}      service interface + implementation
services/transfer/internal/repo/{iface,postgres}   repo interfaces + Postgres impls
services/transfer/internal/data/{model,request,response}  domain types and DTOs
services/transfer/internal/handler                 HTTP handler
common/util/money                                  Money type
common/error                                       AppError + sentinel errors
common/middleware                                  request_id, recovery, logging, error_writer
config/{init,infra}                                env-based config + infra clients
migrations                                         SQL migrations
tests/integration                                  integration & concurrency tests
```

---

## Schema

```sql
CREATE TABLE wallets (
    id            VARCHAR(64)  PRIMARY KEY,
    balance_minor BIGINT       NOT NULL DEFAULT 0 CHECK (balance_minor >= 0),
    currency      VARCHAR(3)   NOT NULL CHECK (char_length(currency) = 3),
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE transfers (
    id              UUID         PRIMARY KEY,
    idempotency_key VARCHAR(255) NOT NULL UNIQUE,
    request_hash    VARCHAR(64)  NOT NULL CHECK (char_length(request_hash) = 64),
    from_wallet_id  VARCHAR(64)  NOT NULL REFERENCES wallets(id),
    to_wallet_id    VARCHAR(64)  NOT NULL REFERENCES wallets(id),
    amount_minor    BIGINT       NOT NULL CHECK (amount_minor > 0),
    currency        VARCHAR(3)   NOT NULL CHECK (char_length(currency) = 3 OR currency = ''),
    status          VARCHAR(16)  NOT NULL CHECK (status IN ('PENDING','PROCESSED','FAILED')),
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (from_wallet_id <> to_wallet_id)
);

CREATE TABLE ledger_entries (
    id           BIGSERIAL    PRIMARY KEY,
    transfer_id  UUID         NOT NULL REFERENCES transfers(id),
    wallet_id    VARCHAR(64)  NOT NULL REFERENCES wallets(id),
    entry_type   VARCHAR(8)   NOT NULL CHECK (entry_type IN ('DEBIT','CREDIT')),
    amount_minor BIGINT       NOT NULL CHECK (amount_minor > 0),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (transfer_id, entry_type)
);

CREATE INDEX idx_ledger_wallet_created ON ledger_entries(wallet_id, created_at DESC);
CREATE INDEX idx_transfers_from_wallet ON transfers(from_wallet_id);
CREATE INDEX idx_transfers_to_wallet   ON transfers(to_wallet_id);
```

Key invariants:

- `wallets.balance_minor >= 0` is the last-line defense against double-spending.
- `amount_minor > 0` and `from_wallet_id <> to_wallet_id` reject impossible
  transfers at the database layer.
- `UNIQUE(transfer_id, entry_type)` encodes "exactly one DEBIT and one CREDIT
  per transfer" directly in the schema.
- `UNIQUE(idempotency_key)` is the idempotency record itself.

Indexes (auto-created PK and UNIQUE indexes plus three explicit ones):

- `idx_ledger_wallet_created (wallet_id, created_at DESC)` — supports any
  per-wallet history scan and doubles as the FK-enforcement index for
  `ledger_entries.wallet_id`.
- `idx_transfers_from_wallet`, `idx_transfers_to_wallet` — Postgres does not
  auto-index the *referencing* side of a foreign key, so without these the
  FK-enforcement check (or any "transfers from/to wallet X" query) would
  scan the whole `transfers` table.
- The `UNIQUE(transfer_id, entry_type)` constraint on `ledger_entries`
  doubles as a `transfer_id`-prefix index, so we don't need a separate one.

In CQRS-lite framing:

- `ledger_entries` is the **immutable event log** (append-only, the audit trail).
- `wallets.balance_minor` is the **materialized read view** of the ledger
  (denormalized for O(1) authorization checks).
- `transfers.status` is the committed outcome of each request — the per-request
  "phase status" that lets duplicates replay the original outcome.

---

## Idempotency strategy

The contract is `exactly-once = at-least-once + at-most-once`:

1. **Clients retry** failed requests with the same `idempotencyKey` (at-least-once).
2. **The server deduplicates** via the unique constraint on
   `transfers.idempotency_key` (at-most-once).

The duplicate path is implemented inside the same transfer transaction:

```sql
INSERT INTO transfers (...) VALUES (..., 'PENDING')
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING id;
```

If the row insert succeeds, the rest of the transfer (lock wallets, validate,
append ledger, update balances, mark PROCESSED/FAILED) proceeds in the same
transaction.

If the insert finds an existing key, Postgres' **unique-index xmax lock**
already blocked the second statement until the original transaction committed
or rolled back. We then `SELECT` the row — it reflects committed state — and
return the original outcome. We prefer this Stripe-style replay (block + return
original) over `429 Too Many Requests`: the client gets the actual outcome on
the first retry rather than another retry cycle.

**Strict body-match contract**: each transfer row stores `request_hash`
(SHA-256 of `<fromWalletId>|<toWalletId>|<amountMinor>`). A duplicate request
with the same key but a different body returns `409 IDEMPOTENCY_CONFLICT`.

**No orphan idempotency claims**: because the INSERT is part of the transfer
transaction, a rollback releases the unique-key claim. Replays of a
rolled-back attempt execute fresh.

---

## Concurrency strategy

We use **READ COMMITTED + `SELECT ... FOR UPDATE`** with deterministic lock
ordering:

```sql
SELECT id, balance_minor, currency, ...
FROM wallets
WHERE id = ANY($1::text[])
ORDER BY id
FOR UPDATE
```

- A single round-trip locks both wallet rows.
- Sorting wallet IDs lexicographically before the SELECT is what guarantees
  deadlock-freedom: any two transactions touching the same pair always acquire
  locks in the same order.
- Once both rows are locked, balance validation, ledger insertion, and balance
  updates run inside the critical section.
- The DB-level `CHECK (balance_minor >= 0)` is the last-line defense against
  any application-layer bug that lets a transfer past the validation gate.

We deliberately avoid `SERIALIZABLE`: it would require a `40001` retry loop on
serialization failure, and the cost-benefit is unfavorable for a small,
well-understood critical section that is already deadlock-free.

---

## State machine

`PENDING` is a transient mid-transaction state and is **never observed in
committed state** under normal operation: every transaction either updates the
status to `PROCESSED` or `FAILED` before commit, or rolls back entirely.

`PENDING` is allowed by the `CHECK` constraint so the initial INSERT can store
the row before its outcome is known, but expect to see only
`PROCESSED` / `FAILED` in committed state.

A failure that is a **business outcome** (insufficient funds, currency
mismatch) commits a `FAILED` row; a duplicate retry replays the same FAILED
outcome rather than re-attempting the doomed transfer.

---

## API contract & side effects

### `POST /transfers`

Request body:

```json
{
  "idempotencyKey": "abc123",
  "fromWalletId":   "wallet_1",
  "toWalletId":     "wallet_2",
  "amount":         100
}
```

Validation:

- `idempotencyKey` is required and must be ≤ 255 chars
- `fromWalletId` and `toWalletId` are required, must differ, and must be ≤ 64 chars
- `amount` must be a positive integer in minor units
- Body must be ≤ 64 KiB and contain no unknown fields

Status mapping:

| Status | When |
|---|---|
| `201 Created` | Fresh transfer was processed and committed |
| `200 OK` | Idempotent replay — same key, same body, original outcome returned |
| `400 Bad Request` | Validation error (`SAME_WALLET`, `INVALID_AMOUNT`, `INVALID_WALLET_ID`, `INVALID_IDEMPOTENCY_KEY`, `BAD_JSON`) |
| `404 Not Found` | One or both of `fromWalletId` / `toWalletId` does not exist |
| `409 Conflict` | Same idempotency key reused with a different request body (`IDEMPOTENCY_CONFLICT`) |
| `413 Payload Too Large` | Request body exceeded the 64 KiB cap |
| `422 Unprocessable Entity` | Fresh `FAILED` outcome (`INSUFFICIENT_FUNDS`, `CURRENCY_MISMATCH`) |
| `5xx` | Internal error |

Success response (`201` or `200`):

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "idempotencyKey": "abc123",
  "fromWalletId": "wallet_1",
  "toWalletId": "wallet_2",
  "amount": 100,
  "currency": "INR",
  "status": "PROCESSED",
  "createdAt": "2026-04-28T12:34:56Z",
  "updatedAt": "2026-04-28T12:34:56Z",
  "replayed": false
}
```

Failed response (`422` for fresh failure, `200` for FAILED replay):

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "idempotencyKey": "abc123",
  "fromWalletId": "wallet_1",
  "toWalletId": "wallet_2",
  "amount": 99999,
  "currency": "INR",
  "status": "FAILED",
  "failureReason": "INSUFFICIENT_FUNDS",
  "createdAt": "2026-04-28T12:34:56Z",
  "updatedAt": "2026-04-28T12:34:56Z",
  "replayed": false
}
```

Error response (`4xx` / `5xx`):

```json
{
  "code": "IDEMPOTENCY_CONFLICT",
  "message": "idempotency key reused with a different request body",
  "request_id": "8f4e2c10-7a91-4f8d-9b3e-c5d2e1a0b8f4"
}
```

The `request_id` field carries the per-request correlation id so a client can quote the body verbatim in a bug report and an operator can grep server logs by that id.

Every response carries an `X-Request-ID` header (echoed from the request, or generated if absent) so a client can correlate its own logs with the server's.

Side effects of a successful (`201`) request:

- Inserts one row into `transfers` with the canonical idempotency key, request hash, currency, and `status = PROCESSED`.
- Inserts exactly two rows into `ledger_entries`: one `DEBIT` against the source wallet, one `CREDIT` against the destination, both for the same amount.
- Updates `wallets.balance_minor` and `updated_at` on both wallets, in one `UPDATE … CASE WHEN …` statement.
- Emits a structured `slog` line (`level=info`) including request id, method, path, status, and duration.

Side effects of a `422 FAILED` request:

- Inserts one row into `transfers` with `status = FAILED` and `failure_reason` set.
- Emits a `level=warn` log line with the failure reason.
- No ledger entries are written; balances are unchanged.

Side effects of a `409 IDEMPOTENCY_CONFLICT`:

- No state changes. The original transfer row stands.
- Emits a `level=warn` log line with the conflicting idempotency key and the existing transfer ID.

### `GET /healthz`

Returns `200 {"status":"ok"}` when the DB pool ping succeeds (1-second timeout) and `503 {"status":"down","reason":"database not reachable"}` otherwise.

---

## Observability

The service emits structured logs via `log/slog` configured by `LOG_JSON` (default `true`) and `LOG_LEVEL` (default `info`). Notable events:

| Event | Level | Fields |
|---|---|---|
| Per-request access log | `info` | `method`, `path`, `status`, `duration_ms`, `request_id` |
| Idempotent replay | `info` | `idempotency_key`, `transfer_id`, `original_status` |
| Idempotency conflict | `warn` | `idempotency_key`, `existing_transfer_id` |
| Transfer failed | `warn` | `transfer_id`, `idempotency_key`, `reason`, `from_wallet_id`, `to_wallet_id`, `amount_minor` |
| Panic recovered | `error` | `panic`, `stack`, `request_id` |
| Server lifecycle | `info` | `addr` on start, plain message on shutdown |

Every inbound request is tagged with an `X-Request-ID` header (generated by the `RequestID` middleware if not supplied by the caller) and the same value is threaded through `slog` so a single request's log lines can be grep'd from a JSON log stream.

Out of scope for v1: Prometheus metrics, OpenTelemetry tracing, audit-trail exports. The ledger itself is the audit trail; metrics and traces would be the next addition for production rollout.

---

## How to run

### Prerequisites

- Go 1.22+ (CI runs 1.24)
- Docker + Docker Compose (for the local Postgres)
- [`golang-migrate`](https://github.com/golang-migrate/migrate/tree/master/cmd/migrate) CLI for `make migrate-{up,down}`
  ```bash
  brew install golang-migrate          # macOS
  ```
- `psql` (for `make seed`; ships with `postgresql-client` on most distros)
- Optional: `golangci-lint` v2.5.0+ for `make lint` locally
  ```bash
  brew install golangci-lint           # macOS
  ```

Copy `.env.example` to `.env` if you want to override any of the defaults
the `Makefile` already sets.

### Bring up the service

```bash
# Start a local Postgres and apply migrations.
make pg-up
make migrate-up
make seed         # inserts wallet_1 (10000 paisa) and wallet_2 (0 paisa), both INR

# Run the server (defaults: PORT=8080, DATABASE_URL from Makefile).
make run

# In another terminal, exercise the API:
curl -sS -X POST localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -d '{"idempotencyKey":"k1","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}'
# => 201 Created, transfer PROCESSED

# Idempotent replay of the same body:
curl -sS -X POST localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -d '{"idempotencyKey":"k1","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}'
# => 200 OK, replayed=true, identical id

# Insufficient funds (committed as FAILED):
curl -sS -X POST localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -d '{"idempotencyKey":"k2","fromWalletId":"wallet_2","toWalletId":"wallet_1","amount":99999}'
# => 422 Unprocessable, status FAILED, failureReason INSUFFICIENT_FUNDS

# Self-transfer (rejected at the handler):
curl -sS -X POST localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -d '{"idempotencyKey":"k3","fromWalletId":"wallet_1","toWalletId":"wallet_1","amount":1}'
# => 400 SAME_WALLET

# Same idempotency key with a different body:
curl -sS -X POST localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -d '{"idempotencyKey":"k1","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":999}'
# => 409 IDEMPOTENCY_CONFLICT
```

To run the full stack (app + Postgres) under Docker Compose:

```bash
make compose-up
make migrate-up         # uses DATABASE_URL pointing at the host-published port
make seed
# the app is on http://localhost:8080
make compose-down
```

---

## How to test

```bash
# Unit tests with the race detector (no DB needed).
make test

# Integration + concurrency tests (requires a running Postgres).
# Note: for assignment scope, the integration tests default to the SAME
# Postgres instance used by `make run` / `make seed`. Each test TRUNCATEs the
# tables in setUp, so running `make test-int` will wipe any interactively
# seeded rows. Override INTEGRATION_DATABASE_URL to use a separate database.
make pg-up
make migrate-up
INTEGRATION_DATABASE_URL=postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable \
  make test-int

# Lint + format check.
make lint
make fmt-check
```

The unit tests cover the money type, the transfer state machine, and the
service orchestration with hand-written fakes (S1-S9).

The integration tests (`-tags=integration`) cover:

| ID  | What it asserts                                                                  |
|-----|----------------------------------------------------------------------------------|
| I1  | Happy path over HTTP - 201, balances and ledger correct                          |
| I2  | Idempotent replay - 200, replayed=true, identical id, balances unchanged         |
| I3  | Ledger zero-sum and stored-balance == initial + ledger net (reproducibility)     |
| I4  | Insufficient funds - 422 FAILED; replay returns the same FAILED                  |
| I5  | Same key, different body - 409 IDEMPOTENCY_CONFLICT, original transfer intact    |

Concurrency tests:

| ID  | What it asserts                                                                  |
|-----|----------------------------------------------------------------------------------|
| C1  | 50 goroutines debit one wallet, exactly 30 succeed and 20 are FAILED             |
| C2  | 100 goroutines submit the same idempotency key, exactly 1 transfer + 2 ledger    |
| C3  | Cross-wallet bidirectional load, no deadlocks, balances net to zero change       |
| C4  | Mixed load (40 same-key + 40 unique), invariants from I3 hold                    |

---

## Tradeoffs / Assumptions

- **`request_hash` is enforced** to make idempotency strict: same key + different
  body returns `409 IDEMPOTENCY_CONFLICT`.
- **No async worker / outbox**. `PENDING` is a transient mid-transaction state.
  Process crashes mid-transaction roll back the row entirely; no zombie rows.
- **READ COMMITTED + `FOR UPDATE`** is preferred over `SERIALIZABLE`: the
  pessimistic lock on the participating wallets is the canonical way to avoid
  the read-then-write race, and it does not require `40001` retry handling.
- **Per-entity repositories** (Wallet, Transfer, Ledger) are kept separate to
  match the convention from an internal Go service codebase from a prior
  project. Each repo is small (2-3 methods).
- **Stripe-style replay** (block + return original) is preferred over
  ByteByteGo's `429 Too Many Requests` mid-flight choice: the contract is
  identical and the client gets the actual outcome on the first retry.
- **No `wallet_updated` / `ledger_updated` per-side-effect flags** on the
  transfer row (which the ByteByteGo Payment System chapter uses to track
  partial completion across async services). Single-transaction atomicity makes
  per-side-effect tracking redundant.
- **Amount is decoded as a JSON number** per the assignment spec. A
  production-grade API would decode it as a string to avoid any client-side
  float-precision risk.
- **Single currency per transfer**: source and destination wallets must share a
  currency; otherwise the transfer is committed as FAILED with reason
  `CURRENCY_MISMATCH`. We do not perform foreign-exchange conversion.
- **GORM as a connection-and-transaction manager only.** Persistence uses
  `gorm.io/gorm` for pool / tx-lifecycle / `WithContext` propagation, but every
  statement in `services/transfer/internal/repo/postgres/*` is hand-written
  parameterized SQL invoked via `tx.Raw(sql, args...).Row().Scan(...)` or
  `tx.Exec(sql, args...)` — **no GORM ORM, query-builder, AutoMigrate, hooks,
  or struct-tag scanning.** The schema is owned by the migrations, not by
  reflection over Go structs. We set `gorm.Config{TranslateError: false,
  SkipDefaultTransaction: true, Logger: Silent}` so driver errors (SQLSTATE
  `23503`/`23514` for FK / CHECK violations) propagate verbatim and so the
  manual `txManager` is the single source of transactional truth.

---

## Optional enhancements (ASSIGNMENT.md) — disposition

`ASSIGNMENT.md` lists five enhancements as *optional and not required for
evaluation*. Each one was considered explicitly:

| Enhancement | Status | Rationale |
|---|---|---|
| Wallet balance API | **Not built** | A `GET /wallets/{id}/balance` endpoint adds a read-side surface that is straightforward but tangential to the core requirements. The DB row + the integration helpers are sufficient for verification in tests; a CLI evaluator can `psql` against the seeded DB. |
| Transfer history API | **Not built** | Same as above; would require a paginated `GET /wallets/{id}/transfers`. |
| Observability / logging | **Built** | Structured `slog` with request-id correlation; see the [Observability](#observability) section for the event table. |
| Metrics | **Not built** | Prometheus / OTel metrics would be the obvious next addition. The structured logs already carry duration and status, so a log-derived dashboard is the v1 substitute. |
| Retry-safe workflows | **Built (at the API layer)** | The full retry contract is implemented: idempotency-key dedup, request-hash strict-body match, FAILED states are committed (not rolled back) so retries replay the same outcome, and the unique-index xmax lock blocks concurrent retries until the original commit is visible. There are no async hops to retry, so a server-side outbox / DLQ is not needed at this scope (see [Design philosophy](#design-philosophy--connection-to-broader-system-design) for what scaling beyond a single Postgres node would require). |

The "Built" rows are tested:

- Idempotency replay: `S7`, `S8`, `I2`, `I4` (FAILED replay), `C2` (concurrent replay)
- Strict-body match: `S9`, `I5`
- Observability: every request emits a structured access log with `request_id`; failure paths emit `warn`-level logs (verified by the manual run-curl walkthrough)

---

## Design philosophy & connection to broader system design

The full ByteByteGo Digital Wallet (Ch. 28) and Payment System (Ch. 27) designs
target 1M-TPS production-grade systems with distributed transactions (TC/C,
Saga, 2PC), event sourcing, Raft consensus, mmap/RocksDB storage, CQRS push,
PSP integration, retry queues, dead-letter queues, and reconciliation jobs.
Most of that machinery is operationally out of scope for a 3-5 hour single-node
exercise - but the **mental models** are not.

| Production concept                                  | Our v1 equivalent                                                  |
|-----------------------------------------------------|--------------------------------------------------------------------|
| Immutable event log (Kafka, file-based)             | `ledger_entries` (append-only, `BIGSERIAL`)                        |
| Materialized state (RocksDB cache)                  | `wallets.balance_minor` (denormalized for O(1) authz)              |
| Phase status table (TC/C bookkeeping)               | `transfers.status` (`PROCESSED` / `FAILED`)                         |
| Distributed transaction (TC/C, Saga, 2PC)           | Single Postgres transaction with `SELECT FOR UPDATE`                |
| Raft consensus replication                          | Postgres streaming replication (operator concern)                  |
| CQRS write/read separation                          | Same DB, ledger=write-side, balance=read-view, test asserts equality |
| Reproducibility (replay events)                     | Test I3: `SUM(ledger)` per wallet == `wallets.balance_minor`        |
| Reconciliation job                                  | Replaced by write-time invariants (CHECK constraints + I3)          |
| Retry queue + dead-letter queue                     | Not built - single-DB transaction has no async hops to fail        |
| Exactly-once = at-least-once + at-most-once         | Adopted as our framing; client retries + unique constraint         |

### Scaling path (deliberately not built)

If this service ever needed to scale toward production-grade throughput:

1. **Read replicas** for balance/history queries.
2. **Sharding by wallet ID** (e.g. via Citus) - would require TC/C or Saga at
   the service layer for cross-shard transfers.
3. **Outbox + Kafka** to publish ledger events to downstream consumers.
4. **Snapshotting** if ledger growth becomes problematic.
5. **Raft-replicated event log** if a single primary becomes a bottleneck.

None of these are built in v1.

---

## AI usage disclosure

This project was implemented with **Claude Code** (Anthropic's CLI for Claude),
following an explicit plan-then-execute workflow:

1. **Plan**: Claude explored both this repository and an internal Go service
   codebase from a prior project to extract engineering conventions, then
   synthesized an implementation plan covering schema, layering, concurrency,
   idempotency, and tests. The plan was reviewed and refined against the
   ByteByteGo Digital Wallet (Ch. 28) and Payment System (Ch. 27) reference
   designs.
2. **Execute**: After plan approval, Claude implemented the solution in small
   increments: one package or test file at a time, with unit tests run after
   each layer to catch regressions early.

The verbatim transcript with model responses is preserved in Claude Code's
local session log and can be exported on request. As a fallback the major
prompts I sent during the session are listed in
[`AI_TRANSCRIPT.md`](./AI_TRANSCRIPT.md), satisfying ASSIGNMENT.md §AI usage
point 3 ("give us all the prompts that you used with the AI") even if a full
transcript export is not available at submission time.

How AI was used in practice:

- Code generation (boilerplate, types, repository methods).
- Test scaffolding (S1-S9, I1-I5, C1-C4).
- Documentation drafting (this README, comments).
- Sanity-checking the concurrency model, the idempotency contract, and the
  "PENDING is mid-transaction only" trap.

What the human author owned:

- The choice of Postgres + `FOR UPDATE` over alternatives (Saga, event
  sourcing, optimistic locking).
- The decision to mirror an internal reference codebase's
  `services/<name>/{init,route,internal/...}` layout.
- The decision to enforce `request_hash` in v1.
- All review and integration of the AI's output.
