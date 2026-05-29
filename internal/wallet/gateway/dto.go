package gateway

import (
	"strings"

	"github.com/google/uuid"

	apperr "github.com/Robustrade/wallet-transfer-assignment/common/error"
	"github.com/Robustrade/wallet-transfer-assignment/common/util/money"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/service"
)

// balanceTransferRequest is the wire body of POST /v1/wallet/balance_transfer.
// Per the chapter, `amount` is a STRING (no float) and `currency` is ISO-4217.
type balanceTransferRequest struct {
	FromAccount   string `json:"from_account"`
	ToAccount     string `json:"to_account"`
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
	TransactionID string `json:"transaction_id"`
}

// validate checks and normalizes the request into a service.Input, or returns
// an AppError mapped to the right HTTP status. This is transport-level
// validation only; business rules live in the service layer.
func (r balanceTransferRequest) validate() (service.Input, error) {
	from := strings.TrimSpace(r.FromAccount)
	to := strings.TrimSpace(r.ToAccount)
	if from == "" || len(from) > 64 || to == "" || len(to) > 64 {
		return service.Input{}, apperr.ErrInvalidWalletID
	}
	if from == to {
		return service.Input{}, apperr.ErrSameWallet
	}

	currency := strings.ToUpper(strings.TrimSpace(r.Currency))
	if len(currency) != 3 || !isAlpha(currency) {
		return service.Input{}, apperr.ErrInvalidCurrency
	}

	amount, err := money.ParseMinor(r.Amount, currency)
	if err != nil || !amount.IsPositive() {
		return service.Input{}, apperr.ErrInvalidAmount
	}

	txID, err := uuid.Parse(strings.TrimSpace(r.TransactionID))
	if err != nil || txID == uuid.Nil {
		// Reject the nil uuid: it parses successfully but is reserved internally
		// (seed/genesis events use a nil SagaID) and is not a valid dedup key.
		return service.Input{}, apperr.ErrInvalidTransactionID
	}

	return service.Input{
		TxID:        txID,
		From:        from,
		To:          to,
		AmountMinor: amount.Minor(),
		Currency:    currency,
	}, nil
}

func isAlpha(s string) bool {
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// transferResponse is the JSON body returned by the transfer + status endpoints.
type transferResponse struct {
	Status        string `json:"status"` // success | failed | pending
	TransactionID string `json:"transaction_id"`
	Reason        string `json:"reason,omitempty"`
}

// balanceResponse is the JSON body returned by the balance query.
type balanceResponse struct {
	Account  string `json:"account"`
	Balance  string `json:"balance"` // formatted with the currency's precision
	Currency string `json:"currency"`
}
