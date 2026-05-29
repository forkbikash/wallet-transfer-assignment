package apperr

import "net/http"

// Sentinel errors specific to the event-sourced wallet (Ch.28 design).
var (
	ErrInvalidCurrency = &AppError{
		Code:       "INVALID_CURRENCY",
		HTTPStatus: http.StatusBadRequest,
		Message:    "currency must be a 3-letter ISO-4217 code",
	}

	ErrInvalidTransactionID = &AppError{
		Code:       "INVALID_TRANSACTION_ID",
		HTTPStatus: http.StatusBadRequest,
		Message:    "transaction_id must be a valid uuid",
	}

	// ErrCurrencyMismatch is a business outcome (the transfer FAILED), not a
	// request error: the source and destination wallets hold different
	// currencies and foreign exchange is out of scope.
	ErrCurrencyMismatch = &AppError{
		Code:       "CURRENCY_MISMATCH",
		HTTPStatus: http.StatusUnprocessableEntity,
		Message:    "source and destination currencies differ",
	}

	// ErrTransferPending is returned (202) when the asynchronous pipeline has
	// not settled before the gateway's push deadline. The client polls the
	// transaction status endpoint to learn the final outcome.
	ErrTransferPending = &AppError{
		Code:       "TRANSFER_PENDING",
		HTTPStatus: http.StatusAccepted,
		Message:    "transfer accepted; settlement in progress",
	}

	// ErrAccountNotFound is returned when a balance query targets an account
	// that has no events yet (does not exist in the read model).
	ErrAccountNotFound = &AppError{
		Code:       "ACCOUNT_NOT_FOUND",
		HTTPStatus: http.StatusNotFound,
		Message:    "account not found",
	}
)
