# Twitter Snowflake — Distributed Unique ID Generation

**Snowflake** is Twitter's scheme for generating unique, roughly time-ordered 64-bit IDs across many machines **without any coordination at generation time**. Each machine mints IDs locally and independently, yet no two machines ever produce the same ID.

It answers the question:

> "How do I get globally unique, sortable IDs at millions per second — when no single database or counter can be in the hot path?"

## Why not the obvious alternatives?

| Approach | Problem |
|---|---|
| DB auto-increment | Single point of failure, single-writer bottleneck, doesn't scale across shards |
| UUID v4 (random 128-bit) | 128 bits (2x storage in every index/FK), completely unordered → random B-tree inserts cause page splits and cache thrash, not sortable by creation time |
| Central ticket server (Flickr) | Network round-trip per ID, availability risk, ordering across two servers is fuzzy |
| Timestamp alone | Collides under any concurrency — two requests in the same millisecond |

Snowflake's design goals were explicit:

- **64-bit** — fits a `BIGINT`, half the size of a UUID
- **Time-sortable (k-sorted)** — IDs generated later are (roughly) numerically larger, so `ORDER BY id` ≈ `ORDER BY created_at`, and B-tree inserts are append-mostly
- **Uncoordinated** — each node generates IDs with zero network calls
- **High throughput** — thousands of IDs per millisecond per node

## The bit layout

A Snowflake ID packs three facts into one 64-bit integer:

```
 0 | 41 bits timestamp           | 10 bits machine ID | 12 bits sequence
---+-----------------------------+--------------------+------------------
 ^   milliseconds since a          5 bits datacenter     per-millisecond
 |   custom epoch                + 5 bits worker         counter
 sign bit, always 0
 (keeps the ID positive)
```

| Field | Bits | Range | Meaning |
|---|---|---|---|
| Sign | 1 | always `0` | keeps the number positive in signed `int64` |
| Timestamp | 41 | ~69 years | milliseconds since a **custom epoch** (Twitter used `2010-11-04`) |
| Machine ID | 10 | 1024 nodes | uniquely identifies the generator instance (Twitter split it 5 datacenter + 5 worker) |
| Sequence | 12 | 4096/ms | counter for IDs minted in the **same millisecond on the same machine** |

Assembly is pure bit-shifting:

```
id = (timestamp << 22) | (machineID << 12) | sequence
```

**Why a custom epoch?** 41 bits of milliseconds is ~69.7 years. Counting from Unix epoch (1970) would have burned 40 of those years before the first ID was ever minted. Counting from your service's launch date gives you the full ~69 years of runway.

**Capacity:** 4096 sequences × 1000 ms × 1024 machines = **~4.2 billion IDs per second** across the fleet, ~4.1M/sec per machine.

### Why uniqueness holds

Two IDs can only collide if all three fields match. Different machines differ in machine ID. The same machine in different milliseconds differs in timestamp. The same machine in the same millisecond differs in sequence — and if the sequence overflows (4097th ID in one millisecond), the generator **spin-waits until the next millisecond**:

```go
func (n *Node) Generate() int64 {
    n.mu.Lock()
    defer n.mu.Unlock()

    now := currentMillis()

    if now == n.lastTimestamp {
        n.sequence = (n.sequence + 1) & 0xFFF // mask to 12 bits
        if n.sequence == 0 {
            // 4096 IDs already minted this millisecond — wait for the next one
            for now <= n.lastTimestamp {
                now = currentMillis()
            }
        }
    } else {
        n.sequence = 0
    }

    n.lastTimestamp = now
    return (now-customEpoch)<<22 | n.machineID<<12 | n.sequence
}
```

Note the whole proof rests on one assumption: **`now` never repeats a past value on the same machine**. That assumption is exactly what clock skew breaks.

### Assigning machine IDs

The only coordination Snowflake needs is **at startup**, to hand each node a unique 10-bit machine ID. Options, in increasing rigor:

- **Static config** — fine for a small, hand-managed fleet; humans will eventually duplicate one
- **Derived from host identity** — last bits of the private IP / pod ordinal (StatefulSet index); simple, but assumes the network layout guarantees uniqueness
- **Coordination service** — ZooKeeper/etcd ephemeral sequential nodes (what Twitter did); the node registers at boot, gets a unique ID, and a **lease** — if it can't renew the lease it must stop generating, which also protects against two nodes accidentally holding the same ID

## The clock skew problem

The timestamp field comes from the **wall clock** (`System.currentTimeMillis()` / `time.Now()`), and wall clocks are not monotonic:

- **NTP step corrections** — if a node's clock has drifted ahead and NTP corrects it, time jumps *backward*, sometimes by seconds
- **Leap seconds** — historically caused backward jumps on some systems
- **VM migration / host suspend** — guest clock can be reset or jump on resume
- **Manual misconfiguration** — someone sets the clock

If the clock moves back, `currentMillis()` returns a timestamp the node has **already minted IDs for**. The sequence counter was reset to 0 when that millisecond first passed — so the node will regenerate the exact same IDs. Silent duplicate ID generation is one of the worst failure modes a system can have (think: two wallet transactions sharing an ID).

### Handling strategies

**1. Refuse to generate (fail fast)** — what Twitter's original implementation did:

```go
if now < n.lastTimestamp {
    return 0, fmt.Errorf("clock moved backwards by %dms; refusing to generate", n.lastTimestamp-now)
}
```

Correctness over availability. Callers see errors until the clock catches up. Right default for anything money-adjacent.

**2. Wait it out (bounded)** — if the regression is small (a few ms, typical of NTP slew boundaries), just sleep until the clock catches up to `lastTimestamp`:

```go
if now < n.lastTimestamp {
    drift := n.lastTimestamp - now
    if drift <= 5 { // small skew: tolerate
        time.Sleep(time.Duration(drift) * time.Millisecond)
        now = currentMillis()
    } else {
        return 0, ErrClockMovedBackwards // large skew: refuse
    }
}
```

This is the common production pattern: absorb tiny regressions, alarm and refuse on big ones.

**3. Keep using logical time (don't trust the clock at all for ordering)** — never let the timestamp field go backward; treat `lastTimestamp` as the floor and keep incrementing the sequence within it:

```go
if now < n.lastTimestamp {
    now = n.lastTimestamp // pin to the highest time we've seen
}
```

Combined with the sequence-overflow rule ("when sequence wraps, advance `lastTimestamp` by 1ms"), the generator keeps producing unique IDs even with a wrong clock — timestamps drift from real time temporarily but uniqueness and monotonicity hold. (This is how Baidu's UidGenerator and Seata's IDs behave.)

**4. Spare machine IDs as escape hatch** — Meituan's Leaf-snowflake: on detecting backward drift, the node can re-register with ZooKeeper for a *fresh machine ID* — a different machine ID can never collide with the old one's IDs, so it can resume immediately regardless of the clock.

**5. Operational hygiene (do these regardless):**

- Run NTP in **slew mode** (`slewalways` / chrony `makestep` only at boot) — slewing skews the clock rate gradually instead of stepping it backward
- **Persist `lastTimestamp`** periodically (and to ZooKeeper on shutdown); on startup, refuse to start until `wallClock > lastPersistedTimestamp` — protects against the restart-after-clock-reset case, where in-memory protection is lost
- Alarm on detected drift even when auto-handled

## A different approach to time: stop reading the wall clock per ID

All the strategies above *react* to a broken clock. The cleaner fix is to change what "time" means inside the generator so the problem can't occur.

### Monotonic clock + one wall-clock anchor

Operating systems expose two clocks:

| Clock | Property | Problem |
|---|---|---|
| Wall clock (`CLOCK_REALTIME`) | meaningful absolute time | can jump backward/forward (NTP, manual set) |
| Monotonic clock (`CLOCK_MONOTONIC`) | **never goes backward**, ticks at a steady rate | meaningless absolute value (e.g. nanoseconds since boot) |

So: read the wall clock **once, at startup**, pair it with a monotonic reading, and from then on derive every timestamp from monotonic elapsed time:

```go
type Node struct {
    epochWall  int64     // wall-clock millis at startup (read once)
    epochMono  time.Time // monotonic anchor at startup
    // ...
}

func (n *Node) currentMillis() int64 {
    // time.Since uses the monotonic reading — immune to wall-clock jumps
    return n.epochWall + time.Since(n.epochMono).Milliseconds()
}
```

Properties:

- Within one process lifetime, time **cannot go backward** — the `now < lastTimestamp` branch is dead code
- NTP stepping the wall clock mid-flight is invisible to the generator
- The residual risk moves to **restart**: if the wall clock was set backward *while the process was down*, the new anchor could be in the past relative to already-minted IDs. Pair with the persisted-`lastTimestamp` startup check ("wait until wall clock exceeds the high-water mark") to close that hole.
- Trade-off: timestamps embedded in IDs can drift slightly from true wall time over long uptimes (monotonic clocks drift too, and NTP corrections no longer reach you) — acceptable, because the timestamp's job here is *ordering and uniqueness*, not being an authoritative `created_at`. Store a real timestamp column if you need one.

This is what Go's `time` package quietly does for `time.Since` already, and it's the approach Sonyflake-style libraries adopt: **wall clock for meaning, monotonic clock for progress.**

### Pure logical time (no clock in the hot path at all)

Take it one step further — the "timestamp" is just a counter that *starts* from real time and is advanced **by the generator itself**, never read again:

- Initialize `T = wallClockMillis()` at boot
- Mint IDs as `(T, sequence++)`; when the sequence overflows, `T++`
- Optionally, a background ticker nudges `T` toward real time when `T` lags behind (but never backward)

Now uniqueness depends on nothing but in-process state. The ID's time component becomes approximate ("k-sorted, roughly creation-ordered") rather than exact — which was always the honest contract of Snowflake IDs anyway, since clock skew *between* machines (often tens of ms) already means cross-machine ordering is only approximate.

### Related designs worth knowing

| System | Twist on Snowflake |
|---|---|
| **Sonyflake** (Sony) | 39-bit time in **10ms units** (174 years of runway), 16-bit machine ID (65k nodes), 8-bit sequence — trades peak per-node throughput (25.6k/sec) for fleet size and lifetime |
| **Instagram** | Snowflake layout computed **inside Postgres** (`41 time | 13 shard | 10 per-shard sequence`) using a per-shard DB sequence — no separate ID service at all |
| **Leaf** (Meituan) | Dual mode: segment mode (DB allocates ranges of IDs to nodes — no timestamps, no clock problem) and snowflake mode with the ZooKeeper clock-check described above |
| **UidGenerator** (Baidu) | Seconds-granularity time + big sequence; consumes "future" time when the sequence outruns the clock — i.e., logical time in practice |
| **ULID / UUIDv7** | 128-bit, timestamp + randomness instead of machine ID + sequence — no machine ID assignment needed, but back to 128 bits and probabilistic (not guaranteed) uniqueness |

## Summary

- Snowflake = `time (41) | machine (10) | sequence (12)` in an int64: unique, k-sorted, coordination-free at generation time, ~4M IDs/sec/node
- Uniqueness rests entirely on "time never repeats on a machine" — which the wall clock does **not** guarantee
- Clock skew handling, weakest to strongest: refuse on regression → wait out small drift → pin to logical time / fresh machine ID
- The structural fix is to take the wall clock out of the hot path: **anchor once at startup, advance with the monotonic clock** (or a pure logical counter), persist a high-water mark to survive restarts — then backward clock jumps simply can't reach the generator
