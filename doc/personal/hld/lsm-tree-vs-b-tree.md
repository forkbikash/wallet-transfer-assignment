# LSM Trees and B-Trees in Databases

Almost every database stores its data on disk using one of two storage engine designs:

- **B-Trees** — update data *in place* on disk. Used by PostgreSQL, MySQL (InnoDB), SQLite, Oracle, SQL Server.
- **LSM Trees** (Log-Structured Merge Trees) — never update in place; *append* changes and merge them later. Used by RocksDB, LevelDB, Cassandra, ScyllaDB, HBase, BadgerDB, and as the engine under CockroachDB and TiDB.

**Core idea of the whole topic:** disks are much faster at *sequential* I/O than *random* I/O, and they read/write in fixed-size blocks. B-Trees optimize for **reads** by keeping data sorted in place (paying with random writes). LSM Trees optimize for **writes** by turning all writes into sequential appends (paying with more work on reads and background compaction).

---

## 1. The Problem Both Are Solving

A database table can be millions of rows — far bigger than RAM. You need a disk data structure that supports:

1. **Point lookup** — `get(key)`
2. **Range scan** — `scan(key_a, key_b)` (so the structure must keep keys *sorted*)
3. **Insert / update / delete**

Why not a simple sorted file? Inserting into the middle of a sorted file means rewriting everything after the insertion point — O(n) per write. Why not a hash table on disk? Hashing destroys ordering, so range scans become impossible.

The other constraint is the hardware:

| Fact | Consequence |
|---|---|
| Disks read/write in blocks (typically 4 KB) | Reading 1 byte costs the same as reading 4 KB — structure data in block-sized pages |
| Sequential I/O is 10–100× faster than random I/O (even on SSDs) | Appending is cheap; scattered updates are expensive |
| SSDs wear out per write and internally erase in large blocks | Rewriting the same page repeatedly (write amplification) shortens SSD life |

B-Trees and LSM Trees are two different answers to the same question: *how do you keep sorted data on block storage and update it efficiently?*

---

## 2. B-Trees

### 2.1 Structure

A B-Tree breaks the database into fixed-size **pages** (typically 4–16 KB) and arranges them as a wide, shallow tree. Each page holds many sorted keys and pointers to child pages.

In practice databases use the **B+Tree** variant:

- **Internal pages** hold only keys + child pointers (pure routing — no row data).
- **Leaf pages** hold the actual key/value data (or row pointers).
- Leaves are linked left-to-right like a linked list, which makes range scans a simple walk along the leaf level.

```
                        ┌─────────────────────┐
              root      │   [ 100 | 200 ]     │          internal pages:
                        └──┬──────┬──────┬────┘          keys route the search
                  <100     │ 100..199    │  >=200
              ┌────────────┘      │      └───────────┐
        ┌─────┴─────┐      ┌──────┴────┐      ┌──────┴─────┐
        │ [30 | 60] │      │ [130|170] │      │ [250|300]  │
        └─┬───┬───┬─┘      └─┬───┬───┬─┘      └─┬───┬────┬─┘
          │   │   │          │   │   │          │   │    │
        ┌─▼─┐┌▼──┐┌▼──┐   ┌─▼──┐ ... (leaf pages hold the rows,
        │k:v││k:v││k:v│   │k:v │      sorted, linked ──► for scans)
        └───┘└───┘└───┘   └────┘
```

The key property: pages are **huge** compared to binary tree nodes. A 16 KB page can hold hundreds of keys, so the **branching factor** is in the hundreds. With branching factor ~500:

| Tree height | Keys addressable |
|---|---|
| 2 | 250,000 |
| 3 | 125 million |
| 4 | 62 billion |

So *any* key in a multi-terabyte table is reachable in **3–4 page reads** — and the top levels are almost always cached in RAM, so a lookup is often just 1 actual disk read.

### 2.2 Reading

`get(42)`:

1. Read the root page. Binary-search its keys to find which child covers 42.
2. Descend, repeat at each internal page.
3. At the leaf, binary-search for 42. Found (or definitively absent).

Cost: O(log n) page reads, in practice 3–4, mostly cached. **A B-Tree lookup has exactly one place to look** — this is its superpower.

Range scan: descend to the leaf containing `key_a`, then follow the leaf-level sibling links until you pass `key_b`. Sequential and fast.

### 2.3 Writing

Updates happen **in place**:

- **Update an existing key:** find its leaf, modify the value inside the page, write the page back to the *same* disk location.
- **Insert:** find the leaf where the key belongs. If the page has room, insert and write back. If the page is **full**, **split** it:

```
Insert 55 into a full leaf [10 20 30 40 50 60]:

        before                          after
   ┌──────────────┐            ┌─────────[40]──────────┐   ← parent gains
   │ parent       │            │ parent                │     a separator key
   └──────┬───────┘            └────┬──────────────┬───┘
          │                         │              │
 ┌────────▼────────┐        ┌───────▼──────┐ ┌─────▼─────────┐
 │ 10 20 30 40 50 60│  ──►  │ 10 20 30     │ │ 40 50 55 60   │
 └─────────────────┘        └──────────────┘ └───────────────┘
```

The middle key is pushed up into the parent. If the parent is also full, *it* splits, and so on up. If the root splits, the tree grows one level taller — B-Trees grow **from the top**, which is why they stay perfectly balanced forever (every leaf is at the same depth).

Deletes are the mirror image: remove the key; if a page gets too empty, merge it with a sibling (many real implementations just leave pages under-full and reclaim space lazily).

### 2.4 Crash Safety: the WAL

In-place updates create a danger: a crash halfway through writing a page (or mid-split, when multiple pages must change together) leaves the tree corrupted — a **torn page**.

The fix is the **write-ahead log (WAL / redo log)**: before touching any page, append a record describing the change to a sequential log file and fsync it. After a crash, replay the WAL to restore consistency. (PostgreSQL additionally writes a full image of each page the first time it's modified after a checkpoint — *full page writes* — to defend against torn pages.)

Note the irony: even the archetypal *update-in-place* structure needs an *append-only log* to be safe. So every B-Tree write is physically written **twice**: once to the WAL, once to the page.

### 2.5 Concurrency

Readers and writers operate on the same pages, so B-Trees need careful locking: **latch crabbing** (lock parent, lock child, release parent as you descend) or optimistic schemes. MVCC databases like Postgres layer row versioning on top so readers never block writers. This is solvable but is real complexity — contrast with LSM below, where immutability makes much of it trivial.

---

## 3. LSM Trees

### 3.1 The Core Idea

Don't update disk data in place — ever. Instead:

1. Buffer incoming writes in a sorted **in-memory** structure.
2. When it fills up, flush it to disk as an **immutable sorted file**.
3. In the background, **merge** these files to keep reads fast and discard overwritten data.

Every disk write is a big sequential append. Random writes are converted into sequential ones — this is the entire point.

### 3.2 Components

```
   writes
     │
     ▼
 ┌─────────┐   append    ┌───────────────────────────┐
 │   WAL   │◄────────────│  MemTable (sorted, in RAM)│   ← active, mutable
 └─────────┘             └────────────┬──────────────┘
  crash recovery                      │ full → made immutable, flushed
                                      ▼
              DISK        ┌───────────────────────────┐
              Level 0     │ SSTable │ SSTable │ ...   │  ← may overlap in key range
                          └────────────┬──────────────┘
                                       │ compaction (merge sort)
                          ┌────────────▼──────────────┐
              Level 1     │  SSTables, sorted, non-   │  ← each level ~10× bigger
                          │  overlapping key ranges   │     than the one above
                          └────────────┬──────────────┘
                                       ▼
              Level 2     │ ......................... │
              ...
```

**MemTable** — an in-memory sorted structure (usually a **skip list** or red-black tree) absorbing all writes. Lookups check here first, so recent data is served from RAM.

**WAL** — every write is appended to a log *before* going into the MemTable, so a crash doesn't lose the RAM-only data. Once a MemTable is flushed to disk, its WAL segment can be deleted.

**SSTable** (Sorted String Table) — an immutable file of sorted key/value pairs, written once by a sequential dump of the MemTable, never modified afterward. Each SSTable carries:
- a **sparse index** (one entry per block: "block 17 starts at key `mango`") so a lookup reads ~one block,
- min/max key metadata, and
- usually a **Bloom filter** (see 3.5).

**Levels** — SSTables are organized into levels of exponentially increasing size (L1 ≈ 10× L0's target, L2 ≈ 10× L1, ...). In levels 1+, files have non-overlapping key ranges, so a key lives in at most one file per level.

### 3.3 Writing, Updating, Deleting

- **Write/update:** append to WAL, insert into MemTable. Done — the disk saw one sequential append. An *update* doesn't touch the old value; the new version simply lives in a newer place and **shadows** the old one.
- **Delete:** you can't remove a key from immutable files, so you write a **tombstone** — a special "this key is deleted" marker. It shadows older values exactly like an update would. The actual data is reclaimed later by compaction. (Consequence: deletes temporarily *grow* the database, and tombstones must survive until they've reached the bottom level, or a deleted key could "resurrect.")

The rule that makes all of this coherent: **newer always wins**. MemTable beats L0, L0 beats L1, and so on.

### 3.4 Reading

`get(42)` must check the places newest-first and stop at the first hit:

1. MemTable (and any immutable MemTables awaiting flush)
2. Each L0 SSTable, newest first (they can overlap, so possibly all of them)
3. L1 — binary search the file metadata to find the *one* file whose range covers 42
4. L2, L3, ... same, one file per level

If nothing is found anywhere: the key doesn't exist. This is the LSM's weak spot — **a missing key requires checking every level**, and a present key may still require several file probes. There are many places to look, vs. the B-Tree's one.

### 3.5 Bloom Filters: Rescuing Reads

A **Bloom filter** is a small bitmap built per SSTable that answers "might this file contain key K?" with no false negatives:

- Filter says **no** → key is definitely not in the file → skip it, zero I/O.
- Filter says **yes** → key is *probably* there (small false-positive rate, ~1% at 10 bits/key) → read the block to confirm.

With ~10 bits per key in RAM, almost all the "look in every level" probes for absent keys are eliminated. Bloom filters are what make LSM point reads competitive in practice. (Caveat: they don't help range scans, since they can only answer exact-key questions — range scans genuinely must consult every level and merge.)

### 3.6 Compaction

Without intervention, files pile up and reads degrade. **Compaction** runs in the background: pick some SSTables, merge-sort them into new SSTables (keeping only the newest version of each key, dropping dead tombstones at the bottom level), then delete the inputs. Merging sorted files is a streaming k-way merge — cheap and sequential.

Two dominant strategies:

| | Leveled compaction | Size-tiered compaction |
|---|---|---|
| How | Merge overlapping files from level N into N+1, keep levels non-overlapping | Collect ~4 files of similar size, merge into one bigger file |
| Space overhead | Low (~10%) | High — can transiently need 2× space |
| Write amplification | Higher (~10× per level crossed) | Lower |
| Read cost | Lower (≤1 file per level) | Higher (more overlapping files) |
| Used by | RocksDB/LevelDB default | Cassandra default, good for write-heavy loads |

Compaction is the LSM's "rent": it re-reads and re-writes data that was already written, in the background, costing I/O bandwidth and CPU. If writes outpace compaction, files accumulate, and the engine throttles or stalls incoming writes (RocksDB's infamous *write stalls*).

---

## 4. Amplification: the Common Currency

Every storage engine pays in three currencies. You can never minimize all three at once (the **RUM conjecture**):

- **Write amplification** — bytes physically written to disk per byte of user data. Hurts SSD lifespan and steals write bandwidth.
- **Read amplification** — disk reads per logical query.
- **Space amplification** — disk space used vs. live data size.

| | B-Tree | LSM Tree |
|---|---|---|
| Write amp | Whole page (e.g. 16 KB) rewritten for one 100-byte row, + WAL | Each entry rewritten once per level by compaction (~10–30× total) — but all *sequential* |
| Read amp | Very low: 3–4 page reads, one place to look | Higher: many levels to check; Bloom filters + caching mitigate |
| Space amp | Pages run partially empty (typically ~33% slack); fragmentation | Old versions and tombstones linger until compaction; leveled keeps it ~10% |

Plus one more axis: B-Tree write amplification is *random* I/O at write time (foreground latency), while LSM write amplification is *sequential* I/O in the background (deferred cost).

---

## 5. Head-to-Head Summary

| Aspect | B-Tree | LSM Tree |
|---|---|---|
| Disk update model | In place, page-oriented | Append-only, immutable files |
| Write path | Find page, modify, write back (random I/O) + WAL | WAL append + RAM insert (sequential) |
| Write throughput | Good | Excellent — often the reason to choose LSM |
| Point read | Excellent, predictable (one location) | Good with Bloom filters, more variance |
| Range scan | Excellent (linked leaves) | Slower — must merge across levels; Bloom filters don't help |
| Latency profile | Steady | Can spike during compaction/write stalls |
| Background work | Little (vacuum/defrag in some systems) | Continuous compaction — needs tuning |
| Space efficiency | ~2/3 full pages | Better after compaction; also compresses better (large immutable blocks) |
| Concurrency | Page latching needed | Easier — immutable files need no locks; only MemTable is mutable |
| Transactions | Natural fit — a key lives in exactly one place, lock it | Harder — same key exists in many versions/places |
| Typical homes | PostgreSQL, MySQL/InnoDB, SQLite, Oracle | RocksDB, LevelDB, Cassandra, HBase, ScyllaDB, Badger |

---

## 6. How to Choose

**Pick B-Tree (or rather, the databases built on them) when:**
- Read-heavy or balanced workloads, lots of range queries.
- You need predictable latency and strong transactional behavior (the one-key-one-place property makes row locking straightforward).
- Classic OLTP: user accounts, orders, inventory, payments — this is why the relational mainstays are all B-Tree based.

**Pick LSM when:**
- Write-heavy workloads: event ingestion, time-series, metrics, message queues, logs.
- Huge datasets on SSDs where sequential-write behavior and compression pay off.
- You can tolerate (and tune) background compaction and occasional latency variance.

**The punchline to remember:**

> A B-Tree keeps the data sorted *in place* and pays with random writes and page rewrites.
> An LSM tree keeps the data sorted *in pieces* and pays with compaction and multi-place reads.
> Both need a WAL; both keep keys sorted; they differ in *when* they do the sorting work — B-Trees at write time, LSM trees later, in bulk, in the background.

A useful mental model: an LSM tree is a **buffered, batched, deferred B-Tree** — it accepts disorder now (fast writes) and restores order later (compaction), while a B-Tree insists on perfect order at every moment.

---

## 7. Worked Example: Life of a Key in an LSM Tree

```
t1: put(user42, "alice")     → WAL, MemTable {user42: alice}
t2: MemTable fills           → flushed: SSTable-1 in L0 contains user42=alice
t3: put(user42, "alicia")    → WAL, MemTable {user42: alicia}
                               (SSTable-1 still says "alice" — stale but shadowed)
t4: get(user42)              → found in MemTable → "alicia"  ✓ newest wins
t5: MemTable fills           → flushed: SSTable-2 in L0 contains user42=alicia
t6: delete(user42)           → tombstone in MemTable, flushed → SSTable-3
t7: get(user42)              → tombstone found first → "not found" ✓
t8: compaction merges SSTable-1,2,3 toward the bottom level:
    alice (oldest) dropped, alicia dropped, tombstone dropped at bottom level
    → user42 has physically vanished; space reclaimed
```

The same key existed in four places at once (t6) and the system stayed correct because reads always honor newest-first. That deferred cleanup at t8 is exactly the work a B-Tree would have done eagerly at t3 and t6.
