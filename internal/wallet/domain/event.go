package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/Robustrade/wallet-transfer-assignment/common/util/money"
)

// EventType is the kind of fact an event records.
type EventType string

const (
	// EventDebited : funds were deducted from Account (balance-changing).
	EventDebited EventType = "ACCOUNT_DEBITED"
	// EventCredited : funds were added to Account (balance-changing). Both a
	// normal destination credit and a compensating refund use this type.
	EventCredited EventType = "ACCOUNT_CREDITED"
	// EventRejected : a leg command was refused (e.g. insufficient funds).
	// Not balance-changing; recorded for audit and to drive the Saga.
	EventRejected EventType = "TRANSFER_REJECTED"
)

// Rejection reasons (also surfaced to clients).
const (
	ReasonInsufficientFunds = "INSUFFICIENT_FUNDS"
	ReasonCurrencyMismatch  = "CURRENCY_MISMATCH"
	ReasonInvalidAmount     = "INVALID_AMOUNT"
)

// Event is an immutable, validated fact appended to the Kafka event log (the
// system's source of truth). Applying events through Apply reconstructs state
// deterministically, which is what makes the system reproducible/auditable.
type Event struct {
	EventID       uuid.UUID `json:"event_id"`
	TransactionID uuid.UUID `json:"transaction_id"`
	SagaID        uuid.UUID `json:"saga_id"`
	Leg           Leg       `json:"leg"`
	Type          EventType `json:"type"`
	Account       string    `json:"account"` // partition key; the account this event mutates
	AmountMinor   int64     `json:"amount_minor"`
	Currency      string    `json:"currency"`
	Reason        string    `json:"reason,omitempty"` // set for EventRejected
	Seq           int64     `json:"seq"`              // per-account monotonic order of balance-changing events
	OccurredAt    time.Time `json:"occurred_at"`
}

// Amount returns the typed amount.
func (e Event) Amount() money.Money { return money.FromMinor(e.AmountMinor) }

// ChangesBalance reports whether applying this event mutates a balance.
func (e Event) ChangesBalance() bool {
	return e.Type == EventDebited || e.Type == EventCredited
}

// CommandKey is the idempotency key of the command that produced this event.
// The command-processor uses it on replay to avoid re-emitting an event for a
// command it already handled before a crash.
func (e Event) CommandKey() string { return e.TransactionID.String() + ":" + string(e.Leg) }

// Encode serializes the event for Kafka.
func (e Event) Encode() ([]byte, error) { return json.Marshal(e) }

// DecodeEvent parses an event from Kafka bytes.
func DecodeEvent(b []byte) (Event, error) {
	var e Event
	err := json.Unmarshal(b, &e)
	return e, err
}
