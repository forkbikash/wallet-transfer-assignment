# Isolation Levels: PostgreSQL vs DynamoDB

**Isolation** is the "I" in ACID: when multiple transactions run concurrently, how much of each other's work can they see? Stronger isolation means fewer concurrency anomalies but more aborts/retries or blocking; weaker isolation means better throughput but the application must reason about races.

PostgreSQL and DynamoDB approach this from opposite ends:

- **PostgreSQL** is a classic MVCC relational database with tunable, per-transaction isolation levels (`READ COMMITTED` → `REPEATABLE READ` → `SERIALIZABLE`).
- **DynamoDB** has no tunable isolation level. It gives you atomic single-item operations, a read-consistency knob (eventual vs strong), conditional writes for optimistic locking, and serializable multi-item transactions via `TransactWriteItems` / `TransactGetItems`.

---

## 1. The Anomalies Isolation Levels Exist to Prevent

The SQL standard defines isolation levels by which of these phenomena they forbid:

| Anomaly | What happens |
|---|---|
| **Dirty read** | T1 reads data written by T2 *before T2 commits*. If T2 rolls back, T1 acted on data that never existed. |
| **Non-repeatable read** | T1 reads a row, T2 updates+commits it, T1 reads the same row again and sees a different value. |
| **Phantom read** | T1 runs a query (`WHERE balance > 100`), T2 inserts a matching row and commits, T1 re-runs the query and sees a new "phantom" row. |
| **Lost update** | T1 and T2 both read `balance = 100`, both compute `+10`, both write `110`. One increment is silently lost. |
| **Write skew** | T1 and T2 both read the same data, each makes a decision based on it, and each writes to a *different* row. Because they touch different rows, the database sees no conflict and both commit — but the combined result breaks a rule that held when each transaction checked it. See the walkthrough below. |

**Write skew, step by step.** A hospital has a rule: *at least one doctor must be on call*. Right now Alice and Bob are both on call, and both feel sick at the same time:

```
Invariant: COUNT(on_call = true) >= 1          Start: Alice ✔ on call, Bob ✔ on call

T1 (Alice)                                T2 (Bob)
──────────────────────────────            ──────────────────────────────
SELECT COUNT(*) WHERE on_call             SELECT COUNT(*) WHERE on_call
  → sees 2  "Bob's covering, I             → sees 2  "Alice's covering, I
     can leave"                               can leave"
UPDATE doctors SET on_call=false          UPDATE doctors SET on_call=false
  WHERE name='Alice'                        WHERE name='Bob'
COMMIT ✔                                  COMMIT ✔

Result: 0 doctors on call. Invariant broken.
```

Each transaction's check was correct *against the data it read*, and the two transactions wrote to **different rows** (Alice's row vs Bob's row) — so first-committer-wins update detection never fires and both commits succeed. The anomaly lives in the *combination*: each decision was premised on data the other transaction was about to change. This is the one anomaly that snapshot isolation (Postgres REPEATABLE READ) cannot catch; only SERIALIZABLE does.

The standard's level ladder:

| Level | Dirty read | Non-repeatable read | Phantom | Write skew |
|---|---|---|---|---|
| READ UNCOMMITTED | possible¹ | possible | possible | possible |
| READ COMMITTED | ✗ | possible | possible | possible |
| REPEATABLE READ | ✗ | ✗ | possible² | possible |
| SERIALIZABLE | ✗ | ✗ | ✗ | ✗ |

¹ Never possible in PostgreSQL — see below.
² Not possible in PostgreSQL's REPEATABLE READ — its implementation (snapshot isolation) is stronger than the standard requires.

---

## 2. PostgreSQL

### 2.1 MVCC: the machinery underneath

PostgreSQL never lets a reader see uncommitted data and never blocks readers with writers. It does this with **MVCC (Multi-Version Concurrency Control)**: every `UPDATE` writes a *new version* of the row (tagged with the creating transaction's `xmin`) and marks the old one as expired (`xmax`). Each transaction works against a **snapshot** — the set of transaction IDs that had committed when the snapshot was taken — and only sees row versions visible in that snapshot.

```
 row versions of account 42:            transaction snapshots:

 v1 (xmin=100, xmax=205)  bal=500       T_a (snapshot taken before 205
 v2 (xmin=205, xmax= - )  bal=400           committed) ──► sees v1: 500

                                        T_b (snapshot taken after 205
                                            committed) ──► sees v2: 400
```

The isolation levels differ only in **when the snapshot is taken** and **what extra conflict checking happens at write/commit time**.

### 2.2 READ COMMITTED (the default)

- A **new snapshot per statement**. Each statement sees everything committed before *that statement* began.
- `READ UNCOMMITTED` exists syntactically but behaves identically — Postgres simply cannot do dirty reads.
- An `UPDATE`/`DELETE` that hits a row currently being modified by another transaction **waits** for it; if the other transaction commits, Postgres re-evaluates the `WHERE` clause against the *new* row version and proceeds (the "EvalPlanQual" recheck).

Consequences:

- Two statements in one transaction can see different data (non-repeatable reads, phantoms).
- **Lost updates are possible** with read-modify-write done in application code:

```sql
-- BOTH transactions read 100, both write 90: one debit is lost.
SELECT balance FROM accounts WHERE id = 42;   -- app computes 100 - 10
UPDATE accounts SET balance = 90 WHERE id = 42;
```

- But a **single atomic UPDATE is safe**, because the second writer waits and then sees the committed new version:

```sql
UPDATE accounts SET balance = balance - 10 WHERE id = 42;  -- no lost update
```

- For explicit read-then-decide logic, take a row lock: `SELECT ... FOR UPDATE` (or `FOR NO KEY UPDATE`). This serializes access to that row.

### 2.3 REPEATABLE READ (actually Snapshot Isolation)

- **One snapshot for the whole transaction**, taken at the first query. Every statement sees the same frozen view of the database.
- No non-repeatable reads, and — beyond what the standard requires — **no phantoms** either.
- **First-committer-wins** write conflict detection: if you try to update a row that another transaction modified and committed *after your snapshot*, your transaction aborts:

```
ERROR:  could not serialize access due to concurrent update
```

  The application must catch this and **retry the whole transaction**.

- This kills the lost-update anomaly: of two read-modify-write transactions, the second to write gets aborted.
- What it does **not** prevent: **write skew**, because the two transactions write to *different* rows so neither trips the first-committer-wins check.

```sql
-- Invariant: checking + savings >= 0. Both start at 500.
-- T1: SELECT sum → 1000; withdraw 800 from checking   (writes checking)
-- T2: SELECT sum → 1000; withdraw 800 from savings    (writes savings)
-- Both commit under REPEATABLE READ. Combined balance = -600. Invariant broken.
```

### 2.4 SERIALIZABLE (SSI — Serializable Snapshot Isolation)

- Same snapshot behavior as REPEATABLE READ, **plus** tracking of read/write dependencies between concurrent transactions using **predicate locks** (`SIReadLock`). These locks don't block anything — they exist only to detect dangerous dependency patterns.
- When Postgres detects a pattern that could produce a non-serializable outcome (two adjacent read-write antidependencies, the "dangerous structure"), it aborts one transaction:

```
ERROR:  could not serialize access due to read/write dependencies
        among transactions
```

- The result is equivalent to *some serial order* of the committed transactions. **Write skew is impossible.**
- The detection is conservative: **false positives happen** (transactions that would have been fine get aborted), especially when predicate locks escalate from row → page → relation granularity (tune with `max_pred_locks_per_transaction`). The contract is: *anything that commits is serializable*, not *everything serializable commits*.
- Operational rules:
  1. Every transaction must be **retryable** (catch SQLSTATE `40001`, retry with backoff).
  2. Keep transactions short; long transactions hold predicate locks and inflate abort rates.
  3. Declare read-only transactions as `READ ONLY` (Postgres can opt them out of conflict tracking via "safe snapshots", e.g. `DEFERRABLE`).

### 2.5 Choosing a level in practice

| Situation | Use |
|---|---|
| Simple CRUD, single-statement atomic updates | READ COMMITTED (default) + `UPDATE ... SET x = x + δ` |
| Read-modify-write on specific rows | READ COMMITTED + `SELECT ... FOR UPDATE` |
| Consistent multi-statement reports / backups | REPEATABLE READ (one consistent snapshot) |
| Invariants spanning multiple rows (write skew risk) | SERIALIZABLE + retry loop, or explicit locking |

For a wallet transfer (debit A, credit B, both must survive together), READ COMMITTED with row locks in a **consistent order** (lock the lower account ID first to avoid deadlocks) is the standard pattern:

```sql
BEGIN;
SELECT * FROM accounts WHERE id IN (1, 2) ORDER BY id FOR UPDATE;
UPDATE accounts SET balance = balance - 100 WHERE id = 1;  -- check >= 0 first
UPDATE accounts SET balance = balance + 100 WHERE id = 2;
COMMIT;
```

---

## 3. DynamoDB

DynamoDB is a distributed key-value store: data is partitioned by hash key across many storage nodes, each partition replicated (multi-AZ, leader + replicas). There is **no session, no `BEGIN`, no tunable isolation level**. Instead, isolation is a property of *which API you call*.

### 3.1 Single-item operations

- Every `PutItem` / `UpdateItem` / `DeleteItem` is **atomic at the item level** — concurrent writers can't interleave *within* one item; last writer wins.
- An `UpdateItem` with an arithmetic update expression is atomic read-modify-write **on the server**, like Postgres's single `UPDATE ... SET x = x + δ`:

```
UpdateExpression: "SET balance = balance - :amt"
```

- But unguarded `SET balance = balance - :amt` can drive the balance negative — there's no `WHERE`-style gate unless you add one (next section).

### 3.2 Read consistency: the only "knob"

| Read mode | Behavior | Cost |
|---|---|---|
| **Eventually consistent** (default) | May be served by a replica that hasn't received the latest write; typically lags ≪ 1 s | 0.5 RCU |
| **Strongly consistent** (`ConsistentRead: true`) | Served by the partition leader; reflects all acknowledged writes | 1 RCU; unavailable on Global Secondary Indexes; not honored across regions in Global Tables |

This is a **consistency** (recency) knob, not an isolation knob — it says nothing about multi-item atomicity. Global Tables add a third concern: cross-region replication is asynchronous and **last-writer-wins** on conflict, so concurrent writes to the same item in two regions silently drop one of them.

### 3.3 Conditional writes — optimistic concurrency control

`ConditionExpression` makes any single-item write atomic *check-and-set*. This is DynamoDB's substitute for both `WHERE` guards and `SELECT FOR UPDATE`:

```
// Guard an invariant:
UpdateExpression:    "SET balance = balance - :amt"
ConditionExpression: "balance >= :amt"
// fails with ConditionalCheckFailedException instead of going negative

// Optimistic locking (version attribute):
UpdateExpression:    "SET balance = :new, version = :v_next"
ConditionExpression: "version = :v_read"
// fails if anyone wrote the item since you read it → re-read and retry
```

The condition is evaluated **atomically with the write on the leader**, so two concurrent conditional debits can never both pass a `balance >= :amt` check that only one should pass. This pattern (read → compute → conditional write → retry on failure) is the DynamoDB equivalent of Postgres REPEATABLE READ's first-committer-wins, scoped to one item.

### 3.4 Transactions: `TransactWriteItems` / `TransactGetItems`

For multi-item atomicity, DynamoDB offers transactions (up to **100 items / 4 MB** per transaction, items can span tables but not regions, no items repeated within one transaction):

- **`TransactWriteItems`** — all-or-nothing group of `Put` / `Update` / `Delete` / `ConditionCheck` actions. Either every action commits or none do.
- **`TransactGetItems`** — a consistent snapshot read of up to 100 items.

Under the hood this is **two-phase commit with a transaction coordinator** (prepare on every participating partition leader, then commit), which is why each transactional write costs **2× the WCU** of a normal write and each transactional read 2× the RCU.

Isolation guarantees, as AWS documents them:

| Operations in conflict | Isolation |
|---|---|
| Transaction vs transaction | **Serializable** |
| Transaction vs standard single-item read/write | **Serializable** |
| `TransactWriteItems` vs `BatchGetItem` / `TransactGetItems` running concurrently | **Read-committed** at the batch level — each *item* read is serializable vs the transaction, but the batch as a whole may observe the transaction partially applied (some items pre-commit state, some post-commit) |
| `BatchWriteItem` / `BatchGetItem` internally | Just N independent single-item operations — no atomicity across the batch at all |

Key contrasts with Postgres transactions:

1. **No interactivity.** The whole transaction is submitted in one request — you cannot read, think in application code, then write *inside* the transaction. Read-then-decide logic must be expressed as `ConditionCheck` / `ConditionExpression` actions, or done optimistically (read outside, conditionally write inside).
2. **No blocking, only rejection.** Conflicting transactions don't queue; one fails with `TransactionCanceledException` (reason `TransactionConflict`) and the *client* retries. Under hot-key contention, throughput degrades via retry storms rather than lock queues.
3. **Idempotency tokens.** `ClientRequestToken` makes a `TransactWriteItems` retry-safe for 10 minutes — replaying the same token doesn't double-apply (Postgres has no built-in equivalent; you build idempotency in the schema).

The wallet transfer in DynamoDB:

```
TransactWriteItems:
  Update accounts[A]:
      SET balance = balance - :amt
      CONDITION balance >= :amt
  Update accounts[B]:
      SET balance = balance + :amt
  Put    transfers[txn-123]:
      CONDITION attribute_not_exists(pk)     ← idempotency record
```

Atomic, serializable vs all other transactions and single-item ops, and idempotent.

### 3.5 What DynamoDB does *not* give you

- **No multi-item consistent `Query`/`Scan`.** A `Scan` is not a snapshot; concurrent writes may or may not appear. There is no equivalent of REPEATABLE READ across a query result.
- **No predicate/range conditions across items** ("fail if *any* account of this user is frozen" requires modeling that fact into an item you can `ConditionCheck`).
- **No serializability for batches** (see table above).
- **No cross-region transactions** — Global Tables replication is async, last-writer-wins.

---

## 4. Side-by-Side Summary

| Dimension | PostgreSQL | DynamoDB |
|---|---|---|
| Isolation model | Tunable per transaction: RC / RR (snapshot) / SERIALIZABLE (SSI) | Fixed per API: single-item atomic ops; serializable `TransactWriteItems` |
| Default behavior | READ COMMITTED, statement-level snapshots | Atomic single-item ops, eventually consistent reads |
| Dirty reads | Impossible at any level | Impossible (you never see an uncommitted transactional write) |
| Lost update protection | Atomic `UPDATE`, `FOR UPDATE` locks, or RR/SSI aborts | Atomic `UpdateItem` expressions, `ConditionExpression` (optimistic locking) |
| Write skew protection | SERIALIZABLE only | Within one `TransactWriteItems` via `ConditionCheck`; nothing spanning separate requests |
| Multi-row consistent read | Any transaction at RR+ (one snapshot) | Only `TransactGetItems` (≤ 100 items); `Scan`/`Query` are never snapshots |
| Conflict handling | Blocking (RC) or abort + retry (`40001` at RR/SSI) | Never blocks; reject + client retry (`ConditionalCheckFailed`, `TransactionConflict`) |
| Interactive transactions | Yes — arbitrary statements between BEGIN and COMMIT | No — single-shot request, conditions instead of application logic |
| Scope limits | Transaction size effectively unbounded | 100 items / 4 MB per transaction, single region |
| Cost of stronger guarantees | Higher abort rate, predicate-lock memory | 2× RCU/WCU for transactional ops, retry storms on hot keys |

**The mental model:**

- In **PostgreSQL** you choose how much serializability you want and the database enforces it with snapshots + conflict detection; your job is to handle serialization failures with retries.
- In **DynamoDB** you get strong primitives (atomic conditional single-item writes, serializable bounded transactions) but no ambient isolation; your job is to *design the data model* so that every invariant you care about is checkable within one item or one ≤100-item transaction.

This is also why event-driven patterns like the **saga** (as used in this wallet-transfer project) pair naturally with DynamoDB-style stores: when an invariant can't fit in one transaction's scope, you decompose it into locally-atomic steps with compensating actions, instead of relying on a database-wide serializable transaction.

---

## 5. Further Reading

- PostgreSQL docs — [Transaction Isolation](https://www.postgresql.org/docs/current/transaction-iso.html)
- Ports & Cabrera et al. — *Serializable Snapshot Isolation in PostgreSQL* (VLDB 2012)
- AWS docs — [Amazon DynamoDB Transactions: How It Works](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/transaction-apis.html)
- AWS docs — [Read Consistency](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/HowItWorks.ReadConsistency.html)
- Kleppmann — *Designing Data-Intensive Applications*, ch. 7 (anomalies, snapshot isolation, write skew)
