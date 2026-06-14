# Circuit Breaker & Bulkhead

Two complementary **resilience patterns** for distributed systems. Both protect a service from a misbehaving dependency, but they answer different questions:

> **Circuit breaker:** "Should I even *try* calling this dependency right now?"
>
> **Bulkhead:** "How much of *my* capacity am I willing to spend waiting on this dependency?"

A circuit breaker protects you **in time** (stop calling a dependency that is currently failing). A bulkhead protects you **in space** (cap the resources any one dependency can consume, so one slow dependency can't sink the whole service).

---

## Circuit Breaker

Named after the electrical device: when a circuit is overloaded, the breaker trips and cuts the flow instead of letting the wiring melt.

Without one, a failing downstream service causes the worst possible behavior: every caller waits for a full timeout, threads/connections pile up, and you keep hammering a service that is already struggling — making its recovery *harder* (this is how retry storms turn a blip into an outage).

### The three states

```
                 failures >= threshold
   ┌────────┐ ─────────────────────────► ┌────────┐
   │ CLOSED │                            │  OPEN  │
   │(normal)│ ◄──────────┐               │ (fail  │
   └────────┘            │               │  fast) │
        ▲                │               └────────┘
        │         probe succeeds              │
        │                │                    │ cool-down timer expires
        │           ┌───────────┐             │
        └───────────│ HALF-OPEN │ ◄───────────┘
   probe fails ───► │  (probe)  │
   (back to OPEN)   └───────────┘
```

1. **Closed** — normal operation. Calls pass through; failures are counted (often as a rate over a sliding window, not a raw count).
2. **Open** — the breaker has *tripped*. Calls fail **immediately** without touching the dependency. The caller gets an instant error (or a fallback) instead of a timeout. After a cool-down period, move to half-open.
3. **Half-open** — let a small number of trial requests through. If they succeed, close the breaker; if any fail, snap back to open and restart the cool-down.

### Key design decisions

| Decision | Typical choice |
|---|---|
| Trip condition | failure rate ≥ 50% over a sliding window, with a minimum request volume (e.g. ≥ 20 reqs) |
| What counts as failure | timeouts, 5xx, connection errors — **not** 4xx (a client error says nothing about dependency health) |
| Cool-down (open → half-open) | a few seconds to a minute, often with jitter |
| Half-open probe budget | 1–5 concurrent trial requests |
| On open | fail fast, serve a fallback (cached value, default, queued-for-later) |

Subtleties worth remembering:

- **Minimum volume matters.** 1 failure out of 2 requests is 50% — you don't want to trip on that. Require a minimum sample size.
- **Scope the breaker per dependency** (and often per endpoint/host), not one global breaker. The ledger DB being down shouldn't open the breaker for the notification service.
- **Failing fast is a feature.** An instant error that the caller can handle (retry later, fallback, degrade) is far better than a 30s timeout that holds a goroutine and a connection hostage.

## Bulkhead

Named after the watertight compartments in a ship's hull: if one compartment floods, the bulkheads stop the water from spreading and the ship stays afloat.

In a service, the "water" is **resource exhaustion**: goroutines, connection-pool slots, worker threads, memory. The classic failure mode — one dependency gets slow (not even down, just *slow*), every in-flight request to it parks a goroutine and a DB/HTTP connection, and soon there is nothing left to serve traffic that has *nothing to do with* the slow dependency.

Say a service has 100 goroutines/connections total and calls three dependencies. The fraud-check service becomes slow — every call to it parks a goroutine for seconds instead of milliseconds, so fraud calls accumulate and eat the shared pool:

```
WITHOUT bulkheads — one shared pool of 100:

  shared pool:  [ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff...f|ll|n]
                 └────────────── 97 stuck waiting on fraud-check ──────────────┘ │  │
                                                 ledger squeezed into 2 slots ───┘  │
                                                  notif squeezed into 1 slot ───────┘

  Result: ledger and notification calls — perfectly healthy — queue or time out.
          The WHOLE service is down because ONE dependency is slow.
```

With bulkheads, each dependency gets its own fixed compartment. Fraud calls can only ever occupy their own 30 permits; the flood stops at the compartment wall:

```
WITH bulkheads — pool partitioned per dependency:

  fraud   (30 permits):  [ffffffffffffffffffffffffffffff]  FULL → new fraud calls
                                                                  rejected instantly
  ledger  (50 permits):  [llllllllll....................]  healthy, plenty free
  notif   (20 permits):  [nnn...........................]  healthy, plenty free

  Result: only fraud-check calls fail (fast, with a fallback).
          Transfers and notifications keep working.
```

The implementation is mundane on purpose: **a counting semaphore (or a dedicated small pool) per dependency.** Want to call the fraud service? Acquire one of its 30 permits. None free (or none free within a short wait)? Reject immediately — that's *load shedding*, and it's the correct behavior: better to fail 1% of requests than to brown out 100%.

| Bulkhead flavor | Mechanism |
|---|---|
| Semaphore | cap concurrent calls per dependency (cheapest, most common) |
| Pool partitioning | separate connection pools / worker pools per dependency or tenant |
| Queue + workers | bounded queue in front of a fixed worker pool; full queue ⇒ reject |
| Process/deployment level | separate instances or node pools for critical vs. batch traffic |

Sizing rule of thumb: `permits ≈ expected RPS to that dependency × its p99 latency`, plus headroom. Tight enough to contain a flood, loose enough not to throttle normal traffic.

### "Do I still need bulkheads if every service runs on its own machine?"

Yes — because the bulkhead protects the **caller's** resources, not the dependency's. Machine separation isolates the dependencies *from each other* (fraud-check can't steal CPU or memory from the ledger service), but it does nothing for the shared pool inside *your* process.

Replay the failure mode with separate machines: fraud-check (on its own box) gets slow. Every call to it still parks a goroutine, an HTTP connection, and some memory **in the wallet service** while waiting — all drawn from your one shared pool, regardless of where fraud-check physically runs. 97 goroutines stuck waiting on a remote machine starve ledger and notification calls exactly as before. The flood happens on the **client side**; server-side machine boundaries can't contain it.

The two kinds of isolation are orthogonal:

- **Separate machines/deployments** — stop dependencies from stealing CPU/memory/disk *from each other* (the "process/deployment level" row above).
- **Client-side bulkheads** (semaphores, partitioned pools) — stop one slow dependency from consuming all of the *caller's* concurrency budget.

You need the second whenever a single process fans out to multiple dependencies — i.e. essentially always. The only exception is a fully dedicated caller deployment per dependency (a worker fleet that *only* talks to fraud-check): with no shared pool left to partition, machine separation alone suffices — but that's rare and expensive, while an in-process semaphore gives the same containment for free.

## How they work together

The two patterns cover each other's blind spots:

- A **slow-but-not-failing** dependency never trips a failure-rate breaker — but it *will* drain your goroutine/connection budget. The bulkhead contains it.
- A **fast-failing** dependency doesn't exhaust resources (errors return quickly) — but pointlessly hammering it delays its recovery. The breaker stops that.
- Bonus: a bulkhead-rejection can be counted as a failure by the breaker, so sustained saturation eventually trips the circuit and sheds load even earlier.

Standard composition order for an outbound call:

```
caller → bulkhead (acquire permit) → circuit breaker (closed?) → timeout → actual call
```

In the wallet-transfer context: the transfer saga calls the ledger DB, a fraud-check service, and a notification service. Each gets its own bulkhead + breaker. If fraud-check melts down, fraud calls fail fast (breaker) and can't hold more than N goroutines (bulkhead) — transfers can still degrade gracefully (e.g. queue for async fraud review) while ledger writes and notifications continue untouched.

## Real-life code example (Go)

A minimal, dependency-free implementation of both patterns composed together, as you'd wrap a fraud-check client in this repo. (In production you'd reach for `sony/gobreaker` or `failsafe-go`, but the mechanics fit in a page.)

```go
package resilience

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrBulkheadFull = errors.New("bulkhead: no permits available")
	ErrCircuitOpen  = errors.New("circuit breaker: open")
)

// ---- Bulkhead: a counting semaphore per dependency ----

type Bulkhead struct {
	permits chan struct{}
}

func NewBulkhead(maxConcurrent int) *Bulkhead {
	return &Bulkhead{permits: make(chan struct{}, maxConcurrent)}
}

// Acquire returns immediately with ErrBulkheadFull instead of queueing —
// shedding load fast is the whole point.
func (b *Bulkhead) Acquire() error {
	select {
	case b.permits <- struct{}{}:
		return nil
	default:
		return ErrBulkheadFull
	}
}

func (b *Bulkhead) Release() { <-b.permits }

// ---- Circuit breaker: closed / open / half-open ----

type state int

const (
	closed state = iota
	open
	halfOpen
)

type Breaker struct {
	mu          sync.Mutex
	state       state
	failures    int
	threshold   int           // consecutive failures to trip
	coolDown    time.Duration // open -> half-open
	openedAt    time.Time
	probeInFlight bool
}

func NewBreaker(threshold int, coolDown time.Duration) *Breaker {
	return &Breaker{state: closed, threshold: threshold, coolDown: coolDown}
}

func (cb *Breaker) allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case closed:
		return nil
	case open:
		if time.Since(cb.openedAt) < cb.coolDown {
			return ErrCircuitOpen
		}
		cb.state = halfOpen // cool-down over: allow one probe
		fallthrough
	default: // halfOpen
		if cb.probeInFlight {
			return ErrCircuitOpen
		}
		cb.probeInFlight = true
		return nil
	}
}

func (cb *Breaker) record(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	wasProbe := cb.state == halfOpen
	cb.probeInFlight = false

	if err == nil {
		cb.failures = 0
		cb.state = closed
		return
	}
	if wasProbe {
		cb.trip() // probe failed: straight back to open
		return
	}
	cb.failures++
	if cb.failures >= cb.threshold {
		cb.trip()
	}
}

func (cb *Breaker) trip() {
	cb.state = open
	cb.openedAt = time.Now()
}

// ---- Composition: bulkhead -> breaker -> timeout -> call ----

type Client struct {
	bulkhead *Bulkhead
	breaker  *Breaker
	timeout  time.Duration
}

func NewClient() *Client {
	return &Client{
		bulkhead: NewBulkhead(30),                       // ≈ RPS × p99 + headroom
		breaker:  NewBreaker(5, 10*time.Second),         // 5 consecutive failures, 10s cool-down
		timeout:  2 * time.Second,
	}
}

func (c *Client) Call(ctx context.Context, fn func(context.Context) error) error {
	if err := c.bulkhead.Acquire(); err != nil {
		return err // shed load: caller can fall back or queue for async retry
	}
	defer c.bulkhead.Release()

	if err := c.breaker.allow(); err != nil {
		return err // fail fast: don't touch the struggling dependency
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	err := fn(ctx)
	c.breaker.record(err)
	return err
}
```

Usage in a transfer saga step:

```go
err := fraudClient.Call(ctx, func(ctx context.Context) error {
	return fraudSvc.Check(ctx, transfer)
})
switch {
case errors.Is(err, ErrCircuitOpen), errors.Is(err, ErrBulkheadFull):
	// degrade gracefully: accept the transfer, queue it for async fraud review
	return queueForReview(ctx, transfer)
case err != nil:
	return err // real failure: let the saga compensate
}
```

## Takeaways

- **Circuit breaker** = temporal isolation: stop calling a *failing* dependency, fail fast, let it recover. Three states: closed → open → half-open.
- **Bulkhead** = resource isolation: cap concurrency per dependency so one *slow* dependency can't starve everything else. Usually just a semaphore.
- They are complements, not alternatives — breakers miss slow-but-succeeding calls, bulkheads miss fast-failing ones. Compose them: bulkhead → breaker → timeout → call.
- Always pair with **timeouts** (an unbounded call defeats both patterns) and make rejections actionable: fallback, cached value, or queue for later.
