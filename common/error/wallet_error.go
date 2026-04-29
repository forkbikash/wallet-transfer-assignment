package apperr

import "net/http"

// Wallet-related sentinel errors.
var (
	ErrWalletNotFound = &AppError{
		Code:       "WALLET_NOT_FOUND",
		HTTPStatus: http.StatusNotFound,
		Message:    "wallet not found",
	}

	ErrInsufficientFunds = &AppError{
		Code:       "INSUFFICIENT_FUNDS",
		HTTPStatus: http.StatusUnprocessableEntity,
		Message:    "insufficient funds",
	}

	ErrCurrencyMismatch = &AppError{
		Code:       "CURRENCY_MISMATCH",
		HTTPStatus: http.StatusUnprocessableEntity,
		Message:    "wallet currencies differ",
	}
)
