package domain

import (
	"testing"

	"github.com/google/uuid"
)

func TestDecide(t *testing.T) {
	tests := []struct {
		name   string
		cmd    Command
		cur    Balance
		want   EventType
		reason string
	}{
		{
			name: "credit new account establishes currency",
			cmd:  Command{Leg: LegCredit, AmountMinor: 100, Currency: "USD"},
			cur:  Balance{},
			want: EventCredited,
		},
		{
			name: "debit with sufficient funds",
			cmd:  Command{Leg: LegDebit, AmountMinor: 30, Currency: "USD"},
			cur:  Balance{Minor: 100, Currency: "USD"},
			want: EventDebited,
		},
		{
			name:   "debit with insufficient funds is rejected",
			cmd:    Command{Leg: LegDebit, AmountMinor: 200, Currency: "USD"},
			cur:    Balance{Minor: 100, Currency: "USD"},
			want:   EventRejected,
			reason: ReasonInsufficientFunds,
		},
		{
			name:   "currency mismatch is rejected",
			cmd:    Command{Leg: LegCredit, AmountMinor: 50, Currency: "EUR"},
			cur:    Balance{Minor: 100, Currency: "USD"},
			want:   EventRejected,
			reason: ReasonCurrencyMismatch,
		},
		{
			name:   "non-positive amount is rejected",
			cmd:    Command{Leg: LegDebit, AmountMinor: 0, Currency: "USD"},
			cur:    Balance{Minor: 100, Currency: "USD"},
			want:   EventRejected,
			reason: ReasonInvalidAmount,
		},
		{
			name: "compensating credit succeeds",
			cmd:  Command{Leg: LegCompensate, AmountMinor: 30, Currency: "USD"},
			cur:  Balance{Minor: 70, Currency: "USD"},
			want: EventCredited,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.cmd, tc.cur)
			if got.Type != tc.want {
				t.Fatalf("Decide type = %s, want %s", got.Type, tc.want)
			}
			if got.Reason != tc.reason {
				t.Fatalf("Decide reason = %q, want %q", got.Reason, tc.reason)
			}
		})
	}
}

func TestApply(t *testing.T) {
	base := Balance{Account: "A", Minor: 100, Currency: "USD", Version: 1}

	debited := Apply(base, Event{Type: EventDebited, Account: "A", AmountMinor: 40, Currency: "USD", Seq: 2})
	if debited.Minor != 60 || debited.Version != 2 {
		t.Fatalf("debited = %+v, want minor 60 version 2", debited)
	}

	credited := Apply(base, Event{Type: EventCredited, Account: "A", AmountMinor: 25, Currency: "USD", Seq: 2})
	if credited.Minor != 125 {
		t.Fatalf("credited minor = %d, want 125", credited.Minor)
	}

	rejected := Apply(base, Event{Type: EventRejected, Account: "A", AmountMinor: 40, Reason: ReasonInsufficientFunds})
	if rejected.Minor != 100 || rejected.Version != 1 {
		t.Fatalf("rejected = %+v, want unchanged (minor 100 version 1)", rejected)
	}
}

// TestApplyDeterministic is the reproducibility guarantee: folding the same
// event sequence twice yields identical state. This is what lets us replay the
// event log to reconstruct balances.
func TestApplyDeterministic(t *testing.T) {
	events := []Event{
		{Type: EventCredited, Account: "A", AmountMinor: 100, Currency: "USD", Seq: 1},
		{Type: EventDebited, Account: "A", AmountMinor: 30, Currency: "USD", Seq: 2},
		{Type: EventRejected, Account: "A", AmountMinor: 1000, Reason: ReasonInsufficientFunds},
		{Type: EventCredited, Account: "A", AmountMinor: 5, Currency: "USD", Seq: 3},
	}
	fold := func() Balance {
		var b Balance
		for _, e := range events {
			b = Apply(b, e)
		}
		return b
	}
	first, second := fold(), fold()
	if first != second {
		t.Fatalf("replay not deterministic: %+v vs %+v", first, second)
	}
	if first.Minor != 75 || first.Version != 3 || first.Currency != "USD" {
		t.Fatalf("final balance = %+v, want minor 75 version 3 USD", first)
	}
}

// ensure CommandKey/event correlation is stable (used for command dedup).
func TestCommandKeyMatchesEvent(t *testing.T) {
	tx := uuid.New()
	cmd := Command{TransactionID: tx, Leg: LegDebit}
	evt := Event{TransactionID: tx, Leg: LegDebit}
	if cmd.Key() != evt.CommandKey() {
		t.Fatalf("command key %q != event command key %q", cmd.Key(), evt.CommandKey())
	}
}

// EventID must be deterministic per (transaction, leg) so a re-emitted event is
// deduped downstream, and distinct across legs of the same transaction.
func TestCommandEventIDDeterministic(t *testing.T) {
	tx := uuid.New()
	a := Command{TransactionID: tx, Leg: LegDebit}
	b := Command{TransactionID: tx, Leg: LegDebit}
	if a.EventID() != b.EventID() {
		t.Fatal("EventID must be identical for the same command")
	}
	if a.EventID() == (Command{TransactionID: tx, Leg: LegCredit}).EventID() {
		t.Fatal("EventID must differ across legs of the same transaction")
	}
	if a.EventID() == (Command{TransactionID: uuid.New(), Leg: LegDebit}).EventID() {
		t.Fatal("EventID must differ across transactions")
	}
}
