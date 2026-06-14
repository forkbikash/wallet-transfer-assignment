# Designing a Highly Scalable Proximity Service

How to design a Yelp/Google-Maps-class "find things near me" service that answers: **given a lat/long and a radius, return all businesses (or drivers, friends, restaurants) within that radius**, ranked and paginated — for hundreds of millions of users.

It answers the question:

> "How do I answer 'what's within 5km of me?' at 5,000 QPS over 200M points, when a database can't index two dimensions (lat, long) with one B-tree?"

## 1. Requirements

### Functional

- **Nearby search**: given `(lat, long, radius)`, return matching businesses
- View business detail
- Business owners add / update / delete businesses (changes need not be visible in search instantly — next-day is fine for Yelp-style; seconds for Uber-style)
- Optional: filters (category, open-now), ranking (distance, rating)

### Non-functional

| Property | Target | Implication |
|---|---|---|
| Scale | 100M DAU, 200M businesses | dataset is large but **fits in memory when reduced to IDs + coordinates** (~few GB) |
| Read:write ratio | extremely read-heavy | search QPS ≫ business-update QPS; replicate the index aggressively |
| Search latency | p99 < 200ms | geo index must be in-memory or cache-resident |
| Availability | high (99.99%) | stale search results are fine — AP system (see [[cap-theorem]]) |
| Consistency | eventual | a new business appearing in search after minutes/hours is acceptable |

The key observation mirroring the social-feed design ([[social-network-system-design]]): **eventual consistency is genuinely acceptable**, so the search index can be a rebuilt/replicated derivative of the source-of-truth database rather than the database itself.

### Back-of-envelope numbers

- 100M DAU, each does ~5 searches/day → 500M searches/day → **~5,800 QPS** (×3 peak ≈ 17K QPS)
- 200M businesses, ~1% updated per day → **~23 writes/sec** — trivial
- Geo index size: 200M × (8B business ID + 8B geohash/cell ID) ≈ **~3.2GB** — fits comfortably in memory on one machine; replicate for throughput, not for capacity

Read QPS is 1000× write QPS → **precompute and replicate the read path; keep the write path simple.**

## 2. The core problem: indexing two dimensions

A naive query:

```sql
SELECT id FROM business
WHERE lat BETWEEN :lat - :d AND :lat + :d
  AND long BETWEEN :long - :d AND :long + :d;
```

A B-tree index on `lat` narrows to a horizontal stripe of the planet — potentially millions of rows — and then the `long` predicate is a scan over that stripe (or vice versa). Two separate one-dimensional indexes **cannot be intersected efficiently**; the database picks one and filters the rest. (Why a single composite B-tree doesn't help either: B-trees order by one dimension first, so points close in 2D space land far apart in the index. See [[lsm-tree-vs-b-tree]] for index mechanics.)

**The fix in every geo index: map 2D space onto a 1D key that mostly preserves locality**, then use ordinary 1D indexes. There are two families:

```
                    Geo indexes
                        |
        +---------------+----------------+
        |                                |
  Hash / fixed grid                Tree / adaptive
        |                                |
  - even grid                      - quadtree
  - geohash                        - R-tree
  - S2 (Hilbert curve)             - k-d tree
```

## 3. Option A: Geohash

Recursively bisect the world. Each split appends one bit: longitude split → first bit, latitude split → second bit, alternating. Interleaved bits are base32-encoded into a string like `9q8yy`.

- **Longer prefix = smaller cell.** Precision 6 ≈ 0.6km × 0.6km cells, precision 5 ≈ 5km × 5km, precision 4 ≈ 20km × 39km.
- **Shared prefix ≈ nearby** — `9q8yy*` strings sort adjacently, so a prefix scan on an ordinary B-tree (or a Redis sorted set) returns a spatial cell.

### Query algorithm

1. Map the search radius to a precision (5km radius → precision 5).
2. Compute the geohash of the user's location at that precision.
3. **Fetch the cell AND its 8 neighbors** — critical, because of two edge cases:
   - **Boundary problem**: two points 1m apart on opposite sides of a cell edge share *no* prefix. Neighbors fix this.
   - Cells on the equator/antimeridian have non-adjacent string prefixes; neighbor computation must be geometric, not string-based (every geohash library provides this).
4. Union the candidates, compute exact haversine distance, filter by radius, rank, paginate.

### Storage

```sql
-- one row per (cell, business); composite primary key, no updates ever — only insert/delete
CREATE TABLE geo_index (
  geohash     VARCHAR(12),
  business_id BIGINT,
  PRIMARY KEY (geohash, business_id)
);
-- prefix query: WHERE geohash LIKE '9q8yy%'
```

Or in Redis: one set per cell (`GEOADD`/sorted sets — Redis GEO commands are geohash 52-bit scores under the hood).

**Pros**: dead simple; works on any B-tree database; prefix length = zoom level for free; easy to shard/cache by prefix.
**Cons**: fixed grid — dense areas (Manhattan) and empty areas (ocean) get the same cell size, so a busy cell can hold 100K businesses while its neighbor holds 0.

## 4. Option B: Quadtree

Recursively split each region into 4 quadrants **only when it exceeds a threshold** (e.g., 100 businesses). Leaf nodes hold business IDs; dense areas get deep, tiny leaves; empty areas stay as one giant leaf.

```
 root (world)
  ├── NW ── (≤100 businesses → leaf)
  ├── NE ── split again
  │    ├── NW (leaf)
  │    ├── NE ── split again (Manhattan…)
  │    ├── SW (leaf)
  │    └── SE (leaf)
  ├── SW (leaf)
  └── SE (leaf)
```

- ~200M businesses / 100 per leaf ≈ 2M leaves; tree ≈ **a few GB → in-memory structure**, built at server startup.
- Query: descend to the leaf containing the point; expand to sibling/adjacent leaves until the radius is covered (or until you have k results — quadtrees naturally support **"k nearest"** queries, which geohash does poorly).

**Pros**: adapts to density; great k-NN support.
**Cons**: it's an in-memory server, not a database row format — you must build it (minutes at startup over 200M points → staggered rolling deploys), rebuild or carefully lock it on updates, and operate it like a stateful service.

## 5. Option C: Google S2 (mention-level)

Maps the sphere onto 6 cube faces, then onto a 1D **Hilbert curve** — better locality preservation than geohash's Z-order (points close on the curve are *always* close in space, with fewer discontinuities). Supports **region covering**: approximate any arbitrary shape (polygon, circle) as a union of variable-sized cells — this is how geofencing is done at Google/Uber-scale. Powerful but significantly more complex; pick it when you need arbitrary-shape geofencing, not for a basic radius search.

### Which to choose

| | Geohash | Quadtree | S2 |
|---|---|---|---|
| Implementation | trivial (string prefix) | in-memory tree, build/refresh logic | library, steep learning curve |
| Density adaptive | no | yes | yes |
| k-nearest | awkward | natural | possible |
| Works in plain DB / Redis | **yes** | no | partially |
| Used by | Redis GEO, Lyft, Elasticsearch | Yelp-style designs | Google Maps, Uber (H3 is the hex cousin) |

**Interview/production default: geohash (or Redis GEO) — simplest thing that works at this scale.** Reach for quadtree/S2 when density adaptivity or shape covering is a hard requirement.

## 6. High-level architecture

```
                          clients
                             |
                             v
                    LB / API Gateway
                       |          |
            (read path)|          |(write path)
                       v          v
              Location-based   Business
              Search Service   Service (CRUD)
                       |          |
                       v          v
              Geo Index        Business DB (source of truth,
              (Redis cluster      sharded by business_id)
               replicas, or          |
               read replicas         v
               of geo_index       CDC / nightly job
               table)  <-------- rebuilds & updates geo index
                                  + warms business-detail cache
```

- **Search service is stateless** — scale horizontally behind the LB.
- **Geo index is small (~GBs) → don't shard it for capacity; replicate it for QPS.** Many full copies, each serving reads. Sharding by geohash prefix is possible but creates hotspots (everyone in a city hits one shard) and complicates neighbor queries — avoid unless the dataset genuinely outgrows a node.
- **Business DB** is the boring part: sharded relational store keyed by `business_id` (see [[consistent-hashing]] for placement), with a cache in front for business detail by ID (cache key = business ID — very high hit rate, business data changes rarely).
- **Write path never touches the read index synchronously**: updates land in the Business DB; a CDC stream or batch job applies them to the geo index replicas. For Yelp-freshness, nightly rebuild is fine; for fresher results, stream via Kafka ([[kafka-features-and-patterns]]) like the social-feed fanout.

### Read path, end to end

1. Client sends `(lat, long, radius, filters)`.
2. Search service computes geohash + 8 neighbors at the right precision.
3. Fetch candidate business IDs from the geo index (one `MGET`/9 set reads, or one `LIKE prefix%` query per cell).
4. Hydrate top candidates from the business-detail cache (batch `MGET`).
5. Compute exact distances, filter, rank, paginate, return.

Total: a couple of cache round-trips → easily inside the 200ms budget.

## 7. Caching

Two caches, both keyed on **immutable-ish keys**:

| Cache | Key | Value | Invalidation |
|---|---|---|---|
| Geo cell | geohash (precisions 4/5/6 only) | list of business IDs in cell | on business add/delete in that cell — update both DB and cache (dual write is OK at 23 writes/sec) |
| Business detail | business_id | full business object | on business edit |

Cache the **finite enumerable set of grid cells**, never raw lat/long (infinite key space, ~0% hit rate). Limiting clients to a few fixed radius options (500m / 1km / 5km / 20km) is what makes precisions enumerable — a product decision that buys a huge engineering simplification.

## 8. Going global

Deploy region-local stacks (US, EU, APAC) and route users to the nearest region — proximity search is naturally **geo-partitionable**: a user in Tokyo never queries Paris cells, so regions share almost nothing. Each region holds the full pipeline; the source-of-truth DB can still be global with regional read replicas. See [[global-service-design]] for the general pattern, and put [[circuit-breaker-and-bulkhead]] guards between the search service and its index/cache dependencies.

## 9. Variant: moving objects (Uber drivers / nearby friends)

Same geo problem, opposite write profile — **millions of location updates/sec**, search over *current* positions:

- Source of truth becomes **ephemeral**: store `driver_id → (geohash cell, timestamp)` in Redis with a TTL; a driver who stops reporting simply expires. No durable DB in the hot path.
- On each update: remove from old cell set, add to new cell set (or only when the cell actually changes — most updates don't cross a cell boundary).
- Fan out location updates over pub/sub (Kafka or Redis pub/sub) to subscribed viewers, exactly like feed fanout in [[social-network-system-design]].
- Use coarser cells + shorter freshness windows; precision matters less than recency.

## 10. Summary of the key decisions

1. **2D → 1D is the whole game**: geohash / quadtree / Hilbert curve exist because B-trees index one dimension.
2. **Fetch neighbors, always** — the boundary problem makes a single-cell lookup wrong, not just incomplete.
3. **Read path is a replicated in-memory derivative; write path is a boring CRUD DB** — connected by async rebuild/CDC, justified by eventual-consistency tolerance.
4. **Replicate the geo index, don't shard it** — it's small; sharding adds hotspot and neighbor-query pain for no capacity benefit.
5. **Fixed radius options → enumerable cache keys** — product constraints can be the best scaling tool.
6. **Static points vs moving objects** flips the design from read-optimized durable index to write-optimized ephemeral TTL store.
