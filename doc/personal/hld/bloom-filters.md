# Bloom Filters

A **Bloom filter** is a space-efficient, probabilistic data structure that answers one question:

> "Have I possibly seen this item before?"

It can return two answers:

- **"Definitely not"** — the item was never added (100% certain, no false negatives).
- **"Possibly yes"** — the item *might* have been added (small chance of a false positive).

Because it never stores the items themselves — only a fixed-size bit array — a Bloom filter can represent millions of items in a few megabytes of memory.

## How it works

A Bloom filter consists of:

1. A bit array of `m` bits, all initially `0`.
2. `k` independent hash functions, each mapping an item to a position in `[0, m)`.

**Add(item):** hash the item with all `k` hash functions and set those `k` bit positions to `1`.

**Contains(item):** hash the item with all `k` hash functions. If **any** of the `k` bits is `0`, the item was definitely never added. If **all** are `1`, the item was *probably* added — but those bits might have been set by other items (that's the false positive case).

```
Add("alice")                    Add("bob")
hashes -> {2, 5, 11}            hashes -> {5, 8, 14}

bit array (m=16):
index:  0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15
value:  0 0 1 0 0 1 0 0 1 0 0  1  0  0  1  0

Contains("carol") -> hashes to {3, 5, 11}
bit 3 is 0  =>  DEFINITELY NOT present

Contains("dave")  -> hashes to {2, 8, 14}
all bits are 1  =>  POSSIBLY present (false positive! dave was never added)
```

### Key properties

| Property | Value |
|---|---|
| False negatives | Never |
| False positives | Possible, tunable |
| Deletions | Not supported (use a Counting Bloom filter) |
| Space | O(m) bits, independent of item size |
| Add / Lookup | O(k) — constant time |

### Sizing the filter

For `n` expected items and a target false-positive rate `p`:

- Optimal bits: `m = -n·ln(p) / (ln 2)²`
- Optimal hash count: `k = (m/n)·ln 2`

Example: 1 million items at 1% false-positive rate needs `m ≈ 9.6M bits ≈ 1.2 MB` and `k ≈ 7` hashes. Storing 1M strings in a hash set would take tens of MB.

## Real-life use cases

- **Databases (Cassandra, RocksDB, PostgreSQL):** before reading an SSTable/page from disk, check a Bloom filter to skip files that definitely don't contain the key. Saves expensive disk I/O.
- **CDNs (Akamai):** only cache an object on its *second* request. A Bloom filter remembers "seen once" URLs, avoiding caching one-hit wonders (~75% of requests).
- **Browsers:** Chrome historically used a Bloom filter for Safe Browsing — check a URL locally first; only call Google's servers on a "possibly malicious" hit.
- **Spell checkers / weak-password lists:** compact membership check against a large dictionary.
- **Payment / wallet systems:** fast pre-check for duplicate transaction IDs or idempotency keys before hitting the database — exactly the kind of dedup a transfer service like this repo needs.

## Real-life code example (Go)

A common production scenario in a wallet/payment service: **rejecting duplicate transfer requests (idempotency keys) without hammering the database**. The Bloom filter sits in front of the DB — if it says "definitely new", we skip the dedup query entirely.

This implementation uses the standard double-hashing trick (`h_i = h1 + i·h2`) so we only compute two real hashes regardless of `k`.

```go
package bloom

import (
	"hash/fnv"
	"math"
)

// Filter is a basic Bloom filter. Not safe for concurrent writes;
// wrap with a sync.RWMutex if shared across goroutines.
type Filter struct {
	bits []uint64 // bit array packed into uint64 words
	m    uint64   // number of bits
	k    uint64   // number of hash functions
}

// New creates a filter sized for n expected items with
// false-positive probability p (e.g. 0.01 for 1%).
func New(n uint64, p float64) *Filter {
	m := uint64(math.Ceil(-float64(n) * math.Log(p) / (math.Ln2 * math.Ln2)))
	k := uint64(math.Round((float64(m) / float64(n)) * math.Ln2))
	if k < 1 {
		k = 1
	}
	return &Filter{
		bits: make([]uint64, (m+63)/64),
		m:    m,
		k:    k,
	}
}

// hashes returns two independent hash values used to derive k indexes
// via double hashing: index_i = h1 + i*h2 (mod m).
func (f *Filter) hashes(data []byte) (uint64, uint64) {
	h := fnv.New64a()
	h.Write(data)
	h1 := h.Sum64()

	h.Write([]byte{0xff}) // perturb to get a second independent hash
	h2 := h.Sum64()
	if h2%2 == 0 {
		h2++ // keep h2 odd so it cycles through all m positions
	}
	return h1, h2
}

// Add inserts an item into the filter.
func (f *Filter) Add(data []byte) {
	h1, h2 := f.hashes(data)
	for i := uint64(0); i < f.k; i++ {
		idx := (h1 + i*h2) % f.m
		f.bits[idx/64] |= 1 << (idx % 64)
	}
}

// Contains reports whether the item is possibly in the filter.
// false  => definitely never added
// true   => probably added (small false-positive chance)
func (f *Filter) Contains(data []byte) bool {
	h1, h2 := f.hashes(data)
	for i := uint64(0); i < f.k; i++ {
		idx := (h1 + i*h2) % f.m
		if f.bits[idx/64]&(1<<(idx%64)) == 0 {
			return false
		}
	}
	return true
}
```

### Using it for transfer idempotency

```go
package main

import (
	"errors"
	"fmt"

	"yourapp/bloom"
)

var ErrDuplicate = errors.New("duplicate transfer request")

type TransferService struct {
	seen *bloom.Filter
	db   *DB // your real storage
}

func NewTransferService(db *DB) *TransferService {
	// Expect up to 10M transfer IDs, 0.1% false-positive rate (~17 MB).
	return &TransferService{
		seen: bloom.New(10_000_000, 0.001),
		db:   db,
	}
}

func (s *TransferService) HandleTransfer(idempotencyKey string, amount int64) error {
	key := []byte(idempotencyKey)

	// Fast path: filter says "definitely new" -> skip the dedup DB query.
	if s.seen.Contains(key) {
		// Possibly a duplicate — confirm against the source of truth,
		// because this could be a false positive.
		dup, err := s.db.TransferExists(idempotencyKey)
		if err != nil {
			return err
		}
		if dup {
			return ErrDuplicate
		}
	}

	if err := s.db.SaveTransfer(idempotencyKey, amount); err != nil {
		return err
	}
	s.seen.Add(key)
	fmt.Println("transfer accepted:", idempotencyKey)
	return nil
}
```

The pattern to notice: the Bloom filter is **never the source of truth**. A "possibly seen" answer triggers a real DB check; a "definitely not seen" answer safely skips it. With a 0.1% false-positive rate, ~99.9% of genuinely new requests avoid the dedup query.

### Quick sanity test

```go
func main() {
	f := bloom.New(1000, 0.01)

	f.Add([]byte("txn-001"))
	f.Add([]byte("txn-002"))

	fmt.Println(f.Contains([]byte("txn-001"))) // true
	fmt.Println(f.Contains([]byte("txn-999"))) // false (almost certainly)
}
```

## When NOT to use a Bloom filter

- You need to **delete** items → use a Counting Bloom filter or a Cuckoo filter.
- You need to **enumerate** stored items → it stores bits, not items.
- The set is small → a plain `map[string]struct{}` is simpler and exact.
- A false positive is unacceptable *and* you can't afford the follow-up exact check.

## Further reading

- Burton Bloom's original 1970 paper: *Space/Time Trade-offs in Hash Coding with Allowable Errors*
- Production-grade Go library: [`github.com/bits-and-blooms/bloom`](https://github.com/bits-and-blooms/bloom)
- Variants: Counting Bloom filters (deletion), Cuckoo filters (deletion + better space), Scalable Bloom filters (unknown n)
