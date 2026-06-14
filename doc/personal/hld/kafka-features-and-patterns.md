# Apache Kafka — Features & Patterns at Scale

A personal reference covering Kafka's core features, architecture, and the design patterns
commonly used when operating Kafka at scale (including how some of them apply to this
wallet-transfer project's saga implementation).

---

## 1. What Kafka Is

Apache Kafka is a distributed, append-only **commit log** used as:

- A **messaging system** (pub/sub + queueing semantics via consumer groups)
- A **storage system** (durable, replicated, replayable log)
- A **stream-processing platform** (Kafka Streams, ksqlDB)
- An **integration backbone** (Kafka Connect, CDC pipelines)

---

## 2. Core Concepts & Features

### 2.1 Topics & Partitions
- A **topic** is a named stream of records, split into **partitions**.
- Each partition is an ordered, immutable sequence of records, each with a monotonically
  increasing **offset**.
- **Ordering is guaranteed only within a partition**, not across the topic.
- Partitions are the unit of parallelism: more partitions → more concurrent consumers.
- Records are routed to partitions by **key hash** (same key → same partition → ordered
  per key), round-robin / sticky partitioning when no key is set. With no key, the
  producer simply cycles across the topic's partitions to balance load (sticky fills one
  batch per partition before moving on), with no ordering guarantee between records.

### 2.2 Brokers & Clusters
- A cluster is a set of **brokers**; each broker hosts a subset of partitions.
- One broker acts as the **controller** (manages partition leadership, cluster metadata).
- **KRaft mode** (Kafka 3.3+, default in 4.x) replaces ZooKeeper: metadata is stored in an
  internal Raft-replicated log, simplifying operations and improving controller failover.

### 2.3 Replication & Durability
- Each partition has one **leader** and N−1 **followers** (replication factor, typically 3).
- Followers replicate the leader's log; replicas that are caught up form the **ISR**
  (in-sync replica set).
- `acks` setting on the producer:
  - `acks=0` — fire and forget
  - `acks=1` — leader ack only
  - `acks=all` — ack after all ISR replicas have the record (use with
    `min.insync.replicas=2` for "no data loss on single-broker failure")
- **Unclean leader election** (`unclean.leader.election.enable=false` by default) prevents
  an out-of-sync replica from becoming leader and losing committed data.

### 2.4 Producers
- **Batching** (`batch.size`, `linger.ms`) and **compression** (`lz4`, `zstd`, `snappy`,
  `gzip`) for throughput.
- **Idempotent producer** (`enable.idempotence=true`, default since 3.0): broker
  de-duplicates retries using producer ID + sequence numbers → exactly-once *per partition
  per producer session*.
- **Transactions** (`transactional.id`): atomic writes across multiple partitions/topics,
  plus atomic "consume-transform-produce" with offset commits in the same transaction.
- Custom **partitioners** for routing control.

### 2.5 Consumers & Consumer Groups
- Consumers in a **group** share partitions: each partition is owned by exactly one
  consumer in the group at a time, so each record is processed by only one member of the
  group (queue semantics — work is split).
- Different groups don't compete: each group tracks its own offsets, so every group
  independently reads the entire topic (pub/sub semantics — fan-out). E.g. a record on
  `payments` is processed once by the `settlement-service` group *and* once by the
  `fraud-detection` group.
- **Offsets** are stored in the internal `__consumer_offsets` topic; commit manually
  (at-least-once) or automatically.
- **Rebalancing**: partition ownership reassigns when membership changes.
  - **Cooperative incremental rebalancing** (`CooperativeStickyAssignor`) avoids
    stop-the-world rebalances.
  - **Static membership** (`group.instance.id`) avoids rebalances on transient restarts
    (rolling deploys, pod restarts).
- **KIP-848** (new consumer rebalance protocol, Kafka 4.x): broker-driven, incremental,
  no global sync barrier.
- **Share groups / "Queues for Kafka"** (KIP-932, 4.x): per-record acknowledgement and
  more consumers than partitions — true work-queue semantics.

### 2.6 Delivery Semantics
| Semantics      | How |
|----------------|-----|
| At-most-once   | Commit offsets before processing |
| At-least-once  | Process, then commit (default for most systems; requires idempotent handlers) |
| Exactly-once   | Idempotent producer + transactions (`read_committed` consumers), or at-least-once + idempotent sink |

### 2.7 Retention & Compaction
- **Time/size retention** (`retention.ms`, `retention.bytes`): the log is a replayable
  buffer, not just a queue — consumers can rewind.
- **Log compaction** (`cleanup.policy=compact`): keep the latest record per key forever —
  ideal for changelogs, state snapshots, and table-like topics. Tombstones (null value)
  delete keys.
- **Tiered storage** (KIP-405, production-ready in 3.9+): old segments offloaded to object
  storage (S3/GCS) — effectively infinite retention with small local disks.

### 2.8 Performance Characteristics
- **Sequential disk I/O** + OS page cache → millions of msgs/sec per broker.
- **Zero-copy** transfer (`sendfile`) from page cache to network socket.
- Batched, compressed wire format; consumers fetch in batches.
- Backpressure is natural: consumers pull at their own pace; lag is observable.

### 2.9 Security
- **Encryption**: TLS for client-broker and inter-broker traffic.
- **Authentication**: SASL (SCRAM, OAUTHBEARER, GSSAPI/Kerberos), mTLS.
- **Authorization**: ACLs per principal per resource (topic, group, cluster).
- **Quotas**: produce/fetch byte-rate and request-rate quotas per client/user.

### 2.10 Ecosystem
- **Kafka Connect** — declarative source/sink connectors (JDBC, S3, Elasticsearch,
  Debezium CDC…), with distributed workers, offset tracking, SMTs (single message
  transforms), and dead-letter queues.
- **Kafka Streams** — JVM library for stateful stream processing: KStream/KTable duality,
  windowing, joins, exactly-once, interactive queries, RocksDB-backed local state with
  changelog topics.
- **Schema Registry** (Confluent / Apicurio) — Avro/Protobuf/JSON-Schema with
  compatibility rules (backward/forward/full) enforced at produce time.
- **MirrorMaker 2** — cross-cluster replication with offset translation.
- **Admin API** — programmatic topic/config/ACL management.

---

## 3. Patterns Used at Scale

### 3.1 Messaging & Integration Patterns

#### Event Notification vs. Event-Carried State Transfer
- **Notification**: thin event ("order 123 changed"), consumer calls back for details.
  Simple, but creates read-load coupling.
- **State transfer**: fat event carrying the full (or relevant) state. Consumers build
  local materialized views; no callback needed. Preferred at scale to decouple services.

#### Transactional Outbox
The standard fix for the dual-write problem (DB write + Kafka publish must both happen):
1. Write business state **and** an `outbox` row in the **same DB transaction**.
2. A relay (poller or Debezium CDC) publishes outbox rows to Kafka.
3. Mark/delete published rows.

Guarantees at-least-once publication that is consistent with the database. Consumers must
be idempotent. *(This is the pattern that pairs with the saga in this wallet-transfer
project — debiting a wallet and emitting the `wallet.debited` event must be atomic.)*

#### Saga / Process Manager (Choreography & Orchestration)
Distributed transactions without 2PC:
- **Choreography**: each service reacts to events and emits the next event. Low coupling,
  but flow logic is implicit and hard to trace at scale.
- **Orchestration**: a saga orchestrator sends commands and consumes replies, tracking
  saga state (often in a compacted topic or DB). Explicit, observable, easier to add
  compensation steps (e.g., refund on failed credit).
- Every step needs a **compensating action**; every handler must be **idempotent**
  (Kafka redeliveries are a fact of life).

#### CQRS & Materialized Views
Commands mutate a write model; events flow through Kafka; consumers build read-optimized
projections (Elasticsearch, Redis, denormalized SQL). Compacted topics + Kafka Streams
KTables make rebuilding a view = replaying the topic.

#### Event Sourcing
The event log is the source of truth; current state is a fold over events. Kafka works as
the transport/distribution layer; per-entity state usually lives in a compacted changelog
or a Streams state store. (Pure event sourcing with arbitrary per-entity reads usually
also needs a DB or state store — Kafka alone has no random-access-by-key reads.)

#### Change Data Capture (CDC)
Debezium tails the database WAL/binlog and emits row-level change events. Used at scale
for: cache invalidation, search indexing, data lake ingestion, outbox relays, and
strangler-fig migrations off monolith databases.

#### Dead Letter Queue (DLQ)
Poison messages (deserialization failures, permanently failing handlers) are published to
`<topic>.dlq` with error headers, instead of blocking the partition. Pair with alerting
and a redrive tool.

#### Retry Topics / Delayed Retry (Tiered Retry)
Kafka has no native per-message delay, so the common pattern is tiered retry topics:
`orders.retry.5s` → `orders.retry.1m` → `orders.retry.10m` → `orders.dlq`. A consumer of
a retry topic waits until the record's timestamp + delay before processing. Keeps the main
partition unblocked while retrying transient failures.

#### Request–Reply over Kafka
Command topic + reply topic, correlation ID header, reply-to header. Used for saga
orchestration command/reply flows; not a great fit for low-latency synchronous RPC.

### 3.2 Data Modeling & Partitioning Patterns

#### Keying for Ordering
Choose the partition key = the entity whose events must be ordered (e.g., `wallet_id`,
`order_id`, `user_id`). All events for that entity land on one partition, so consumers see
them in order. Never rely on cross-key ordering.

#### Avoiding Hot Partitions
- Skewed keys (one celebrity user, one giant tenant) overload a single partition/consumer.
- Mitigations (common thread: spread the raw volume first, restore key locality only
  after the data has been shrunk):
  - **Composite keys** — append a stable bucket to the skewed key, e.g.
    `tenant_42:hash(entity_id)%8`, so one big tenant spreads across up to 8 partitions.
    Keeps per-entity ordering, gives up per-tenant ordering.
  - **Salting + re-aggregation** — for a few known-hot keys, append a *random* suffix
    (`post_123:rand(0..15)`) to fan the firehose over 16 partitions; consumers compute
    partial results, and a second stage re-keys by the plain key (`post_123`) to merge
    the partials. The merge step is mandatory — without it you only have fragments.
  - **Two-stage topology** — when skew is unpredictable: stage 1 produces with **no key**
    (perfectly balanced ingest) and pre-aggregates per consumer; stage 2 emits the small
    pre-aggregated records keyed properly to a repartition topic for the final merge.
    The hot key still maps to one partition in stage 2, but now it carries a few partial
    sums per second instead of raw events.

#### Partitioning Examples in Highly Scalable Systems

- **Payments / wallets (this project's shape)** — key by `wallet_id` (or `account_id`).
  All debits/credits for one wallet hit one partition, so balance-affecting events are
  strictly ordered per wallet, while millions of wallets spread evenly across partitions.
  Keying by `transfer_id` instead would break per-wallet ordering (two transfers touching
  the same wallet could interleave on different partitions).
- **Ride-hailing / delivery tracking** — driver location updates keyed by `driver_id` so
  each driver's GPS stream is ordered; trip events keyed by `trip_id` on a separate topic.
  Geo-aggregation jobs then **repartition by geohash cell** to compute per-area supply.
- **Clickstream / analytics ingest** — often **no key at all** (sticky partitioning):
  maximum even spread and throughput, since downstream aggregation re-keys by
  `session_id` or `user_id` in a repartition step anyway.
- **Multi-tenant SaaS** — naive `tenant_id` keying lets one whale tenant saturate a
  partition. Use a composite key `tenant_id:hash(entity_id)` so a big tenant spreads over
  many partitions while ordering is kept at the entity level, where it actually matters.
- **Hot key salting (celebrity problem)** — a viral post's like-events keyed by `post_id`
  melt one partition. Write with `post_id:rand(0..15)` to fan out over 16 partitions,
  pre-aggregate counts per salt in a Streams job, then re-key by plain `post_id` to merge
  partial counts. Trade: per-post ordering is lost, which counters don't need.
- **CDC / changelog topics** — key by primary key of the source row, so the latest update
  per row wins under log compaction and consumers apply row changes in order.
- **IoT fleets** — key by `device_id` for per-device ordering; with millions of devices
  and few partitions this still balances well because the key space is huge and uniform —
  skew problems come from few/skewed keys, not from many keys.

#### Partition Count Planning
- Partitions can be **added** but never removed, and adding partitions **breaks
  key→partition mapping** (ordering for a key changes going forward). Example: with 3
  partitions, `hash("wallet-42") % 3 = 1`, so every event for wallet-42 lands on P1 in
  order. Add a 4th partition and now `hash("wallet-42") % 4 = 3` — new events go to P3
  while the older ones still sit unconsumed on P1. A consumer can process the new
  `wallet.debited` on P3 *before* an earlier one on P1, so per-wallet ordering is broken
  across the resize boundary (it recovers only once all pre-resize events are consumed).

#### Handling Partition Resize in Critical Flows (Wallet-to-Wallet Transfers)

For money movement, partition ordering should be a *performance optimization*, never the
*correctness mechanism* — then a resize can't corrupt balances even if it scrambles order:

- **Make the database the source of truth, events derived.** The wallet balance lives in
  a DB ledger updated transactionally (with the outbox); Kafka events describe what
  already happened. A reordered event can delay a saga step but can never make a balance
  wrong.
- **Per-wallet sequence numbers in the event.** Stamp each event with a monotonically
  increasing `wallet_version` (from the DB row, e.g. an optimistic-lock column). Consumers
  track the last applied version per wallet: on a gap (got v7, expecting v6) they **buffer
  or retry-later** instead of applying; on a duplicate/lower version they skip. This makes
  ordering self-healing regardless of which partition events arrive on.
- **State-machine guards in the saga.** Each transfer is a state machine
  (`INITIATED → DEBITED → CREDITED → COMPLETED`); a handler validates the transition
  before acting. An out-of-order `credit` for a transfer not yet `DEBITED` is parked and
  retried, not executed.
- **Over-provision partitions up front** (e.g. 48–96 for a payments topic) so a resize is
  never needed during normal growth — partitions are cheap relative to this risk.
- **If a resize is unavoidable, drain then switch:** pause producers (or queue at the
  API edge), wait for consumer lag to hit 0 on every partition, add partitions, resume.
  No in-flight pre-resize events → no cross-boundary reordering.
- **Or migrate to a new topic instead of resizing:** create `wallet.events.v2` with the
  target partition count, cut producers over, let consumers fully drain v1 (lag 0) before
  subscribing to v2. Same drain guarantee, plus an easy rollback.
- Size for target throughput ÷ per-partition consumer throughput, plus headroom
  (a common starting point is 2–3× expected need; many orgs standardize on 12/24/48).
- Too many partitions costs: more open files, longer leader elections, more replication
  fetchers, slower controller operations.

#### Schema Evolution Discipline
- Enforce **backward-compatible** changes (add optional fields, never repurpose/rename).
- Contract-first with Schema Registry compatibility checks in CI.
- Topic-per-event-type vs. event-type-per-topic: keep events that must be ordered
  together in one topic (per-entity event streams); otherwise prefer one type per topic
  for independent evolution.

#### Compacted Topics as Tables
`cleanup.policy=compact` topics serve as distributed, replicated key→latest-value tables:
config distribution, entity snapshots, Streams changelogs, connector offsets.

### 3.3 Reliability Patterns

#### Idempotent Consumers
At-least-once delivery means duplicates **will** happen. Standard defenses:
- Natural idempotency (UPSERT by key, set-state-to-X operations).
- **Idempotency keys**: store processed event IDs (in the same DB transaction as the
  business write) and skip already-seen IDs.
- Conditional updates / version checks (optimistic concurrency).

#### Exactly-Once Processing (EOS)
- For Kafka→Kafka pipelines: Kafka Streams `processing.guarantee=exactly_once_v2`
  (transactions wrap state updates + output records + offset commits).
- For Kafka→DB: there is no cross-system transaction — use at-least-once + idempotent
  sink, or store offsets **in the DB** in the same transaction as the write and seek on
  restart.

#### Zombie Fencing
Transactional producers with the same `transactional.id` fence older instances (epoch
bump) — protects sagas/stream jobs from split-brain after a hung instance resumes.

#### Consumer Lag as the Primary Health Signal
Lag (per partition, per group) is *the* SLO metric for async pipelines. Alert on lag
growth rate and time-lag (seconds behind), not just absolute message count. Tools: Burrow,
kafka-lag-exporter, Cruise Control, vendor dashboards.

#### Backpressure & Flow Control
- `max.poll.records` / `max.poll.interval.ms` tuned so the consumer never gets evicted
  mid-batch.
- **Pause/resume** partitions when downstream (DB, HTTP dependency) is saturated rather
  than letting the poll loop time out.
- Bound in-flight work per partition to preserve ordering while parallelizing across
  partitions.

#### Graceful Degradation / Circuit Breaking
When a downstream dependency fails, pause consumption (lag absorbs the backlog) instead of
spinning on errors; resume when the breaker closes. The log *is* the buffer.

### 3.4 Scaling & Operations Patterns

#### Scaling Consumers
- Horizontal scale up to the partition count (extra consumers idle).
- Beyond that: increase partitions (with the key-mapping caveat), batch handlers, or use
  per-partition worker pools with key-level ordering preserved.
- Use **cooperative rebalancing + static membership** to make autoscaling and rolling
  deploys non-disruptive.

#### Cluster Sizing & Balancing
- Keep per-broker partition counts and disk/network utilization balanced — **Cruise
  Control** automates rebalancing, broker addition/removal, and anomaly self-healing.
- **Rack awareness** (`broker.rack`) spreads replicas across AZs; **follower fetching**
  (KIP-392) lets consumers read from the closest replica to cut cross-AZ traffic costs.

#### Multi-Region / Disaster Recovery
- **Stretch cluster** across 3 AZs (synchronous, single region) for HA.
- **MirrorMaker 2 / Cluster Linking** for async cross-region replication
  (active-passive or active-active with topic prefixing and offset translation).
- Define RPO/RTO explicitly: async replication means possible loss of the replication lag
  window on regional failover.

#### Tiered Storage for Infinite Retention
Offload cold segments to S3/GCS: replay months of history, shrink broker disks, faster
broker recovery (less local data). Enables "Kafka as the system of record" patterns.

#### Multi-Tenancy & Governance
- Naming conventions (`<domain>.<entity>.<event>` e.g. `wallet.transfer.completed`),
  ownership metadata, ACLs per team, quotas per client.
- Central schema registry + CI compatibility gates.
- Capacity isolation: per-tenant quotas, or separate clusters for bulk/batch vs.
  latency-sensitive traffic.

#### Topic Lifecycle
- Infrastructure-as-code for topics (Terraform, JulieOps, strimzi `KafkaTopic` CRDs) —
  no hand-created topics in prod; `auto.create.topics.enable=false`.

### 3.5 Stream Processing Patterns
- **Stateless transforms**: filter, map, branch, route.
- **Stateful aggregation**: windowed counts/sums (tumbling, hopping, sliding, session
  windows), with **grace periods** for late/out-of-order events (event-time vs.
  processing-time semantics, watermarks).
- **Stream–table joins**: enrich an event stream against a compacted reference table
  (KTable/GlobalKTable) — the at-scale alternative to per-event DB lookups.
- **Stream–stream joins**: correlate two streams within a time window (e.g., payment
  authorized ⋈ payment captured).
- **Repartition-then-aggregate**: re-key a stream so aggregation is local to a partition.
- **Interactive queries**: serve Streams state stores directly as a read API.

---

## 4. Quick Production Checklist

- [ ] `acks=all`, `min.insync.replicas=2`, RF=3, unclean leader election off
- [ ] Idempotent producer on; transactions where consume-transform-produce needs atomicity
- [ ] Keys chosen for required ordering; hot-key risk assessed
- [ ] Consumers idempotent; offsets committed after processing
- [ ] DLQ + tiered retry topics; poison-message handling tested
- [ ] Outbox pattern wherever DB state + event must be consistent
- [ ] Schema registry with compatibility checks in CI
- [ ] Lag monitoring + alerting (time-lag, growth rate); end-to-end latency SLO
- [ ] Cooperative rebalancing + static membership for deploys/autoscaling
- [ ] Rack-aware replica placement across 3 AZs; DR strategy with stated RPO/RTO
- [ ] Topics managed as code; quotas and ACLs per client

---

## 5. Further Reading

- *Designing Data-Intensive Applications* — Martin Kleppmann (ch. 11, "Stream Processing")
- *Kafka: The Definitive Guide*, 2nd ed. — Shapira, Palino, Sivaram, Petty
- Confluent blog: transactional outbox, exactly-once semantics, KIP-848, KIP-932
- microservices.io — Saga, Outbox, CQRS pattern catalog
