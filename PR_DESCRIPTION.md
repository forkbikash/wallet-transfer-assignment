## Summary

Plan to build `POST /transfers` on Postgres + Go with the four guarantees from `ASSIGNMENT.md`: idempotent request handling, double-entry ledger, correct balance tracking, and safe concurrent execution. Layering: handler → service → repository → domain.

## AI disclosure

1. **Tool:** Claude Code (Anthropic's CLI for Claude).
2. **How I plan to use it:** strict plan-then-execute. I will have Claude explore the assignment repo and a prior Go service for conventions, draft a plan covering schema / layering / concurrency / idempotency / tests, refine it against my own judgment, then implement one package or test file at a time with `go test` and `go vet` after each layer. Each review pass (security, simplify, SOLID, requirement audit) will be its own iteration.
3. **Transcript:** every prompt will be captured in `AI_TRANSCRIPT.md`, grouped by phase (planning / implementation / review).

## Schema Design (planned)

Three tables will live in migrations:

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

## Idempotency Strategy (planned)

Inside the transfer transaction:

```sql
INSERT INTO transfers (..., status) VALUES (..., 'PENDING')
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING ...;
```

If the insert returns a row → fresh transfer. If not → the unique-index xmax lock will block the follow-up SELECT until the original tx commits, after which we will read the committed outcome and return `replayed: true` ("block + return original", preferred over a mid-flight `429 Try Again`).

All side effects (transfer row, wallet locks, ledger inserts, balance update, outcome write) will live in **one transaction** — a rollback will release the unique-key claim, leaving no orphan idempotency rows. A same key + different `request_hash` will return `409 IDEMPOTENCY_CONFLICT`, so a buggy client surfaces instead of silently replaying a different request. Insufficient funds / currency mismatch will be committed as `FAILED` so retries replay the failure rather than re-attempting a doomed transfer.

## Concurrency Strategy (planned)

`READ COMMITTED + SELECT ... FOR NO KEY UPDATE`, with wallet IDs sorted lexicographically before the lock — this will guarantee deadlock-freedom, since any two transfers touching the same pair will acquire locks in the same order. `FOR NO KEY UPDATE` (not `FOR UPDATE`) will be the correct mode here: the preceding `INSERT INTO transfers` FK check will already hold `FOR KEY SHARE` on both wallet rows in the same tx, and `FOR UPDATE` would conflict with `FOR KEY SHARE` and deadlock on `40P01` under concurrency; `FOR NO KEY UPDATE` is mutually exclusive with itself (so concurrent debits will still serialize) without conflicting with `FOR KEY SHARE`. A single round-trip will lock both rows, and the balance update will be one `UPDATE wallets SET balance_minor = balance_minor + CASE id WHEN … END`. `SERIALIZABLE` will be deliberately avoided to skip the `40001` retry loop on a critical section that is already deadlock-free.

## Concurrency Tests (planned)

Concurrency tests (behind the `integration` build tag) will cover the risky scenarios with real Postgres + real goroutines:

- **C1**: 50 goroutines debit one wallet whose initial balance is exactly `30 × amount`. The test will assert that **exactly 30 succeed**, **exactly 20 FAIL with `INSUFFICIENT_FUNDS`**, the final balance is 0, and the ledger has exactly 60 rows (30 DEBIT + 30 CREDIT).
- **C2**: 100 goroutines submit the **same idempotency key**. Will assert exactly 1 transfer row + 2 ledger rows + 1 fresh response (201) + 99 replay responses (200).
- **C3**: 50 goroutines transfer A→B and 50 transfer B→A — the textbook deadlock probe. Will assert **no `40P01` errors** and that final balances net to zero change.
- **C4**: 80-goroutine mixed load on the same wallet pair — 40 share one idempotency key (must collapse to a single transfer via the xmax lock) + 40 unique keys (distinct transfers). Will assert **41 transfer rows / 82 ledger rows**, ledger zero-sum, and `balance_minor == initial + ledger net`.

## How to Run (planned)

```bash
make pg-up && make migrate-up && make seed
make run     # PORT=8080
```

A `curl` walkthrough for happy path / replay / insufficient funds / 409 conflict will land in `README.md`.

## How to Test (planned)

```bash
make test                         # unit + handler + middleware, -race
make pg-up && make migrate-up
INTEGRATION_DATABASE_URL=postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable make test-int
make lint && make fmt-check
```

## Tradeoffs / Assumptions

### Design

- `request_hash` will be enforced (strict body match) so that `409` surfaces client bugs instead of silently replaying a different request.
- No async worker / outbox / retry queue — single-transaction atomicity will make per-side-effect tracking redundant, and `PENDING` will be transient and never observed in committed state.
- The optional `idempotency_records` table from `ASSIGNMENT.md` will be collapsed into `transfers.idempotency_key UNIQUE`. The two are functionally equivalent for our flow, and one less table is one less moving part.
- No per-side-effect flags (`ledger_updated`, `wallet_updated`) on the transfer row. Single-transaction atomicity will make tracking partial-completion redundant — the row either commits with both side effects or rolls back entirely.
- `wallets.balance_minor` will be a denormalized read view over the append-only `ledger_entries` log (CQRS-lite). The ledger will remain the audit trail; the integration test will verify that `SUM(ledger)` per wallet equals the stored balance.
- A single Postgres node with one ACID transaction will be used in place of any distributed-transaction protocol (TC/C, Saga, 2PC). The assignment is a single-node service, so the additional machinery would be cost without benefit.
- `READ COMMITTED + SELECT … FOR NO KEY UPDATE` will be preferred over `SERIALIZABLE`: the critical section is already deadlock-free under deterministic lock ordering, and skipping `SERIALIZABLE` avoids the `40001` retry loop.
- JSON `amount` will be decoded as a number per the assignment example; a production-grade API would prefer a string to avoid JS float-precision risk past `2^53 - 1`.
- Single currency per transfer; a mismatch will be committed as `FAILED` with reason `CURRENCY_MISMATCH` (no FX).

### Performance, scalability & operability ceilings (deliberately accepted in v1)

- **Hot-wallet throughput** will be bounded by `1 / tx_commit_time`: every debit on a given wallet will serialize behind `FOR NO KEY UPDATE` on that row. A wallet receiving thousands of writes/sec will not scale linearly.
- **Write-side scalability** will be vertical only. A single Postgres primary handles all writes. Horizontal write scaling would require partitioning by wallet ID, which would also break the single-DB transaction for cross-shard transfers and force TC/C or Saga at the service layer.
- **Read-side scalability** will also be primary-bound. No read replicas in v1 — every balance read goes to the primary. This avoids the stale-read race where a lagging replica lets an under-funded transfer past authorization, but it caps read throughput.
- **Availability** — a single Postgres primary will be a SPOF. Production would add streaming replication + automated failover (RDS Multi-AZ); both are operator concerns and out of v1 scope.
- Optional enhancements likely to be deferred: balance API, transfer-history API, Prometheus metrics. Structured logging + X-Request-ID correlation will be built.

## Checklist (to satisfy on the implementation PR)

- [ ] Tests pass (`make test`, `-race` clean)
- [ ] Lint passes (`make lint` — `golangci-lint v2.5.0`)
- [ ] Format check passes (`make fmt-check`)
- [ ] README and `AI_TRANSCRIPT.md` updated
- [ ] PR description explains schema, idempotency, and concurrency
