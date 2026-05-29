package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/Robustrade/wallet-transfer-assignment/common/util/money"
)

// TransferRequest is the durable hand-off from the gateway to the Saga
// coordinator (the wallet.transfers topic). It is the client's validated intent
// to move funds; the Saga turns it into ordered debit/credit leg commands.
type TransferRequest struct {
	TransactionID uuid.UUID `json:"transaction_id"`
	FromAccount   string    `json:"from_account"`
	ToAccount     string    `json:"to_account"`
	AmountMinor   int64     `json:"amount_minor"`
	Currency      string    `json:"currency"`
	RequestedAt   time.Time `json:"requested_at"`
}

// Amount returns the typed amount.
func (t TransferRequest) Amount() money.Money { return money.FromMinor(t.AmountMinor) }

// Encode serializes the request for Kafka.
func (t TransferRequest) Encode() ([]byte, error) { return json.Marshal(t) }

// DecodeTransferRequest parses a request from Kafka bytes.
func DecodeTransferRequest(b []byte) (TransferRequest, error) {
	var t TransferRequest
	err := json.Unmarshal(b, &t)
	return t, err
}
