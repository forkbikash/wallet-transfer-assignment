# AI Usage — Session Prompts

Per `ASSIGNMENT.md` §AI usage:

> A transcript of your entire session with your AI tool of choice. You can add
> this to the repo or email it to us with your submission. If for some reason,
> this is not possible, give us all the prompts that you used with the AI.

The tool used was **Claude Code** (Anthropic's CLI for Claude). The full
verbatim transcript with model responses is preserved in Claude Code's local
session log and can be exported on request. As a fallback this file lists the
major prompts I sent during the session so the work is reproducible from the
prompts alone.

## Workflow

I drove the session in a strict **plan-then-execute** loop:

1. Asked Claude to *explore* the assignment repo and an internal Go service
   codebase from a prior project (used as a reference for engineering
   conventions) before writing any code.
2. Asked Claude to *plan* the implementation, refined the plan against
   reference designs (ByteByteGo Ch. 27 Payment System, Ch. 28 Digital
   Wallet), then explicitly approved the plan via `ExitPlanMode`.
3. Asked Claude to *implement* the solution incrementally, with `go test`
   and `go vet` after each layer.
4. Iterated through review passes (security, simplify, SOLID/OOP audit,
   requirement audit) and applied focused fixes between each pass.

## Prompts (in order)

### Phase 1 — planning

1. *"go through the wallet-transfer-assignment code repository and plan the
   solution. Follow the [path to an internal Go service codebase, redacted]
   for practices to be followed. Always follow good practices like SOLID
   design principles, object-oriented design and design patterns."*

2. *"look at the following wallet system design and let's improve the plan if
   needed."* (followed by the ByteByteGo Ch. 28 *Digital Wallet* chapter
   pasted as context)

3. *"now go through the following system design and improve our design if
   relevant:"* (followed by the ByteByteGo Ch. 27 *Payment System* chapter)

4. *"do these as well and update the plan: request_hash for v1"* — switching
   `request_hash` from a documented tradeoff into v1 scope.

5. (Plan-mode answer to two clarifying questions:)
   - **Database:** PostgreSQL (recommended)
   - **Layout:** Mirror the internal reference codebase's
     `services/<name>/{init,route,internal/...}` layout

6. Approved the plan via `ExitPlanMode`.

### Phase 2 — implementation

7. (After plan approval) Claude implemented the full solution from scratch:
   `go.mod`, migrations, `common/util/money`, `common/error`, domain models,
   request/response DTOs, repo interfaces + Postgres implementations,
   service + service tests, HTTP middleware, handler, route registration,
   config + infra, `cmd/server/main.go`, integration & concurrency tests,
   Dockerfile, docker-compose, Makefile, `.golangci.yml`, README.

### Phase 3 — review and refinement

8. *"are we missing anything?"* — caught and fixed the `currency=''`
   placeholder bug (the initial `INSERT` stored an empty currency that was
   never updated; idempotent replays returned the wrong currency).

9. *"assignment code repository has review prompts in different files.
   review the code according to those prompts"* — self-review against
   `evaluation_guide.md` + `.github/copilot-instructions.md`.

10. *"do 1-3 and 5"* — applied four review-driven cleanups: wired the unused
    logger, strengthened the I3 invariant test for canonical-currency,
    forbade `t.Parallel()` in integration tests, and added JSON 404/405
    handlers.

11. *"fix these: currency CHAR(3) pads with spaces; Hash on DTO"* — switched
    the schema to `VARCHAR(3)` and moved `hashRequest` from the request DTO
    into the service package.

12. `/security-review` — ran the bundled multi-agent security review against
    the diff. No HIGH/MEDIUM findings; verified safe.

13. `/simplify` — ran the bundled three-agent code-quality / reuse /
    efficiency review. Applied the high-leverage fixes: collapsed two
    `UpdateBalance` round-trips into one `UPDATE … CASE WHEN`, replaced
    `fmt.Sscanf` with `strconv.Atoi`, deduplicated SQLSTATE helpers, removed
    a misleading comment, simplified `claimOrReplay`'s return signature,
    unified the integration tests' `post`/`callTransfer` helpers.

14. *"are we following good practices like SOLID design principles, object
    oriented design and design patterns"* — line-by-line audit; confirmed
    SRP/OCP/LSP/ISP/DIP and listed the patterns in use (Repository, Unit of
    Work, Adapter, Decorator, State, Value Object, …). Two soft gaps
    identified.

15. *"fix highest-leverage a and b"* — extracted `Healthz` into a
    service-agnostic `common/health` package; lifted `pickWallets` onto a
    typed `model.Wallets` slice with `.Pick(fromID, toID)`.

16. *"now check all other requirements in assignment and fulfil them. don't
    miss any requirements"* — migrated `.golangci.yml` from v1 to v2 schema,
    bumped CI's pinned `golangci-lint` to v2.5.0, fixed nine new lint
    findings, added the README's missing **API contract & side effects** and
    **Observability** sections.

17. *"anything we are missing. go through all the requirements"* — added
    `bin/` to `.gitignore`, included `-cover` in `make test`, added a
    `coverage` Make target, wrote handler-level transport tests
    (`BadJSON`, `UnknownField`, `PayloadTooLarge`, `MethodNotAllowed`,
    `NotFound`, `FreshSuccess`, `Replay`, `FailedTransfer`, `GenericError`,
    `AppError-preserves-status`), and added a `health` package test.

18. *"why … can't be done. can we not just add these in place instead of
    from env?"* — hardcoded `make lint` / `make fmt-check` / `make test`
    directly into `.github/workflows/ci.yml` so the candidate no longer has
    to set repo variables.

19. *"anything we are missing. go through all the requirements"* (again) —
    added an explicit **"Optional enhancements (ASSIGNMENT.md) — disposition"**
    section to the README so a reviewer can see at a glance which of the five
    optional items were considered, which were built, and why each one was
    skipped.

20. *"anything we are missing. go through all the requirements"* (this
    response) — created this `AI_TRANSCRIPT.md` to satisfy the AI-disclosure
    requirement explicitly via the assignment's "list all prompts" fallback.

21. *"anything we are missing. go through all the requirements"* —
    added the four operational-concerns tests in
    `common/middleware/middleware_test.go` (panic recovery, panic logging,
    request-id generation, request-id propagation) so ASSIGNMENT.md's
    documentation-first-workflow step 5 ("verify observability and
    operational concerns") is asserted, not just documented. Also added
    concrete success / failed / error response-body examples to the README's
    API contract section so a reviewer doesn't have to infer the JSON shape
    from the Go struct.

22. *"anything we are missing. go through all the requirements"* —
    verified there are no `TODO` / `FIXME` markers in the code, confirmed
    every "not built" / "not implemented" string is part of the deliberate
    Optional-Enhancements disposition section, and updated this transcript
    to keep the prompt list synchronized through the most recent rounds.

23. *"are we missing any other db unique constraint and indices?"* —
    audited each FK column against the existing indices, found that
    `transfers.from_wallet_id` and `transfers.to_wallet_id` were
    unindexed (Postgres does NOT auto-index the *referencing* side of a FK).
    Added `idx_transfers_from_wallet` and `idx_transfers_to_wallet` to the
    migration; updated the down migration; documented the new indexes in
    the README's schema section.

24. *"have we followed [the Testing Requirements / Red-Blue-Green
    discipline]?"* — gave an honest accounting: the four explicit test
    categories are thoroughly covered, but the workflow that produced the
    tests was incremental-with-tests, not test-first.

25. *"let's follow it"* — demonstrated genuine Red / Blue / Green for one
    new behavior:

    **Red.** Added `TestWriteError_IncludesRequestIDInBody` asserting that
    error response bodies must include a `request_id` field so a client
    can quote the JSON in a bug report. Ran the test:

    ```text
    --- FAIL: TestWriteError_IncludesRequestIDInBody (0.00s)
        Error: Not equal: expected: string("trace-abc-123") actual: <nil>
    ```

    **Blue/Green.** Added a `RequestID` field to `errorBody`, populated
    it from `RequestIDFromContext` in `WriteError`, `NotFoundHandler`,
    and `MethodNotAllowedHandler`. Ran the same test:

    ```text
    ok    common/middleware    0.556s
    ```

    **Refactor.** Hoisted the `RequestIDFromContext` lookup out of the
    `if ae.HTTPStatus >= 500` branch in `WriteError` so both the log line
    and the response body draw from the same value. Updated the README's
    API-contract error-response example to match the new shape.

    Full regression sweep after the change: 7 packages green, 0 lint
    issues, gofmt clean.

26. *"use gorm for postgres but keep the raw sql instead of gorm orm form
    query"* — swapped the persistence layer from `database/sql` + `pgx`
    stdlib driver to `gorm.io/gorm` + `gorm.io/driver/postgres`, while
    keeping every statement as hand-written parameterized SQL. This is
    "GORM as connection / transaction manager + raw-SQL executor", not GORM
    ORM. Refactor mode (no new behavior; tests stay green throughout).

    Concrete moves:
    - `config/infra/postgres.go` — `*sql.DB` → `*gorm.DB`; configure pool via
      `db.DB().Set{MaxOpenConns,MaxIdleConns,ConnMaxLifetime}`; ping via the
      same underlying `*sql.DB`. `gorm.Config{Logger: Silent,
      SkipDefaultTransaction: true, TranslateError: false}` so the driver's
      pgx errors (SQLSTATE) propagate verbatim and the manual `txManager`
      remains the single source of transactional truth.
    - `repo/postgres/querier.go` — context key now stores `*gorm.DB`
      instead of `*sql.Tx`; `mustTxQuerier` returns `tx.WithContext(ctx)`.
    - `repo/postgres/tx_manager.go` — `db.WithContext(ctx).Transaction(fn,
      &sql.TxOptions{Isolation: ReadCommitted})`; GORM's Transaction handles
      panic-rollback for us.
    - `repo/postgres/{wallet,transfer,ledger}_repo.go` — every
      `q.ExecContext(...)` → `q.Exec(sql, args...)` (returns `*gorm.DB` with
      `.Error` and `.RowsAffected`); every `q.QueryRowContext(...).Scan(...)`
      → `q.Raw(sql, args...).Row().Scan(...)`; every `q.QueryContext(...)` →
      `q.Raw(sql, args...).Rows()`. The `INSERT ... ON CONFLICT DO NOTHING
      RETURNING` and `SELECT ... FOR UPDATE` patterns are unchanged.
    - `common/health/health.go` — Ping pulled from `db.DB()`.
    - `services/transfer/init/transfer_init.go` — `Config.DB` typed as
      `*gorm.DB`.
    - `tests/integration/helpers.go` — env now carries both `*gorm.DB` (for
      `transferinit.Config`) and the underlying `*sql.DB` (for
      golang-migrate and the raw-SQL test helpers).

    Verification: 53 tests across 7 packages green with `-race`,
    `golangci-lint run` and `--build-tags=integration` both `0 issues`,
    `go vet` clean both tags, `gofmt -l .` empty.

## Status at end of session

- 52 unit + handler + middleware tests across 7 packages, all green with `-race`
- 9 integration + concurrency tests behind the `integration` build tag; build
  cleanly and skip when `INTEGRATION_DATABASE_URL` is unset
- `golangci-lint run ./...` and `--build-tags=integration ./...` both report
  `0 issues`
- `go vet` clean for both build tags
- `gofmt -l .` empty
- README covers Architecture, Schema, Idempotency strategy, Concurrency
  strategy, State machine, API contract & side effects (with response body
  examples), Observability, How to run (with prerequisites), How to test,
  Tradeoffs, Optional-enhancement disposition, Design philosophy, Scaling
  path, AI usage disclosure

## What I judged the human owned

- The choice of Postgres + `FOR UPDATE` over Saga / event sourcing /
  optimistic locking.
- The decision to mirror an internal reference codebase's
  `services/<name>/{init,route,internal/...}` layout rather than the simpler
  standard-Go `internal/{...}` layout.
- The decision to enforce `request_hash` in v1 rather than defer it.
- The decision to keep `sonar-project.properties` as the assignment template
  shipped it (after I had auto-rewritten it Go-aware, the user reverted).
- All review-and-integrate decisions, including which review findings to act
  on and which to deliberately skip.

## What I judged Claude Code did well

- Plan-then-execute discipline — every non-trivial change was specified in
  the plan file before code was written.
- Incremental verification: `go test` + `go vet` after every layer.
- Catching its own bug (`currency=''` placeholder) on the *follow-up* audit
  rather than at write time. The fix included a strengthened test (I3 now
  asserts persisted currency on every transfer row) so the regression
  cannot recur.

## What did not go well

- Initial schema used `CHAR(3)` for currency (space-padded). Surfaced as a
  paper cut later; switched to `VARCHAR(3) CHECK (char_length = 3)`.
- The `Hash()` function originally lived on the request DTO. A strict reading
  of "service owns identity decisions" pushed it into the service package.
- The first `claimOrReplay` signature returned a value-typed transfer *or* a
  pointer-typed replay; later collapsed to `(*Transfer, bool, error)`.
- The first `WalletRepoIface` exposed per-wallet `UpdateBalance`; collapsed
  into `ApplyBalanceDeltas([]BalanceDelta)` which removes one DB round-trip
  per successful transfer.
