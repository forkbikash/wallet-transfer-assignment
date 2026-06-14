# RAFT Consensus Algorithm

RAFT is a consensus algorithm designed as an understandable alternative to Paxos. It lets a cluster of machines agree on a shared state (a replicated log) and keep working even when some machines fail. It powers systems like etcd, Consul, TiKV, and CockroachDB.

**Core idea:** All changes go through a single elected leader, which replicates a log of commands to followers. A command is "committed" once a majority of nodes have it, after which it is applied to each node's state machine in the same order. Same log + same order = same state everywhere (state machine replication).

---

## 1. The Problem RAFT Solves

In a distributed system you want:

- **Consistency** — every node sees the same data in the same order.
- **Availability** — the system keeps serving even when nodes crash.
- **Partition tolerance** — network splits don't corrupt state.

CAP says you can't have all three perfectly. RAFT chooses **CP**: it stays consistent and tolerates partitions, but a cluster of `2f + 1` nodes only stays available while a **majority (quorum)** of `f + 1` nodes can talk to each other.

| Cluster size | Tolerated failures | Quorum |
|---|---|---|
| 3 | 1 | 2 |
| 5 | 2 | 3 |
| 7 | 3 | 4 |

Odd-sized clusters are preferred: a 4-node cluster still only tolerates 1 failure (quorum is 3) — you pay for an extra node with no extra fault tolerance.

---

## 2. Node Roles and Terms

Every node is in exactly one of three states:

```
                 times out,                receives votes from
                 starts election           majority of nodes
   ┌──────────┐ ───────────────► ┌───────────┐ ─────────────► ┌────────┐
   │ FOLLOWER │                  │ CANDIDATE │                │ LEADER │
   └──────────┘ ◄─────────────── └───────────┘                └────────┘
        ▲        discovers leader      │                           │
        │        or new term           │ times out,                │
        │                              │ new election              │
        └──────────────────────────────┴───────────────────────────┘
                  discovers node with higher term
```

- **Follower** — passive; responds to RPCs from leaders and candidates.
- **Candidate** — campaigning to become leader.
- **Leader** — handles all client writes; sends heartbeats; replicates the log.

**Terms** are RAFT's logical clock. Time is divided into numbered terms; each term begins with an election and has *at most one* leader. Every RPC carries the sender's term:

- If a node receives a message with a **higher** term, it updates its term and reverts to follower.
- If a node receives a message with a **lower** term, it rejects it.

This single rule cleanly handles stale leaders rejoining after a partition.

---

## 3. Leader Election

1. Followers expect periodic **heartbeats** (empty `AppendEntries` RPCs) from the leader.
2. If a follower hears nothing for a randomized **election timeout** (e.g., 150–300 ms), it assumes the leader is dead:
   - increments its term,
   - becomes a candidate,
   - votes for itself,
   - sends `RequestVote` RPCs to all peers.
3. A peer grants its vote if (a) it hasn't voted this term, and (b) the candidate's log is **at least as up-to-date** as its own (compares last log term, then last log index).
4. Outcomes:
   - **Majority of votes** → candidate becomes leader, immediately sends heartbeats.
   - **Another node's heartbeat arrives** with an equal/higher term → revert to follower.
   - **Split vote** (no majority) → wait a new *randomized* timeout and retry. Randomization makes repeated split votes vanishingly unlikely.

The "at least as up-to-date" check in step 3 is the **election restriction**: it guarantees a new leader already contains every committed entry, so committed data is never lost or overwritten.

---

## 4. Log Replication

Each log entry holds `{term, index, command}`.

1. Client sends a command to the leader.
2. Leader appends it to its own log (uncommitted).
3. Leader sends `AppendEntries(prevLogIndex, prevLogTerm, entries, leaderCommit)` to followers.
4. A follower accepts only if its log contains an entry at `prevLogIndex` with `prevLogTerm` — this **consistency check** inductively guarantees identical log prefixes.
5. Once a **majority** has the entry, the leader marks it **committed**, applies it to its state machine, and replies to the client.
6. Followers learn the commit point via `leaderCommit` in subsequent RPCs and apply entries up to it.

If a follower's log diverges (it crashed, or was a stale leader), the leader decrements that follower's `nextIndex` until the consistency check passes, then overwrites the follower's conflicting suffix with its own entries. **The leader's log is always the source of truth; leaders never overwrite or delete their own entries.**

**Safety properties RAFT guarantees:**

- **Election Safety** — at most one leader per term.
- **Leader Append-Only** — a leader never overwrites or deletes its log entries.
- **Log Matching** — if two logs have an entry with the same index and term, the logs are identical up to that index.
- **Leader Completeness** — a committed entry appears in the logs of all future leaders.
- **State Machine Safety** — no two nodes ever apply different commands at the same index.

One subtlety: a leader only commits entries **from its own term** by counting replicas. Older-term entries get committed indirectly when a current-term entry that follows them commits (this prevents the famous "figure 8" inconsistency from the RAFT paper).

---

## 5. Log Compaction (Snapshots)

The log can't grow forever. Periodically each node:

1. Takes a **snapshot** of its state machine at some applied index.
2. Records `lastIncludedIndex` and `lastIncludedTerm` in the snapshot.
3. Discards all log entries up to that index.

If a follower is so far behind that the leader has already discarded the entries it needs, the leader sends the whole snapshot via an `InstallSnapshot` RPC.

---

## 6. Cluster Membership Changes

Switching directly from an old config to a new one is unsafe — two disjoint majorities could exist simultaneously. RAFT offers two approaches:

- **Joint consensus** (original paper): an intermediate config where decisions require majorities of *both* old and new configurations.
- **Single-server changes** (common in practice, used by etcd): add or remove only one node at a time — old and new quorums always overlap, so no joint phase is needed. New nodes typically join as non-voting **learners** that catch up before being promoted.

---

# Example: Designing a Distributed Key-Value Store on RAFT

Design "MiniKV" — a strongly consistent, fault-tolerant KV store (think a simplified etcd).

## Requirements

- `PUT(key, value)`, `GET(key)`, `DELETE(key)`, plus compare-and-swap `CAS(key, expected, new)`.
- Linearizable reads and writes (a read always sees the latest committed write).
- Survive crashes/partitions of a minority of nodes.
- 5-node cluster → tolerates 2 failures.

## Architecture

```
                        Clients
                          │  (writes & linearizable reads → leader)
                          ▼
        ┌────────────────────────────────────┐
        │              Node 1 (LEADER)       │
        │  ┌──────────┐   ┌───────────────┐  │
        │  │ KV API   │──►│  RAFT module  │  │
        │  │ (gRPC)   │   │  - log (WAL)  │  │
        │  └──────────┘   │  - election   │  │
        │       ▲         └──────┬────────┘  │
        │       │ apply()        │           │
        │  ┌────┴─────────┐      │           │
        │  │ State machine│      │           │
        │  │ (hashmap /   │      │           │
        │  │  LSM tree)   │      │           │
        │  └──────────────┘      │           │
        └────────────────────────┼───────────┘
                                 │ AppendEntries / heartbeats
                ┌────────────────┼────────────────┐
                ▼                ▼                ▼
           Node 2           Node 3            Node 4, 5
          (follower)       (follower)        (followers)
```

Each node runs three layers:

1. **KV API layer** — gRPC/HTTP endpoints; redirects writes to the leader.
2. **RAFT module** — election, log replication, persistence (the consensus part is generic and knows nothing about KV semantics).
3. **State machine** — an in-memory hashmap (or RocksDB/LSM tree for larger-than-memory data) that applies committed log entries.

## Write Path: `PUT("user:42", "alice")`

```
Client          Leader              Followers (×4)
  │  PUT k,v      │                      │
  ├──────────────►│                      │
  │               │ 1. append {term:7,   │
  │               │    cmd:PUT k,v} to   │
  │               │    local WAL (fsync) │
  │               │ 2. AppendEntries     │
  │               ├─────────────────────►│
  │               │                      │ 3. append + fsync
  │               │ 4. ACK               │
  │               │◄─────────────────────┤  (leader + 2 ACKs = 3/5 majority)
  │               │ 5. committed → apply │
  │               │    to hashmap        │
  │  6. OK        │                      │
  │◄──────────────┤                      │
  │               │ 7. next heartbeat carries leaderCommit;
  │               │    followers apply to their hashmaps
```

Latency for a write ≈ one round trip to the fastest majority + two fsyncs. Slow or dead followers don't block the write as long as a quorum responds.

## Read Path: Three Consistency Options

| Strategy | How | Consistency | Cost |
|---|---|---|---|
| **Log read** | Append the read itself to the RAFT log | Linearizable | Full replication round trip — wasteful |
| **ReadIndex** | Leader records its commit index, confirms leadership with one heartbeat round, serves from local state once `applied ≥ readIndex` | Linearizable | One heartbeat round, no disk write (etcd default) |
| **Lease read** | Leader serves reads locally while its election-timeout lease is fresh | Linearizable *if clocks are well-behaved* | Free, but unsafe under clock skew |
| **Follower read** | Any follower serves from local state | Stale (eventual) | Cheapest; fine for dashboards/caches |

The leader can't naively serve reads from memory without ReadIndex/lease: a deposed leader stuck behind a partition might not know a new leader has accepted newer writes — serving the read would violate linearizability.

## Handling Failures

**Follower crashes (≤2 of 5):** Writes continue — quorum of 3 still reachable. The crashed node replays its WAL on restart, rejoins, and the leader backfills missing entries (or ships a snapshot if it's too far behind).

**Leader crashes:**
1. Followers' election timeouts fire (~150–300 ms); one becomes candidate.
2. The election restriction ensures only a node with all committed entries can win.
3. New leader appends a no-op entry for its new term (so it can safely commit prior-term entries) and resumes service. Total write unavailability: a few hundred ms.

**Network partition (2 | 3 split):**
- Majority side: elects/keeps a leader, keeps serving. ✅
- Minority side: a stale leader there can never commit (no quorum) — clients time out. Candidates keep incrementing terms but never win.
- On heal: the minority's higher term forces a step-down/re-sync; any uncommitted entries on the stale side are overwritten by the true leader's log. **No split-brain, no lost committed writes.**

## Exactly-Once Semantics (Client Retries)

A client may time out and retry a `PUT` that actually committed → duplicate application. Fix: sessions + deduplication, *inside* the state machine so it's replicated consistently:

```
Command = { clientID, seqNum, op, key, value }

State machine:
  if lastApplied[clientID] >= seqNum:
      return cachedResponse[clientID]   # duplicate — don't re-apply
  result = apply(op)
  lastApplied[clientID] = seqNum
  cachedResponse[clientID] = result
```

## Snapshots in MiniKV

When the log exceeds a threshold (e.g., 10k entries or 64 MB):

1. Serialize the hashmap (plus the dedup session table!) with `lastIncludedIndex/Term`.
2. Atomically persist, then truncate the log prefix.
3. With an LSM/RocksDB state machine, the snapshot can be a checkpoint reference instead of a full copy.

## Scaling Beyond One RAFT Group

One RAFT group = one leader = a single-machine write throughput ceiling. Real systems shard:

```
key ──hash/range──► shard ──► RAFT group (3–5 replicas each)

  Shard A (keys a–h):  group {n1*, n2, n3}     * = leader
  Shard B (keys i–p):  group {n2, n3*, n4}
  Shard C (keys q–z):  group {n3, n4, n5*}
```

This is the **multi-raft** design (TiKV, CockroachDB): many independent RAFT groups, leaders spread across nodes to balance write load. Cross-shard transactions then need an extra layer (e.g., 2PC with Percolator-style timestamps) on top of per-shard RAFT.

## Design Summary

| Concern | Mechanism |
|---|---|
| Durability | WAL fsync before ACK; majority replication |
| Consistency | Single leader, log order = apply order |
| Availability | Tolerates `f` failures with `2f+1` nodes; ~300 ms leader failover |
| Linearizable reads | ReadIndex (or leases) |
| Idempotent retries | Client sessions + seq-num dedup in the state machine |
| Unbounded log | Snapshots + log truncation, InstallSnapshot for stragglers |
| Membership change | Single-server changes with learner catch-up |
| Write scalability | Shard into multiple RAFT groups (multi-raft) |

---

## RAFT vs. Alternatives (Quick Reference)

- **Paxos** — equivalent guarantees; harder to understand/implement; Multi-Paxos roughly converges to a leader-based design like RAFT anyway.
- **ZAB (ZooKeeper)** — also leader-based; similar in spirit, predates RAFT, tightly coupled to ZooKeeper.
- **Gossip/CRDTs (Dynamo-style)** — choose availability over consistency (AP); no leader, no linearizability; right when stale reads are acceptable.

## Further Reading

- Ongaro & Ousterhout, *In Search of an Understandable Consensus Algorithm* (the RAFT paper, 2014)
- Ongaro's PhD thesis — covers membership changes, log compaction, and client interaction in depth
- [raft.github.io](https://raft.github.io) — interactive visualization
- etcd/raft and HashiCorp raft — production-grade Go implementations worth reading
