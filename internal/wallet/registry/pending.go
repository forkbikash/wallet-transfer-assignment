// Package registry implements the in-process pending-request registry behind
// the chapter's push model. The gateway registers a waiter keyed by
// transaction_id BEFORE producing the transfer; when the Saga reaches a terminal
// state it resolves the waiter, waking the blocked HTTP handler so the async
// pipeline feels synchronous.
//
// This only works when the gateway and Saga run in the same process (the `all`
// run-mode, or a co-located deployment). In a split deployment the Saga cannot
// reach this registry, and the gateway instead polls transaction_outcomes — see
// the gateway handler's timeout/fallback path.
package registry

import (
	"sync"

	"github.com/google/uuid"
)

// Outcome is the terminal result delivered to a waiter.
type Outcome struct {
	Success bool
	Reason  string // machine-readable reason when !Success
}

// Registry tracks pending transfer waiters.
type Registry struct {
	mu      sync.Mutex
	waiters map[uuid.UUID]chan Outcome
}

// New constructs an empty Registry.
func New() *Registry {
	return &Registry{waiters: make(map[uuid.UUID]chan Outcome)}
}

// Register returns a channel that will receive the outcome for txID exactly
// once. Must be called before the transfer is produced so the resolve can never
// race ahead of registration. Calling Register twice for the same id returns the
// same channel.
func (r *Registry) Register(txID uuid.UUID) <-chan Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.waiters[txID]; ok {
		return ch
	}
	ch := make(chan Outcome, 1) // buffered so Resolve never blocks
	r.waiters[txID] = ch
	return ch
}

// Resolve delivers the outcome to a waiter (if any) and removes it. Safe to
// call when no waiter is registered (e.g. in a split deployment) — it is a
// no-op then.
func (r *Registry) Resolve(txID uuid.UUID, success bool, reason string) {
	r.mu.Lock()
	ch, ok := r.waiters[txID]
	if ok {
		delete(r.waiters, txID)
	}
	r.mu.Unlock()
	if ok {
		ch <- Outcome{Success: success, Reason: reason}
	}
}

// Cancel drops a waiter without delivering an outcome (e.g. on handler timeout).
func (r *Registry) Cancel(txID uuid.UUID) {
	r.mu.Lock()
	delete(r.waiters, txID)
	r.mu.Unlock()
}
