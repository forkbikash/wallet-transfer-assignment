## Summary

Implements `POST /transfers` end-to-end with the four guarantees the
assignment calls out: idempotent request handling, double-entry ledger
recording, correct balance tracking, and safe concurrent execution. Backed
by Postgres, written in Go with a clean handler / service / repository /
domain layering. Persistence uses GORM strictly as a connection /
transaction manager + raw-SQL executor — **no ORM, no AutoMigrate, no
struct-tag scanning**; the schema is owned by the migrations.

**63 tests across 7 packages** (unit + handler + middleware + integration
+ concurrency), `golangci-lint v2.5.0` and `go vet` clean, `gofmt -l .`
empty, SonarCloud quality gate clean (with documented rule suppressions
for PL/SQL false positives on Postgres migrations and the dev compose
password).

## AI disclosure

1. **Tool**: Claude Code (Anthropic's CLI for Claude). I drove the session
   from the terminal in this repo's working directory.

2. **How I use it**: a strict **plan-then-execute** loop. First I have
   Claude explore the assignment repo and an internal Go service
   codebase from a prior project to extract conventions, then
   synthesize an implementation plan covering schema, layering,
   concurrency, idempotency, and tests.
   The plan is reviewed and refined against external references
   (ByteByteGo Ch. 27 *Payment System* and Ch. 28 *Digital Wallet*) and
   then explicitly approved before any code is written. After approval I
   have Claude implement the solution incrementally — one package or test
   file at a time, with `go test` and `go vet` after each layer to catch
   regressions early. Every review pass (security, simplify, SOLID/OOP,
   requirement audit) is its own iteration.

3. **Transcript / prompts**: see [`AI_TRANSCRIPT.md`](./AI_TRANSCRIPT.md)
   in the repo, which lists every prompt I sent during the session in
   order, grouped by phase (planning / implementation / review). The
   verbatim transcript with model responses is preserved in Claude Code's
   local session log and is available on request. The bullet list in
   `AI_TRANSCRIPT.md` satisfies the "give us all the prompts that you
   used" fallback option from `ASSIGNMENT.md` §AI usage point 3 even
   if a full transcript export is not feasible at submission time.

   Bugs that Claude itself caught on review (and how) are also documented:
   the `currency=''` placeholder regression, the `claimOrReplay` zero-
   timestamp regression, and a misleading test name that was actually
   testing the opposite case.

## Schema Design

Three tables (`migrations/0001_init.up.sql`).

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

**Constraints encode invariants directly:**

- `balance_minor >= 0` — last-line defense against double-spending; the DB
  rejects the operation even if application validation has a bug.
- `amount_minor > 0` and `from_wallet_id <> to_wallet_id` — invalid
  transfers can't be persisted at all.
- `UNIQUE (transfer_id, entry_type)` — encodes "exactly one DEBIT and one
  CREDIT per transfer" at the schema level. Inserting a third entry, or a
  duplicate DEBIT, hard-fails.
- `UNIQUE (idempotency_key)` — the dedup record itself; this is the
  uniqueness that the idempotency strategy hinges on.
- `currency CHECK (... OR currency = '')` on transfers (only) — allows the
  transient empty placeholder during the INSERT-before-wallet-lock phase.
  `wallets.currency` has the strict 3-char check.

**Indexes:** PK + UNIQUE constraints auto-create their indexes. Three are
explicit:

- `idx_ledger_wallet_created (wallet_id, created_at DESC)` — covers any
  per-wallet history scan and serves as the FK-enforcement index for
  `ledger_entries.wallet_id`.
- `idx_transfers_from_wallet`, `idx_transfers_to_wallet` — Postgres does
  NOT auto-index the *referencing* side of an FK. Without these, the FK
  enforcement check (and any "transfers from/to wallet X" lookup) would
  seq-scan the entire `transfers` table.
- `UNIQUE(transfer_id, entry_type)` doubles as a `transfer_id`-prefix
  index, so we don't need a separate one on `ledger_entries.transfer_id`.

The `idempotency_records` table suggested in `ASSIGNMENT.md` is
deliberately collapsed into `transfers.idempotency_key UNIQUE` — they are
functionally equivalent and one less table is one less moving part. The
collapse is documented as a tradeoff.

## Idempotency Strategy

The contract is `exactly-once = at-least-once (client retries) +
at-most-once (server dedup)`.

**Storage.** The `idempotencyKey` is stored in `transfers.idempotency_key`
with a `NOT NULL UNIQUE` constraint. Storage is durable across process
restarts because it's a regular Postgres column, not in-memory state.

**Detection.** Inside the transfer's transaction:

```sql
INSERT INTO transfers (..., status) VALUES (..., 'PENDING')
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING created_at, updated_at;
```

If the insert returns a row, this is a fresh transfer; if it returns no
rows, the key was already taken — fall through to the replay path.

**Returning the original result.** On conflict, run a `SELECT` for the
existing row. Postgres' **unique-index xmax lock** on the conflicting key
already blocked the second statement until the original transaction
committed or rolled back, so the SELECT reads committed state — never an
in-flight `PENDING`. The handler returns the original `PROCESSED` /
`FAILED` outcome with `replayed: true`. We prefer this Stripe-style
"block + return original" replay over `429 Too Many Requests`: the client
gets the actual outcome on the first retry rather than another retry
cycle, and the contract is identical.

**Preventing duplicate side effects.**

- The INSERT, the wallet locks, the ledger inserts, the balance updates,
  and the outcome write are **all in the same transaction**. There is no
  multi-transaction split where a partially-applied state could leak.
- Because the INSERT is part of the transfer transaction, **a rollback
  releases the unique-key claim** — replays of a rolled-back attempt
  execute fresh, with no orphan idempotency record.

**Strict body-match contract.** Each transfer row stores `request_hash`,
a SHA-256 hex digest of length-prefixed canonical fields
(`fromWalletId | toWalletId | amount`). On replay we compare the incoming
request's hash to the stored one. **Same key + different body returns
`409 IDEMPOTENCY_CONFLICT`**, not the original outcome — surfacing a
client bug instead of silently replaying a different request. Length-
prefixing prevents the `("a|b","c")` vs `("a","b|c")` delimiter-collision
class.

**Failed states are committed, not rolled back.** Insufficient funds and
currency mismatch are *business outcomes*, not system errors — the
transfer row is persisted with `status = FAILED` and the failure reason.
A retry replays the same `FAILED` rather than re-attempting the doomed
transfer.

**State machine.** `PENDING → PROCESSED | FAILED`, where `PENDING` is a
transient mid-transaction state and **never observed in committed state**
under normal operation. Every transaction either updates to a terminal
status before commit or rolls back entirely. The `CHECK` constraint
allows `PENDING` only so the initial INSERT is legal; you should expect
to see only `PROCESSED` / `FAILED` in committed state.

## Concurrency Strategy

`READ COMMITTED + SELECT ... FOR NO KEY UPDATE` with deterministic lock
ordering — pessimistic locking on the wallet rows that are about to
change.

```sql
SELECT id, balance_minor, currency, ...
FROM wallets
WHERE id = ANY($1::text[])
ORDER BY id
FOR NO KEY UPDATE
```

- A single round-trip locks both wallet rows.
- **Sorting wallet IDs lexicographically** before the SELECT is what
  guarantees lock-acquisition order is the same for any pair of concurrent
  transfers — a necessary condition for deadlock-freedom but not by itself
  sufficient (see `FOR NO KEY UPDATE` choice below).
- **`FOR NO KEY UPDATE`, not `FOR UPDATE`**, is the correct lock mode
  here. The flow is *Claim first, then lock*: `INSERT INTO transfers (...,
  from_wallet_id, to_wallet_id, ...)` runs **before** this `SELECT` in the
  same transaction. That INSERT's foreign-key check on
  `transfers.from_wallet_id` / `transfers.to_wallet_id` acquires
  **`FOR KEY SHARE`** on the referenced wallet rows. `FOR UPDATE` conflicts
  with `FOR KEY SHARE`; `FOR NO KEY UPDATE` does not. Two concurrent
  transfers touching the same wallet would each hold `FOR KEY SHARE` from
  their FK check and then queue on each other's `FOR UPDATE` — Postgres
  detects the cycle and aborts one with `40P01`. `FOR NO KEY UPDATE`
  breaks the cycle without weakening write serialization: it is mutually
  exclusive with itself, so two concurrent debits on the same wallet
  still serialize correctly. We never `UPDATE wallets.id` (the only "key"
  column), so the "no key" caveat is honest. **This bug was caught by the
  C4 mixed-concurrency test, not by review.**
- Once both rows are locked, balance validation, ledger insertion, and
  the balance update all run inside the critical section. No other
  transaction can modify those wallets until we commit or roll back.
- Balance updates use a single `UPDATE wallets SET balance_minor =
  balance_minor + CASE id WHEN ... END` — one DB round-trip for both
  wallets, not one per wallet.
- The DB `CHECK (balance_minor >= 0)` is the **last-line defense** if a
  bug let an under-funded transfer past the application gate; the
  transaction would fail and roll back rather than leave a negative
  balance.

`SERIALIZABLE` is deliberately avoided — it would require a `40001` retry
loop on serialization failure, with cost-benefit that is unfavorable for
a small, well-understood critical section that is already deadlock-free
under `FOR NO KEY UPDATE`.

**Concurrency tests** (behind the `integration` build tag) cover the
risky scenarios with real Postgres + real goroutines:

- **C1**: 50 goroutines debit one wallet whose initial balance is exactly
  `30 × amount`. The test asserts that **exactly 30 succeed**, **exactly
  20 FAIL with `INSUFFICIENT_FUNDS`**, the final balance is 0, and the
  ledger has exactly 60 rows (30 DEBIT + 30 CREDIT).
- **C2**: 100 goroutines submit the **same idempotency key**. Asserts
  exactly 1 transfer row + 2 ledger rows + 1 fresh response (201) + 99
  replay responses (200).
- **C3**: 50 goroutines transfer A→B and 50 transfer B→A — the textbook
  deadlock probe. Asserts **no `40P01` errors** and that final balances
  net to zero change.
- **C4**: 80-goroutine mixed load on the same wallet pair — 40 share one
  idempotency key (must collapse to a single transfer via the xmax lock)
  + 40 unique keys (distinct transfers). Asserts **41 transfer rows / 82
  ledger rows**, ledger zero-sum, and `balance_minor == initial + ledger
  net`. This test first reproduced the `40P01` that drove the
  `FOR UPDATE` → `FOR NO KEY UPDATE` change.

## How to Run

**Prerequisites:**

- Go 1.22+ (CI runs 1.24)
- Docker + Docker Compose
- [`golang-migrate`](https://github.com/golang-migrate/migrate/tree/master/cmd/migrate) CLI: `brew install golang-migrate`
- `psql` (for `make seed`): ships with `postgresql-client`

```bash
# bring up postgres + apply migrations + seed two wallets
make pg-up
make migrate-up
make seed   # wallet_1 (10000 paisa) and wallet_2 (0 paisa), both INR

# run the server (PORT=8080 by default)
make run
```

In another terminal:

```bash
# happy path — fresh transfer
curl -sS -X POST localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -d '{"idempotencyKey":"k1","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}'
# → 201 Created, status PROCESSED

# idempotent replay (same body)
curl -sS -X POST localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -d '{"idempotencyKey":"k1","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}'
# → 200 OK, replayed=true, identical id

# insufficient funds — committed as FAILED, replay returns same FAILED
curl -sS -X POST localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -d '{"idempotencyKey":"k2","fromWalletId":"wallet_2","toWalletId":"wallet_1","amount":99999}'
# → 422 Unprocessable, status FAILED, failureReason INSUFFICIENT_FUNDS

# same key, different body — strict body-match contract
curl -sS -X POST localhost:8080/transfers \
  -H 'Content-Type: application/json' \
  -d '{"idempotencyKey":"k1","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":999}'
# → 409 IDEMPOTENCY_CONFLICT
```

Full curl walkthrough and the response-body schemas are in `README.md`
under "API contract & side effects".

## How to Test

```bash
# unit + handler + middleware tests, with -race and coverage
make test

# integration + concurrency tests (real Postgres required)
make pg-up && make migrate-up
INTEGRATION_DATABASE_URL=postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable \
  make test-int

# lint and format check (also what CI runs)
make lint
make fmt-check
```

| Suite | Covers |
|---|---|
| **Unit** (`go test ./...`) | Money arithmetic, transfer state machine, request validation, request_hash |
| **Service** (S1–S9) | Happy path, insufficient funds, same wallet, invalid amount, currency mismatch, wallet not found, replay PROCESSED, replay FAILED, idempotency conflict |
| **Handler** (10 tests) | BadJSON, UnknownField, PayloadTooLarge, Method/Route 404/405, FailedTransfer→422, Replay→200, Fresh→201, GenericError fallback (no string leak), table-driven AppError-preserves-status over 4 typed sentinels |
| **Middleware** (4 tests) | Panic recovery → 500 with NotPanics; panic logged at ERROR with stack; X-Request-ID generated when absent; X-Request-ID propagated when supplied |
| **Integration** I1–I5 | End-to-end happy / replay / ledger zero-sum + reproducibility / FAILED + replay / 409 conflict |
| **Concurrency** C1–C4 | 50 same-wallet debits / 100 same-key requests / cross-wallet deadlock probe / mixed load |

## Tradeoffs / Assumptions

- **`request_hash` is enforced** to make idempotency strict: same key +
  different body returns `409 IDEMPOTENCY_CONFLICT` rather than silently
  replaying a different request.
- **No async worker, no outbox, no retry queue.** `PENDING` is a transient
  mid-transaction state. Process crashes mid-transaction roll back the row
  entirely — there are no zombie rows to clean up.
- **`READ COMMITTED + SELECT FOR NO KEY UPDATE`** is preferred over
  `SERIALIZABLE`: pessimistic locking on the participating wallets is the
  canonical way to avoid the read-then-write race, and it does not require
  `40001` retry handling. **`FOR NO KEY UPDATE` (not `FOR UPDATE`)** is
  required because the in-flight `INSERT INTO transfers` already holds
  `FOR KEY SHARE` on the referenced wallet rows (FK enforcement) — using
  `FOR UPDATE` would deadlock with itself across concurrent transfers.
  See "Concurrency Strategy" above for the full reasoning.
- **Stripe-style replay** (block + return original) is preferred over
  ByteByteGo Ch. 27's `429 Too Many Requests` mid-flight choice: same
  exactly-once contract, fewer client retries, and the client gets the
  actual outcome on the first retry.
- **No `wallet_updated` / `ledger_updated` per-side-effect flags** on the
  transfer row (which the ByteByteGo Payment System chapter uses to track
  partial completion across async services). Single-transaction atomicity
  makes per-side-effect tracking redundant.
- **GORM as a connection-and-transaction manager only.** Persistence uses
  `gorm.io/gorm` for pool / transaction lifecycle / `WithContext`
  propagation, but every statement in `services/transfer/internal/repo/postgres/*`
  is hand-written parameterized SQL via `tx.Raw(sql, args...).Row().Scan(...)`
  or `tx.Exec(sql, args...)`. **No GORM ORM, query-builder, AutoMigrate,
  hooks, struct-tag scanning, callbacks, or associations.** The schema is
  owned by the migrations, not by reflection over Go structs. We set
  `gorm.Config{TranslateError: false, SkipDefaultTransaction: true,
  Logger: Silent}` so driver errors (SQLSTATE `23503` / `23514` for FK /
  CHECK violations) propagate verbatim and the manual `txManager` is the
  single source of transactional truth.
- **Amount is decoded as a JSON number** per the assignment example. A
  production-grade API would decode it as a string to avoid client-side
  float-precision risk (e.g., a JS client losing precision past
  `2^53 - 1`). Documented in the README.
- **Single currency per transfer**: source and destination wallets must
  share a currency; otherwise the transfer is committed as `FAILED` with
  reason `CURRENCY_MISMATCH`. We do not perform foreign-exchange
  conversion.
- **Integration tests share the dev Postgres instance.**
  `INTEGRATION_DATABASE_URL` defaults to `DATABASE_URL` (see Makefile);
  i.e. `make test-int` runs against the same DB used by `make run` /
  `make seed` and `TRUNCATE`s the tables on setup. A production-grade
  setup would point this at a dedicated test DB or testcontainers; we
  intentionally don't, to keep the local-run story to a single Postgres.
  Override `INTEGRATION_DATABASE_URL` to use a separate database.
- **Optional enhancements deliberately deferred** (each accounted for in
  the README's "Optional enhancements (ASSIGNMENT.md) — disposition"
  table): wallet-balance API, transfer-history API, Prometheus metrics.
  Observability (structured logging with X-Request-ID correlation) and
  retry-safe workflows (idempotency at the API layer) are **built**.

## Checklist

- [x] Tests pass (`make test` — 63 tests across 7 packages, `-race` clean)
- [x] Lint passes (`make lint` — `golangci-lint v2.5.0` reports `0 issues`, both unit and `--build-tags=integration`)
- [x] Format check passes (`make fmt-check` — `gofmt -l .` empty)
- [x] README or notes updated (full design notes in `README.md`; AI prompts in `AI_TRANSCRIPT.md`)
- [x] PR description explains schema, idempotency, and concurrency


<!-- This is an auto-generated comment: release notes by coderabbit.ai -->

## Summary by CodeRabbit

## Release Notes

* **New Features**
  * Added wallet-to-wallet transfer API endpoint with idempotency and concurrency support
  * Implemented health check endpoint for service monitoring
  * Added request ID tracking for improved observability

* **Infrastructure**
  * Added Docker containerization support for deployment
  * Configured PostgreSQL database with migrations and seed data
  * Set up CI/CD pipeline with automated testing and linting

* **Documentation**
  * Comprehensive README with API specifications and operational instructions
  * Environment configuration template for local development

* **Tests**
  * Added integration tests covering idempotency, concurrency, and ledger consistency
  * Included test suite for concurrent wallet operations

<!-- end of auto-generated comment: release notes by coderabbit.ai -->