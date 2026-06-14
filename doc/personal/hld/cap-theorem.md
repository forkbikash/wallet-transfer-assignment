# CAP Theorem

## What it says

In a distributed data system, when a **network partition** occurs, you must choose between **consistency** and **availability**. You cannot have both.

Formulated by Eric Brewer (2000), formally proven by Gilbert and Lynch (2002).

## The three properties

- **C — Consistency**: Every read receives the most recent write or an error. All nodes see the same data at the same time (linearizability — note this is *not* the same "C" as in ACID).
- **A — Availability**: Every request to a non-failing node receives a non-error response, without guaranteeing it contains the most recent write.
- **P — Partition tolerance**: The system continues to operate despite network partitions (messages between nodes being dropped or delayed).

## The common misreading

CAP is often presented as "pick any two of three." This is misleading. In any real distributed system, **partitions will happen** — networks are unreliable. So P is not optional; the real choice is what to sacrifice *when* a partition occurs:

- **CP**: During a partition, refuse some requests (lose availability) to stay consistent. Examples: etcd, ZooKeeper, HBase, MongoDB (default config).
- **AP**: During a partition, keep serving requests (stay available) but allow stale or divergent reads, reconciling later. Examples: Cassandra, DynamoDB, CouchDB, DNS.

A "CA" system only exists in the absence of partitions — i.e., a single-node system or one that simply stops working when the network fails.

## When there is no partition

CAP says nothing about the normal case. That gap is addressed by **PACELC** (Abadi, 2010):

> If there is a **P**artition, choose **A**vailability or **C**onsistency; **E**lse, choose **L**atency or **C**onsistency.

Even with a healthy network, synchronous replication for strong consistency costs latency. Examples:

- **PA/EL**: Cassandra, DynamoDB — favor availability and low latency.
- **PC/EC**: etcd, ZooKeeper, spanner-style systems — favor consistency throughout.

## Consistency is a spectrum

CAP's "C" is the strongest level (linearizability), but real systems offer many intermediate levels:

| Level | Guarantee |
|---|---|
| Linearizable | Reads always reflect the latest write, globally ordered |
| Sequential | All nodes see operations in the same order |
| Causal | Causally related operations are seen in order |
| Read-your-writes | A client always sees its own writes |
| Eventual | Replicas converge if writes stop |

Many AP systems are tunable: Cassandra's `QUORUM` reads/writes (R + W > N) give strong consistency per-key at the cost of availability during partitions.

## Practical takeaways

1. **Partition tolerance is mandatory** in any multi-node system; design for what happens during a partition, not whether one happens.
2. **The choice is per-operation, not per-system.** A wallet balance check might tolerate staleness (AP), while a funds transfer needs strong consistency (CP). Systems like this one (wallet transfers via saga pattern over Kafka) effectively choose availability with *eventual* consistency, restoring correctness through compensating transactions rather than global locks.
3. **Quorums let you tune the tradeoff**: with N replicas, requiring W writes + R reads where W + R > N guarantees read-after-write consistency.
4. **CAP is about a single data item** under partition — it doesn't directly model cross-service workflows, multi-key transactions, or latency. Use PACELC and consistency models for the fuller picture.

## Relation to this codebase

The saga pattern used here is itself a CAP-driven design: instead of a distributed ACID transaction (which would block — sacrificing availability — whenever any participant is unreachable), each local step commits independently and failures are undone with compensating actions. The system stays available and converges to a consistent state eventually.

## Further reading

- Eric Brewer, *CAP Twelve Years Later: How the "Rules" Have Changed* (2012)
- Gilbert & Lynch, *Brewer's Conjecture and the Feasibility of Consistent, Available, Partition-Tolerant Web Services* (2002)
- Daniel Abadi, *Consistency Tradeoffs in Modern Distributed Database System Design* (PACELC, 2012)
- Martin Kleppmann, *Designing Data-Intensive Applications*, ch. 9
