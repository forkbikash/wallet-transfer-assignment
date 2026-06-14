# Postgres Locks, MVCC & Hot-Wallet Throughput — Q&A Notes

Questions asked while reviewing the wallet-transfer design (idempotency via
`INSERT ... ON CONFLICT`, concurrency via `FOR NO KEY UPDATE`).

---

## Q1. What is the "xmax lock" in the idempotency strategy?

- Shorthand for Postgres's **wait-on-transaction-ID** mechanism, discovered
  through the tuple header.
- Every row carries hidden txid stamps. A blocked session reads the txid of the
  in-progress transaction from the tuple and takes a `ShareLock` on that
  **transaction ID** (`XactLockTableWait`) — i.e. "sleep until that transaction
  commits or aborts".
- In the idempotency flow: request B's duplicate `INSERT` hits the unique index
  on `idempotency_key`, finds A's **uncommitted** row, and blocks on A's txid.
  - A commits → B's insert returns 0 rows → follow-up `SELECT` reads A's
    committed final outcome (`PROCESSED`/`FAILED`) → return `replayed: true`.
  - A rolls back → conflict evaporates → B's own insert succeeds → B becomes
    the fresh transfer. No orphaned idempotency claims.
- Visible in `pg_locks` during the race:
  `locktype = transactionid, mode = ShareLock, granted = false`.

## Q2. What does "hot-wallet throughput bounded by 1 / tx_commit_time" mean?

- Every transfer touching wallet W holds `FOR NO KEY UPDATE` on W's row from
  lock acquisition **until commit**. The mode conflicts with itself, so all
  transfers on W execute strictly one at a time — the row lock is a mutex.
- Hold time ≈ balance UPDATE + ledger INSERTs + status update + **commit
  (WAL fsync)** — commit usually dominates.
- Ceiling: `max transfers/sec on one wallet ≈ 1 / tx_commit_time`
  (e.g. ~5 ms → ~200/sec).
- More goroutines / app servers / CPU **do not help** — they only deepen the
  queue. Throughput stays flat; per-request latency grows with queue depth.
- Transfers across disjoint wallet pairs are unaffected (fully parallel).
- Mitigations (all deferred in v1): shorter critical section,
  `synchronous_commit = off` (loses durability — unacceptable for money),
  sub-sharding the hot wallet into N sub-balance rows, async/escrow debits.

## Q3. What lock does `INSERT ... ON CONFLICT (key) DO NOTHING RETURNING` take?

- **Not** any of the `FOR UPDATE` family — those lock *existing* rows.
- Locks actually involved:
  1. `ROW EXCLUSIVE` on the **table** (every INSERT; conflicts only with DDL).
  2. **Implicit ownership** of the new tuple — `xmin` = inserter's txid; the
     row is invisible to others until commit. Insert-vs-insert races are
     handled by *speculative insertion* (insert optimistically, check unique
     index, confirm or super-delete).
  3. On conflict with an **in-progress** insert: block via `ShareLock` on the
     other **transaction ID** (the Q1 mechanism).
- `DO NOTHING` vs `DO UPDATE`:
  - `DO UPDATE` takes a real exclusive **tuple lock** on the conflicting
    committed row (≈ `FOR UPDATE`) before updating.
  - `DO NOTHING` takes **no lock** on the conflicting row — it just confirms a
    committed duplicate exists and skips.
- Safe in our flow because: the INSERT only returns 0 rows *after* the winner
  committed, and the winner wrote the transfer's **final** status in the same
  transaction — the row is effectively immutable by the time anyone reads it.

## Q4. What is the difference between `xmin` and `xmax`?

- Hidden MVCC system columns on every tuple — the row version's lifespan stamp.
- **`xmin`** = txid of the transaction that **inserted** this row version.
  Set once, never changes. Row becomes visible when that txid commits.
- **`xmax`** = txid of the transaction that **deleted/updated** this version
  (0 = live). Row stops being visible once that txid commits.
- `xmax` is overloaded: **row locks** (`FOR UPDATE` / `FOR NO KEY UPDATE` /
  `FOR SHARE` / `FOR KEY SHARE`) store the locker's txid in `xmax` with flag
  bits meaning "lock, not deletion".
- `UPDATE` never modifies in place — it stamps the old version's `xmax` and
  inserts a new version whose `xmin` is the same txid:

  ```
  old version:  xmin = 500   xmax = 731   (updater)
  new version:  xmin = 731   xmax = 0     (live)
  ```

- Inspect live: `SELECT xmin, xmax, id, balance_minor FROM wallets;` while
  another session holds a row lock — the locker's txid shows in `xmax`.
- Consequences: readers never block writers (old version stays readable);
  hot rows accumulate dead versions (autovacuum/HOT matter); 32-bit txids are
  why anti-wraparound freezing exists.

## Q5. What are `FOR UPDATE`, `FOR NO KEY UPDATE`, `FOR SHARE`, `FOR KEY SHARE`?

- The four explicit **row-level lock modes** of `SELECT ... FOR <mode>`,
  strongest → weakest. All are stored via `xmax` and held until transaction end.
- **`FOR UPDATE`** — "I may change/delete the row, including key columns."
  Conflicts with all four modes.
- **`FOR NO KEY UPDATE`** — "I'll modify the row but not its keys." Conflicts
  with itself, `FOR SHARE`, `FOR UPDATE`; **not** with `FOR KEY SHARE`. Also
  what a plain `UPDATE` (non-key columns) takes implicitly.
- **`FOR SHARE`** — "I read this row and depend on it not changing; freeze
  **all columns** until I commit." Blocks every writer (`UPDATE`, `DELETE`,
  `FOR UPDATE`, `FOR NO KEY UPDATE`) but coexists with other readers holding
  `FOR SHARE`/`FOR KEY SHARE` — a reader-writer lock: readers share, writers
  queue. Fixes the check-then-act race that a plain MVCC `SELECT` allows
  (snapshot reads don't stop the row changing before your next statement).
  Unlike `FOR KEY SHARE`, which only pins the key (non-key updates pass
  through), `FOR SHARE` pins the whole row. Unused in this design — we always
  lock wallets intending to write, so `FOR NO KEY UPDATE` is the right mode.
- **`FOR KEY SHARE`** — weakest; "don't delete the row or change its key."
  Only conflicts with `FOR UPDATE`. Taken automatically by **FK checks** on
  the referenced (parent) rows.

  | holds \ wants | KEY SHARE | SHARE | NO KEY UPDATE | UPDATE |
  |---------------|-----------|-------|---------------|--------|
  | KEY SHARE     | ✓         | ✓     | ✓             | ✗      |
  | SHARE         | ✓         | ✓     | ✗             | ✗      |
  | NO KEY UPDATE | ✓         | ✗     | ✗             | ✗      |
  | UPDATE        | ✗         | ✗     | ✗             | ✗      |

- Why the design uses `FOR NO KEY UPDATE`, not `FOR UPDATE` — the **lock
  upgrade deadlock**:
  1. Inserting the `transfers` row first puts FK `FOR KEY SHARE` locks on both
     wallet rows — automatically, before the explicit lock statement runs.
  2. `FOR KEY SHARE` doesn't conflict with itself, so **two concurrent
     transactions both get past their INSERT** and each holds a weak shared
     lock on the same wallet row.
  3. With `FOR UPDATE` the timeline deadlocks (T1: A→B, T2: A→C — one shared
     wallet is enough):

     | Step | T1                                  | T2                                  |
     |------|-------------------------------------|-------------------------------------|
     | 1    | INSERT → KEY SHARE on A, B          |                                     |
     | 2    |                                     | INSERT → KEY SHARE on A, C ✓ (shared) |
     | 3    | FOR UPDATE on A → blocked by T2's KEY SHARE |                             |
     | 4    |                                     | FOR UPDATE on A → blocked by T1's KEY SHARE |
     | 5    | T1 waits for T2 ⟲ T2 waits for T1 → `40P01`, one tx killed |              |

     Your own KEY SHARE doesn't block your upgrade — the **other**
     transaction's does. Neither can proceed until the other commits; cycle.
  4. Sorted lock ordering does **not** prevent this — both lock A first and
     still deadlock. The cycle is FK-locks-vs-upgrades, not explicit locks
     racing in opposite orders.
  5. `FOR NO KEY UPDATE` doesn't conflict with `FOR KEY SHARE`, so step 3 is
     granted immediately; the only remaining conflict is NO-KEY-UPDATE vs
     itself → a simple queue, made cycle-free by sorted wallet IDs. Two
     mechanisms, two jobs: the lock mode kills the FK-upgrade cycle, the
     ordering kills the lock-both-rows cycle. Semantically honest too: we
     update `balance_minor`, never `id`.

---

Related: [postgres-vs-dynamodb-isolation-levels.md](postgres-vs-dynamodb-isolation-levels.md)
