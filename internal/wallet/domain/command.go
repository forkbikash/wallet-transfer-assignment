// Package domain holds the event-sourcing core of the wallet: the Command and
// Event types and the deterministic state machine (Decide + Apply).
//
// A Command is an *intent* from the outside world (validated, may be rejected).
// An Event is a *validated fact* (past tense, deterministic to apply). Per the
// ByteByteGo Ch.28 design, a balance transfer A->C is decomposed by the Saga
// coordinator into per-account leg commands; the command-processor turns each
// valid command into an event on the event log.
package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/Robustrade/wallet-transfer-assignment/common/util/money"
)

// Leg identifies which side of a transfer a command/event represents. It is
// also the second half of the per-transaction idempotency key, so the three
// legs of a transfer never collide.
type Leg string

const (
	// LegDebit deducts from the source account.
	LegDebit Leg = "DEBIT"
	// LegCredit adds to the destination account.
	LegCredit Leg = "CREDIT"
	// LegCompensate refunds the source account after a failed credit (Saga
	// rollback). It credits funds but is a distinct leg so its idempotency key
	// differs from the original debit.
	LegCompensate Leg = "COMPENSATE"
)

// Command is an intent to mutate exactly one account's balance. The Account
// field is the Kafka partition key, giving per-account FIFO ordering.
type Command struct {
	TransactionID uuid.UUID `json:"transaction_id"`
	SagaID        uuid.UUID `json:"saga_id"`
	Leg           Leg       `json:"leg"`
	Account       string    `json:"account"`      // the account this command mutates (partition key)
	Counterparty  string    `json:"counterparty"` // the other side of the transfer (audit context)
	AmountMinor   int64     `json:"amount_minor"`
	Currency      string    `json:"currency"`
	IssuedAt      time.Time `json:"issued_at"`
}

// Key is the idempotency key for this command: a leg of a transaction is
// processed at most once. Used by the command-processor to skip a command it
// has already turned into an event (effectively-once command handling).
func (c Command) Key() string { return c.TransactionID.String() + ":" + string(c.Leg) }

// eventIDNamespace is a fixed UUID namespace for deriving deterministic event
// ids. It never changes (changing it would alter all derived ids).
var eventIDNamespace = uuid.MustParse("c7b2a1d0-0000-4000-8000-000000000001")

// EventID returns the deterministic event id for the event produced from this
// command (UUIDv5 of the command key). Because it is a pure function of the
// command, re-emitting an event for the same command (e.g. a redelivery after a
// crash) yields the SAME id, so the projector and ledger dedup it via their
// event_id uniqueness — effectively-once application without unbounded state.
func (c Command) EventID() uuid.UUID {
	return uuid.NewSHA1(eventIDNamespace, []byte(c.Key()))
}

// Amount returns the typed amount.
func (c Command) Amount() money.Money { return money.FromMinor(c.AmountMinor) }

// Encode serializes the command for Kafka.
func (c Command) Encode() ([]byte, error) { return json.Marshal(c) }

// DecodeCommand parses a command from Kafka bytes.
func DecodeCommand(b []byte) (Command, error) {
	var c Command
	err := json.Unmarshal(b, &c)
	return c, err
}
