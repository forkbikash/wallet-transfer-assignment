# Designing a Highly Scalable Social Network

How to design a Twitter/Instagram-class social network that serves **hundreds of millions of users**, where the core product is: follow people, post content, and read a personalized home feed — with the feed loading in a few hundred milliseconds.

It answers the question:

> "How do I serve a home timeline assembled from thousands of people you follow, to 500M users, in <200ms — when no single database can hold or sort that much data in the hot path?"

## 1. Requirements

### Functional

- Post content (text, images, video)
- Follow / unfollow users (directed graph — no approval needed)
- **Home feed**: posts from people you follow, roughly reverse-chronological (or ranked)
- Like, comment, repost
- Notifications, search, trending

### Non-functional

| Property | Target | Implication |
|---|---|---|
| Scale | 500M DAU | nothing fits on one machine — shard everything |
| Read:write ratio | ~100:1 | optimize aggressively for reads; precompute |
| Feed latency | p99 < 200ms | feed must be served from memory, not computed per request |
| Availability | 99.99% | prefer availability over consistency (AP) |
| Consistency | eventual is fine | a post appearing 5s late in a follower's feed is acceptable; **your own** post must appear instantly to you (read-your-writes) |

The consistency point is the key unlock: a social feed is one of the rare systems where **eventual consistency is genuinely acceptable**, which is what makes the whole design possible. (See [[cap-theorem]].)

### Back-of-envelope numbers

- 500M DAU, each reads feed ~10×/day → **~58K feed reads/sec** (×3 peak ≈ 175K/sec)
- ~50M posts/day → **~600 posts/sec** (peaks in the thousands)
- Average user follows 200 people, is followed by 200
- Media: 50M posts × ~200KB avg ≈ **10TB/day** of new media

100:1 read-to-write ratio confirmed — the architecture must move work from read time to write time.

## 2. High-level architecture

```
 clients
    |
    v
 CDN (media, static) 
    |
    v
 API Gateway / LB  --->  Auth
    |
    +----------------+----------------+----------------+
    |                |                |                |
    v                v                v                v
 Post Service    Feed Service    Graph Service    User Service
    |                |                |                |
    v                v                v                v
 Posts DB        Feed Cache       Graph DB         Users DB
 (sharded)       (Redis)          (sharded)        (sharded)
    |
    v
 Kafka ----> Fanout Workers ----> Feed Cache
       \---> Search Indexer, Notification Service, Trending, Analytics
```

Every write to the Post service emits an event to Kafka; everything downstream (fanout, search, notifications, counters) is an **asynchronous consumer**. The synchronous path for a post is just "persist + ack" — fast and reliable. (See [[kafka-features-and-patterns]].)

## 3. The core problem: home feed

Two fundamental strategies:

### Fanout-on-read (pull)

At read time: fetch the list of people you follow, query each one's recent posts, merge-sort, return.

- Write is O(1) — just insert the post.
- Read is O(following): 200 lookups + a merge **per feed load**. At 175K reads/sec this is ~35M lookups/sec. Too slow, too expensive.

### Fanout-on-write (push)

At write time: when a user posts, **push the post ID into a precomputed feed list for every follower** (a Redis list/sorted-set per user). Read time is a single cache fetch.

- Read is O(1) — one Redis read. This is what makes p99 < 200ms possible.
- Write is O(followers): 200 inserts on average. At 600 posts/sec that's ~120K cache inserts/sec — fine.

| | Fanout-on-write (push) | Fanout-on-read (pull) |
|---|---|---|
| Post cost | O(followers) | O(1) |
| Feed-read cost | O(1) | O(following) |
| Feed freshness | seconds of lag | always current |
| Wasted work | feeds built for inactive users | none |
| Breaks down when | a user has millions of followers | a user follows thousands |

### The celebrity problem — and the hybrid fix

A user with 100M followers posting once would trigger 100M feed inserts — minutes of fanout lag and a write storm. The standard solution (Twitter's actual design):

> **Hybrid fanout**: push for normal users, pull for celebrities.

- Users below a follower threshold (say 10–100K): **fanout-on-write** as usual.
- Above the threshold: **no fanout**. At read time, the feed service fetches your precomputed feed from Redis, *separately* fetches recent posts from the handful of celebrities you follow (their recent posts are themselves hot-cached), and merges the two lists.

Each user follows only a few celebrities, so the read-time merge stays small. The 99.9% common case stays O(1).

Also skip fanout for **dormant users** (no login in 30 days) — rebuild their feed lazily on next login. This avoids burning memory and write throughput on feeds nobody reads.

### Fanout pipeline

```
 post --> Posts DB --> Kafka "post-created"
                          |
                          v
                    Fanout workers (consumer group)
                          |
              get follower IDs from Graph Service
                          |
              for each follower (batched):
                  ZADD feed:{follower_id} {post_id} score={snowflake_id}
                  ZREMRANGEBYRANK feed:{follower_id} 0 -801   # cap at 800
```

Key details:

- The feed cache stores **post IDs only**, not content (a "reference feed"). Hydration happens at read time from a separate hot post-content cache. This keeps feeds tiny (~800 × 8 bytes) and means an edited/deleted post never needs to be rewritten into millions of feeds.
- Feeds are **capped** (Twitter: ~800 entries). Nobody scrolls past that; pagination beyond the cap falls back to pull.
- Fanout workers scale horizontally as a Kafka consumer group; a celebrity post simply skips the fanout step entirely.

**Memory check**: 500M active feeds × 800 entries × ~20 bytes (id + score + overhead) ≈ **8TB** → a Redis cluster of ~80 nodes at 100GB each. Entirely feasible.

## 4. Data model & storage

Different access patterns want different stores — polyglot persistence:

| Data | Store | Why |
|---|---|---|
| Users | Sharded SQL (by user_id) | relational, transactional profile updates |
| Posts | Sharded SQL or Cassandra (by post_id) | write-heavy, append-only, time-ordered reads |
| Follow graph | Sharded KV with two indexes | the two queries below |
| Feed cache | Redis sorted sets | O(log n) insert, range read, score = time |
| Media | Object store (S3) + CDN | blobs don't belong in databases |
| Counters | Redis + periodic flush | like counts at 100K/sec can't hit the DB per click |
| Search | Elasticsearch / inverted index | full-text, fed from Kafka |

### Posts

```
post_id      BIGINT   -- Snowflake ID: time-ordered, generated without coordination
author_id    BIGINT
content      TEXT
media_urls   JSON
created_at   TIMESTAMP
```

Post IDs are **Snowflake IDs** (see [[twitter-snowflake-id-generation]]) — this is load-bearing:

- Generated on any node with zero coordination at 600+/sec.
- Time-ordered, so the ID itself is the feed sort key (`ZADD score = post_id`) — no separate timestamp needed, and cursor pagination is just "give me IDs < cursor".

**Sharding posts by post_id** (not author_id) spreads a celebrity's posts across all shards — no hot shard when one account dominates traffic. The trade-off ("get all posts by author X" now hits every shard) is absorbed by keeping each user's recent post IDs in a per-author cache. Shard routing uses consistent hashing so adding nodes only remaps ~1/n of keys (see [[consistent-hashing]]).

### Follow graph

Two queries, both needed at scale, with opposite access patterns:

- `following(user)` — needed at **read** time (pull path, "do I follow X?")
- `followers(user)` — needed at **write** time (fanout)

So store the edge **twice**, in two tables sharded differently:

```
following:  (follower_id, followee_id)   sharded by follower_id
followers:  (followee_id, follower_id)   sharded by followee_id
```

Each lookup hits exactly one shard. The double-write is kept eventually consistent via the same event pipeline. A celebrity's follower list (100M rows on one shard) is read **sequentially by fanout workers only** — and celebrities skip fanout anyway.

## 5. Serving a feed read (end to end)

```
GET /feed?cursor=...
  1. ZREVRANGEBYSCORE feed:{user_id} max={cursor} count=20     -- post IDs, O(1)-ish
  2. + recent post IDs of followed celebrities (hot cache), merge by ID
  3. MGET post:{id} × 20 from post-content cache (Redis/Memcached)
       cache miss -> Posts DB -> backfill cache
  4. Batch-fetch author profiles, like counts (counter cache)
  5. Return page + next cursor = smallest post_id returned
```

Read-your-writes for your own post: the posting client appends the new post optimistically, and the feed service merges the author's own recent posts at read time — so you never see your own post "missing" while fanout is in flight.

## 6. The supporting cast

**Media**: client gets a pre-signed URL, uploads directly to object storage (the upload never transits the API servers), an async pipeline transcodes/thumbnails, and all reads are served from a **CDN**. Media is ~95% of bandwidth and should touch your backend ~0 times per view.

**Likes/counters**: increment in Redis (`INCR likes:{post_id}`), flush deltas to the DB every few seconds. Counts are approximate in real time, exact eventually — nobody can verify a like count of 1,002,347 anyway.

**Notifications**: a Kafka consumer on post/like/follow events; batch and rate-limit per recipient ("X and 12 others liked…") to avoid notification fanout becoming a second celebrity problem.

**Search & trending**: another Kafka consumer indexes posts into an inverted index; trending is a streaming count over a sliding window (heavy-hitter sketches, not exact counts).

**Membership checks** ("have I seen this post?", "did I already like this?") at this scale are a fit for [[bloom-filters]] — tiny memory, no false negatives.

## 7. Reliability & scale-out checklist

| Concern | Technique |
|---|---|
| Hot user / hot post | per-entity hot cache + request coalescing; shard by post_id not author_id |
| Cache node loss | Redis cluster with replicas; feed is *rebuildable* from Posts DB + graph (it's a cache, not a source of truth) |
| Fanout lag spike | Kafka absorbs the burst; consumers catch up; feeds are eventually consistent by design |
| Cascading failure | circuit breakers between services, bulkheads per dependency (see [[circuit-breaker-and-bulkhead]]) |
| Thundering herd on cold cache | request coalescing (singleflight), jittered TTLs |
| Multi-region | active-active for reads, home-region writes per user, async cross-region replication (see [[global-service-design]]) |
| DB growth | posts are append-only and time-ordered → LSM-based stores work well (see [[lsm-tree-vs-b-tree]]) |

## 8. Design principles this system demonstrates

1. **Move work from read time to write time** when reads dominate 100:1 — precompute the expensive thing (the feed) once per write instead of once per read.
2. **No rule survives the power law.** Fanout-on-write is right for 99.9% of users and catastrophically wrong for celebrities — the answer is a hybrid, not a winner. Skewed distributions (followers, post popularity) break uniform designs; always ask "what does the 99.999th percentile entity do to this?"
3. **Caches that are rebuildable are not state.** The entire feed layer can be lost and regenerated; that's what lets it live in volatile memory at 8TB scale.
4. **Async everything that can be async.** The only synchronous work in posting is one DB insert; Kafka decouples the other six consumers and absorbs every burst.
5. **Eventual consistency is a feature here, not a compromise** — but carve out read-your-writes for the one place users notice (their own posts).
