package domain

// The state machine has two halves, deliberately split per the chapter's
// determinism rule:
//
//   Decide — IMPURE in spirit: it inspects current state to validate a command
//            and decide what fact (event) results. It does no clock/uuid/I/O
//            here (the processor stamps those), but conceptually this is where
//            validation and any non-determinism would live.
//   Apply  — PURE and deterministic: given a balance and an event, it returns
//            the next balance. No clock, no randomness, no I/O. This is the
//            replay function; running it over the event log always reproduces
//            the same balances, which is the basis of reproducibility/audit.

// Balance is an account's state: its materialized balance, established currency
// (empty until the account's first event), and Version = the Seq of the last
// balance-changing event applied.
type Balance struct {
	Account  string
	Minor    int64
	Currency string
	Version  int64
}

// Decision is the outcome of Decide: the event type to emit and, when rejected,
// the machine-readable reason.
type Decision struct {
	Type   EventType
	Reason string
}

// Decide validates a leg command against the current balance and decides the
// resulting fact. It is a pure function of (cmd, cur) — easy to unit test, and
// it never mutates. Business refusals return an EventRejected decision (a fact
// we still record), not a Go error.
func Decide(cmd Command, cur Balance) Decision {
	if cmd.AmountMinor <= 0 {
		return Decision{Type: EventRejected, Reason: ReasonInvalidAmount}
	}
	// Currency must match once an account is established. A brand-new account
	// (Currency == "") adopts the command's currency on its first credit.
	if cur.Currency != "" && cur.Currency != cmd.Currency {
		return Decision{Type: EventRejected, Reason: ReasonCurrencyMismatch}
	}

	switch cmd.Leg {
	case LegDebit:
		if cur.Minor < cmd.AmountMinor {
			return Decision{Type: EventRejected, Reason: ReasonInsufficientFunds}
		}
		return Decision{Type: EventDebited}
	case LegCredit, LegCompensate:
		// Credits (including compensating refunds) always succeed once the
		// currency check passes.
		return Decision{Type: EventCredited}
	default:
		return Decision{Type: EventRejected, Reason: ReasonInvalidAmount}
	}
}

// Apply applies an event to a balance and returns the next balance. PURE and
// deterministic. Rejected (non-balance-changing) events leave the balance
// unchanged. Apply assumes the event has already been ordered/deduped by the
// caller (it does not itself guard against replays).
func Apply(cur Balance, e Event) Balance {
	next := cur
	next.Account = e.Account
	if next.Currency == "" {
		next.Currency = e.Currency
	}
	switch e.Type {
	case EventDebited:
		next.Minor = cur.Minor - e.AmountMinor
		next.Version = e.Seq
	case EventCredited:
		next.Minor = cur.Minor + e.AmountMinor
		next.Version = e.Seq
	default:
		// EventRejected: no balance change, no version bump.
	}
	return next
}
