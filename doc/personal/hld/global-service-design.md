# Designing a Global Service

## What "global" means

A **global service** serves users from multiple geographic regions with low latency, survives the loss of an entire region, and respects regional legal constraints on data. It is more than "deployed in many places" — it is a set of deliberate choices about **routing, data placement, consistency, and failure isolation**.

The three forces that shape every decision:

1. **Speed of light** — a round trip between continents is 100–300 ms. No amount of engineering removes it; you can only move data and compute closer to users.
2. **Partial failure** — with enough regions, something is always broken. The design question is blast radius, not prevention.
3. **Data sovereignty** — laws (GDPR, RBI data-localization, China's PIPL) dictate *where* certain data may live, independent of what is technically optimal.

## What a global service should have

### 1. Global traffic routing

Get the user to the *right* region before the request does any work.

- **GeoDNS / latency-based DNS** (e.g., Route 53): resolve the same hostname to the nearest healthy region. Simple, but DNS caching makes failover slow (TTL-bound).
- **Anycast**: advertise one IP from many edge locations (how Cloudflare, Google front doors, and most CDNs work). Failover is at BGP speed; no client cooperation needed.
- **Global load balancer / front door** with health checks: routes around an unhealthy region automatically and can do weighted shifts for gradual migration.

A request typically flows: client → anycast edge (TLS termination, CDN, WAF) → nearest healthy region → regional load balancer → service.

### 2. Multi-region deployment with failure isolation

- **At least 3 regions** for quorum-based systems (2 can't break ties); active-active where possible.
- **Cell-based architecture**: partition users into self-contained "cells" (a full stack slice). A bad deploy or poison-pill request takes out one cell, not a region. Slack and AWS internal services are built this way.
- **Static stability**: a region must keep serving (possibly degraded) when it cannot reach other regions — don't put a cross-region call on the critical path of every request.
- **No global single point of failure**: a "global control plane" (config, auth, feature flags) must be cached regionally so its outage doesn't take down all regions at once.

### 3. A deliberate data strategy

This is the hard part. Three broad patterns, often mixed within one system:

| Pattern | How it works | Use when |
|---|---|---|
| **Read-local, write-home** | One home region owns each record; replicas everywhere serve reads | Reads dominate; writes tolerate one cross-region hop (user profiles, catalogs) |
| **Active-active / multi-master** | Any region accepts writes; conflicts resolved later (LWW, CRDTs) or avoided | Availability trumps consistency (shopping carts, presence, likes) |
| **Partitioned by user (homing)** | Each user/tenant is pinned to a home region; all their traffic and data live there | Strong consistency needed per user; also satisfies data residency (banking, wallets) |

Key supporting ideas:

- **Consensus-replicated stores** (Spanner, CockroachDB, YugabyteDB) give global strong consistency by paying a quorum round trip per write — great correctness, irreducible latency.
- **Conflict resolution** for multi-master: last-writer-wins (lossy, needs sane clocks), CRDTs (mergeable by construction), or app-level reconciliation.
- **Clocks**: ordering events across regions needs TrueTime-style bounded clocks (Spanner) or hybrid logical clocks; wall clocks alone cannot order cross-region writes.
- **Data residency**: partition data placement by jurisdiction, not just by latency. This often forces the "user homing" pattern regardless of other preferences.

The CAP/PACELC tradeoff (see [cap-theorem.md](cap-theorem.md)) is decided **per data type, not per system**: a balance transfer is CP and stays in the user's home region; a notification feed is AP and replicated everywhere.

### 4. Asynchronous cross-region communication

Synchronous cross-region RPC on the hot path multiplies latency and couples failure domains. Prefer:

- **Event replication**: regional Kafka clusters with MirrorMaker 2 / Confluent Replicator shipping topics across regions asynchronously.
- **Outbox + replication** rather than dual writes, so regional databases and event streams stay consistent.
- **Idempotent consumers everywhere** — cross-region delivery is at-least-once, duplicated, and reordered by design.

### 5. Global operational machinery

- **Progressive, region-by-region deploys** (one region → bake → next), never all regions at once; config changes are deploys too — most global outages are bad config pushed globally.
- **Observability with global aggregation**: per-region metrics/traces rolled up centrally, but alerting must still work when the central system is unreachable.
- **Regularly exercised region evacuation**: failover that isn't drilled doesn't work. Measure RTO (time to recover) and RPO (data loss window) and test them.
- **Capacity for N-1**: every region pair must absorb a failed neighbor's traffic, which means running well below full utilization everywhere.
- **Global identity & authz** that validates locally (signed tokens like JWTs verified with regionally cached keys — no auth call across the ocean per request).

## How a request actually works, end to end

A representative flow for a user in Mumbai using a service homed in `ap-south-1`:

1. **DNS/anycast** resolves to the nearest edge POP (a few ms away). TLS terminates there; static content is served from CDN cache.
2. The edge forwards the API call to the **nearest region** over the provider's backbone (faster and more reliable than public internet).
3. A **routing layer** checks the user's home region. If this region is the user's home, handle locally. If not — say the user is traveling in Europe — either proxy to the home region or serve a read-only/stale view, depending on the endpoint's consistency needs.
4. The service does all its work against **region-local** databases, caches, and queues. No cross-region call on the critical path.
5. State changes are published to the regional event stream and **replicated asynchronously** to other regions for read replicas, analytics, and disaster recovery.
6. If `ap-south-1` fails entirely: health checks fail, the global router drains traffic to the next region, and that region serves the user — either from replicated data (RPO seconds) or by promoting a standby database. The user notices elevated latency, not an outage.

## A minimal checklist

- [ ] Global routing layer with health-based failover (GeoDNS/anycast + global LB)
- [ ] ≥3 regions, active-active or tested active-passive
- [ ] Per-data-type consistency decision (CP vs AP), written down
- [ ] User/tenant homing strategy, including data-residency mapping
- [ ] No synchronous cross-region call on any hot path
- [ ] Async replication with idempotent consumers and an outbox
- [ ] Cell-based or otherwise bounded blast radius
- [ ] Region-by-region deploys, config included
- [ ] Drilled region failover with measured RTO/RPO
- [ ] N-1 capacity headroom
- [ ] Locally verifiable auth tokens

## Relation to this codebase

This wallet-transfer service is single-region, but its design is already the right substrate for going global:

- **The saga pattern over Kafka** is exactly the asynchronous, compensating style that survives cross-region replication lag — sagas don't need a global lock, so steps can execute in a user's home region and replicate outward.
- **Wallets are natural homing keys**: pin each wallet to a home region (also solving data residency for money movement), execute the debit/credit saga there, and replicate events to other regions via MirrorMaker for reads and DR.
- **Idempotency keys on transfers** (already required for at-least-once Kafka delivery) are the same mechanism that makes cross-region replay and failover safe.
- The main new work going global would be: a routing layer that maps wallet → home region, cross-region topic replication, and a drilled failover story for promoting a region's standby Postgres.

## Further reading

- *Designing Data-Intensive Applications* (Kleppmann) — ch. 5 (replication), ch. 9 (consistency & consensus)
- Spanner paper (Google, 2012) — TrueTime and globally consistent transactions
- AWS Builders' Library — "Static stability using Availability Zones", "Workload isolation using shuffle-sharding"
- Related notes: [cap-theorem.md](cap-theorem.md), [consistent-hashing.md](consistent-hashing.md), [raft-consensus-algorithm.md](raft-consensus-algorithm.md), [kafka-features-and-patterns.md](kafka-features-and-patterns.md)
