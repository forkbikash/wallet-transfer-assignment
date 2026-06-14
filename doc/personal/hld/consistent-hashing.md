# Consistent Hashing

**Consistent hashing** is a technique for distributing keys across a set of nodes so that when nodes are added or removed, only a small fraction of keys need to move.

It answers the question every distributed system eventually faces:

> "Which node owns this key — and what happens when my cluster changes size?"

## The problem with naive hashing

The obvious way to shard keys across `N` nodes:

```
node = hash(key) % N
```

This works until `N` changes. If a node dies (`N=4 → N=3`) or you add capacity (`N=4 → N=5`), the modulus changes for **almost every key**:

```
hash("user:42") = 1000

N=4:  1000 % 4 = 0  -> node 0
N=5:  1000 % 5 = 0  -> node 0   (lucky)

hash("user:99") = 1003

N=4:  1003 % 4 = 3  -> node 3
N=5:  1003 % 5 = 3  -> node 3   (lucky)

hash("user:7")  = 1001

N=4:  1001 % 4 = 1  -> node 1
N=5:  1001 % 5 = 1  -> node 1
```

On average, changing `N → N+1` remaps `N/(N+1)` of all keys — roughly **80–95% of your data moves** in a typical cluster. For a cache this means a stampede of misses hitting the database; for a storage system it means a massive rebalancing storm.

### What's the best we could hope for?

Some movement is unavoidable. If a 5th node joins a 4-node cluster, it must end up owning its fair share — 1/5th of the keys — so 1/5th of the keys *have* to move onto it. That's the floor.

The problem with `hash % N` is everything it moves **beyond** that floor: keys shuffling between the old nodes (node 1 → node 3, node 2 → node 0, ...) that accomplish nothing — the cluster is no more balanced for it, and every one of those moves is a cache miss or a network transfer.

So the ideal scheme moves **exactly the minimum**:

| Event (cluster of N) | Keys that move | Why |
|---|---|---|
| Node joins | `1/(N+1)` of keys → onto the new node | its fair share, nothing else |
| Node leaves | `1/N` of keys → off the dead node | its data must go *somewhere* |
| Either way, between surviving nodes | **zero** | no pointless shuffling |

**Goal:** on any single membership change, only the keys belonging to the joining/leaving node move (`~1/N` of the total); every key on an unaffected node stays put. Consistent hashing achieves exactly this.

## How consistent hashing works

### 1. The hash ring

Imagine the output space of a hash function (e.g. `0` to `2^32 - 1`) bent into a circle — the **ring**. Both **nodes** and **keys** are hashed onto the same ring:

- Each node is placed at `hash(node_id)`.
- Each key is placed at `hash(key)`.
- A key is owned by the **first node found moving clockwise** from the key's position.

```
                    0 / 2^32
                       │
              ┌────────┴────────┐
       k4 ●   │                 │
              │                 │  ● Node A (pos 50)
   Node D ●   │                 │
   (pos 270)  │      RING       │      ● k1  -> owned by Node B
              │                 │
              │                 │  ● Node B (pos 120)
       k3 ●   │                 │
              └────────┬────────┘
                       │   ● k2  -> owned by Node C
                  ● Node C (pos 200)

k1 (pos 90)  -> clockwise -> Node B (120)
k2 (pos 190) -> clockwise -> Node C (200)
k3 (pos 230) -> clockwise -> Node D (270)
k4 (pos 300) -> clockwise, wraps past 0 -> Node A (50)
```

### 2. Adding a node

Add **Node E** at position 160. It takes over only the arc between Node B (120) and itself (160) — keys previously owned by Node C:

```
Before:  (120 ─────────── 200]  all owned by C
After:   (120 ── 160]  owned by E      <- only these keys move
         (160 ── 200]  still owned by C
```

Nodes A, B, D are completely untouched. Only `~1/N` of keys move — exactly the goal.

### 3. Removing a node

If Node C dies, its keys simply flow clockwise to the next node (Node D). Again, no other node's keys are affected.

### 4. Virtual nodes (vnodes)

With one position per node, two problems appear:

1. **Uneven distribution** — random placement means some nodes own huge arcs, others tiny ones.
2. **Hotspot on failure** — when a node dies, its *entire* load lands on one neighbor.

The fix: each physical node is hashed onto the ring **many times** (typically 100–1000 virtual nodes), e.g. `hash("nodeA#0")`, `hash("nodeA#1")`, ... `hash("nodeA#255")`.

```
Ring with vnodes:   A B C A C B A C B A B C ...   (interleaved)
```

Benefits:

- Load evens out statistically (variance shrinks as vnode count grows).
- When a node dies, its arcs are scattered around the ring, so its load is **spread across all surviving nodes** instead of dumped on one.
- Heterogeneous hardware: give a 2x-bigger machine 2x the vnodes.

### Key properties

| Property | Value |
|---|---|
| Keys moved on node add/remove | ~K/N (K keys, N nodes) |
| Lookup | O(log V) — binary search over V vnode positions |
| Memory | O(V) — just the sorted ring of vnode positions |
| Balance (with vnodes) | within a few % of uniform |
| Coordination needed | None for lookups — any client with the ring can route |

## System design example: a distributed key-value store

Design a Dynamo-style KV store (think DynamoDB / Cassandra / Riak) supporting `GET(key)` and `PUT(key, value)` across a cluster of commodity nodes.

### Requirements

- Scale horizontally to billions of keys.
- Adding/removing a node must not cause a rebalancing storm.
- Tolerate node failures without losing data (replication).
- No single point of failure for routing.

### Architecture

```
                 PUT("user:42", {...})
                          │
                          ▼
                  ┌──────────────┐
   client ──────► │ Coordinator   │  (any node can coordinate;
                  │ (Node B)      │   it hashes the key onto the ring)
                  └──────┬───────┘
                         │ hash("user:42") -> ring position 90
                         │ walk clockwise, pick N=3 distinct nodes
              ┌──────────┼──────────┐
              ▼          ▼          ▼
          Node B      Node C      Node D
         (primary)  (replica 2) (replica 3)
         "preference list" = next N distinct physical nodes clockwise
```

**1. Partitioning.** Every node holds an identical copy of the ring metadata (vnode positions → physical nodes), shared via gossip or a small config service (etcd/ZooKeeper). To route a request, a node hashes the key and binary-searches the ring — no central lookup service, no directory hot spot.

**2. Replication.** Instead of storing a key only on its owner, walk clockwise and store it on the next `N` **distinct physical nodes** (skipping vnodes of the same machine). This list is the key's *preference list*. Consistent hashing makes the preference list deterministic — every node computes the same list independently.

**3. Tunable consistency.** With `N` replicas, require `W` acks for a write and `R` reads for a read:

- `W=2, R=2, N=3` → `R + W > N` gives quorum consistency.
- `W=1, R=1` → fast but eventually consistent.

**4. Adding a node (the payoff).** Say the cluster has 10 nodes holding 1 TB total and we add an 11th:

| | `hash % N` | Consistent hashing |
|---|---|---|
| Data moved | ~910 GB (91%) | ~91 GB (1/11) |
| Nodes involved in transfer | all 10 | only the vnode neighbors |
| Cache hit rate during rebalance | collapses | barely dips |

The new node announces itself, gets assigned vnodes, and streams just those key ranges from their current owners. The rest of the cluster serves traffic unaffected.

**5. Handling failure.** If Node C is down during a write, the coordinator writes to the next node on the ring (Node E) with a *hinted handoff* — "this belongs to C, return it when C recovers." Reads still meet quorum from the surviving replicas. When C rejoins, only its own arcs need to catch up (via anti-entropy / Merkle tree sync), not the whole dataset.

### Walkthrough of a `PUT`

1. Client sends `PUT("user:42", v)` to any node (say B).
2. B computes `hash("user:42") = 90`, binary-searches the ring, finds the preference list `[B, C, D]`.
3. B writes locally and forwards to C and D in parallel.
4. On `W=2` acks, B returns success to the client.
5. D's ack arrives later — fine, it's replica 3.

### Pseudocode: the full picture

The ring itself stores **no keys** — it is pure routing metadata (a few KB). Keys live in an ordinary local store (hash map, LSM tree, ...) on each physical node. The ring's only job is to answer "which node(s) should hold this key?"; the key-value operations then go to those nodes.

**1. The ring — routing metadata only:**

```go
type Ring struct {
    positions []uint32          // sorted vnode positions
    owner     map[uint32]string // vnode position -> physical node ID
}

// Lookup: which physical node owns this key?
func (r *Ring) Lookup(key string) string {
    h := hash(key)
    // first vnode position >= h, wrapping to 0
    i := sort.Search(len(r.positions), func(i int) bool {
        return r.positions[i] >= h
    })
    if i == len(r.positions) {
        i = 0 // wrap around the ring
    }
    return r.owner[r.positions[i]]
}
```

**2. Building the ring — each physical node gets many vnodes:**

```go
func (r *Ring) AddNode(nodeID string, vnodes int) {
    for v := 0; v < vnodes; v++ {
        pos := hash(fmt.Sprintf("%s#%d", nodeID, v)) // "nodeA#0", "nodeA#1", ...
        r.owner[pos] = nodeID
        r.positions = append(r.positions, pos)
    }
    sort.Slice(r.positions, func(i, j int) bool {
        return r.positions[i] < r.positions[j]
    })
}
```

**3. Storing and reading keys — route via the ring, store on the node:**

Each node has its own local store; the cluster maps node IDs to those stores (in reality, a network client per node):

```go
type Node struct {
    id   string
    data map[string]string // local KV storage — this is where keys actually live
}

type Cluster struct {
    ring  *Ring
    nodes map[string]*Node // node ID -> node (in production: an RPC client)
}

func (c *Cluster) Put(key, value string) {
    nodeID := c.ring.Lookup(key)        // routing decision
    c.nodes[nodeID].data[key] = value   // actual storage (RPC in production)
}

func (c *Cluster) Get(key string) (string, bool) {
    nodeID := c.ring.Lookup(key)
    v, ok := c.nodes[nodeID].data[key]
    return v, ok
}
```

**4. Replication — store on the next N distinct physical nodes:**

`Lookup` returns one owner; for fault tolerance, keep walking clockwise past further vnodes, collecting distinct *physical* nodes (skip vnodes of a node already picked):

```go
func (r *Ring) PreferenceList(key string, n int) []string {
    h := hash(key)
    i := sort.Search(len(r.positions), func(i int) bool {
        return r.positions[i] >= h
    })

    var list []string
    seen := map[string]bool{}
    for steps := 0; len(list) < n && steps < len(r.positions); steps++ {
        pos := r.positions[i%len(r.positions)] // wraps around the ring
        node := r.owner[pos]
        if !seen[node] { // skip extra vnodes of an already-chosen node
            seen[node] = true
            list = append(list, node)
        }
        i++
    }
    return list
}

func (c *Cluster) Put(key, value string) {
    for _, nodeID := range c.ring.PreferenceList(key, 3) { // N=3 replicas
        c.nodes[nodeID].data[key] = value // parallel RPCs; wait for W acks
    }
}
```

**5. Key movement — what actually happens when a node joins:**

This is the payoff step. When a node joins, re-`Lookup` decides which keys it now owns, and only those stream over:

```go
func (c *Cluster) Join(newID string, vnodes int) {
    c.nodes[newID] = &Node{id: newID, data: map[string]string{}}
    c.ring.AddNode(newID, vnodes)

    // Each existing node hands over only the keys the new ring assigns away from it.
    for _, node := range c.nodes {
        for key, value := range node.data {
            if owner := c.ring.Lookup(key); owner != node.id {
                c.nodes[owner].data[key] = value // stream to new owner
                delete(node.data, key)
            }
        }
    }
}
```

With consistent hashing, that `owner != node.id` condition is true for only `~1/N` of the keys — and the new owner is always the joining node, never a reshuffle between old nodes. (Production systems don't scan every key like this; they know each vnode's arc `(prev_position, position]` and stream just those contiguous ranges. Same outcome, no full scan.)

## Real-world users

- **Amazon DynamoDB / the Dynamo paper (2007)** — the canonical design above; consistent hashing + vnodes + quorums.
- **Apache Cassandra** — vnodes (default 256 per node) on a token ring.
- **Memcached clients (Ketama)** — consistent hashing client-side so cache nodes can come and go with minimal miss storms.
- **Riak** — fixed-partition ring (a power-of-2 number of partitions claimed by nodes).
- **Akamai / CDNs** — the original 1997 consistent hashing paper came out of CDN request routing.
- **Envoy / load balancers** — ring hash LB policy for session affinity.

## Variations worth knowing

- **Rendezvous (HRW) hashing** — for each key, score every node with `hash(key, node)` and pick the max. Simpler (no ring state), same 1/N movement property; O(N) per lookup unless tree-structured. Great for small node counts.
- **Jump consistent hash (Google)** — O(1) memory, near-perfect balance, but only supports numbered buckets added/removed at the end — good for fixed shard pools, not arbitrary node failure.
- **Maglev hashing (Google)** — precomputed lookup table for fast, even load balancing with minimal disruption; used in Google's network LB.

## Trade-offs and gotchas

- **Range queries are lost.** Hashing destroys key order — `SCAN user:1..user:100` hits every node. Systems needing range scans (HBase, Bigtable, TiKV) use range partitioning instead, at the cost of harder rebalancing.
- **Vnode count is a knob.** Too few → imbalance; too many → bigger ring metadata, more fragmented transfer streams. 100–500 per node is typical.
- **Hot keys still exist.** Consistent hashing balances *key counts*, not *traffic*. A single celebrity key still hammers one preference list — handle with caching or key-splitting (`key#1..key#k`).
- **Ring membership must converge.** If two nodes disagree about the ring, they route the same key differently. Gossip protocols converge eventually; a strongly consistent config store (etcd) avoids the ambiguity at the cost of a dependency.

## Summary

Consistent hashing decouples **where data lives** from **how many nodes exist**. Hash nodes and keys onto the same ring, walk clockwise for ownership, scatter vnodes for balance, and replicate down the preference list. The result: cluster membership changes move only `~1/N` of the data, lookups are local and coordination-free, and the design scales from a 3-node Memcached pool to a planet-scale Dynamo.
