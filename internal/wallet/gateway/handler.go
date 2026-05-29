// Package gateway is the HTTP transport layer (the chapter's "reverse proxy").
// Handlers are thin: they validate/decode the request, delegate to the service
// layer, and map the result (or a typed AppError) to an HTTP response. All
// business logic — idempotency, producing onto the log, the push-model wait —
// lives in internal/wallet/service.
package gateway

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	apperr "github.com/Robustrade/wallet-transfer-assignment/common/error"
	"github.com/Robustrade/wallet-transfer-assignment/common/middleware"
	"github.com/Robustrade/wallet-transfer-assignment/common/util/money"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/service"
)

// Handler serves the wallet HTTP API by delegating to the service layer.
type Handler struct {
	svc service.TransferService
}

// NewHandler constructs a Handler over a TransferService.
func NewHandler(svc service.TransferService) *Handler {
	return &Handler{svc: svc}
}

// CreateTransfer handles POST /v1/wallet/balance_transfer.
func (h *Handler) CreateTransfer(w http.ResponseWriter, r *http.Request) {
	var body balanceTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			middleware.WriteError(w, r, apperr.ErrPayloadTooLarge)
			return
		}
		middleware.WriteError(w, r, apperr.ErrBadJSON)
		return
	}
	in, err := body.validate()
	if err != nil {
		middleware.WriteError(w, r, err)
		return
	}
	res, err := h.svc.Transfer(r.Context(), in)
	if err != nil {
		middleware.WriteError(w, r, err)
		return
	}
	writeResult(w, in.TxID, res)
}

// GetBalance handles GET /v1/wallet/{account}/balance (the CQRS read side).
func (h *Handler) GetBalance(w http.ResponseWriter, r *http.Request) {
	bal, err := h.svc.Balance(r.Context(), mux.Vars(r)["account"])
	if err != nil {
		middleware.WriteError(w, r, err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, balanceResponse{
		Account:  bal.Account,
		Balance:  money.FromMinor(bal.Minor).StringFor(bal.Currency),
		Currency: bal.Currency,
	})
}

// GetTransaction handles GET /v1/wallet/transaction/{transaction_id} — the poll
// endpoint for the 202 fallback path.
func (h *Handler) GetTransaction(w http.ResponseWriter, r *http.Request) {
	txID, err := uuid.Parse(mux.Vars(r)["transaction_id"])
	if err != nil {
		middleware.WriteError(w, r, apperr.ErrInvalidTransactionID)
		return
	}
	res, err := h.svc.Status(r.Context(), txID)
	if err != nil {
		middleware.WriteError(w, r, err)
		return
	}
	writeResult(w, txID, res)
}

// writeResult maps a service.Result to an HTTP status + JSON body. success ->
// 200, failed -> 422 (a business outcome), pending -> 202 (poll for settlement).
func writeResult(w http.ResponseWriter, txID uuid.UUID, res service.Result) {
	status := http.StatusOK
	switch res.State {
	case service.StateFailed:
		status = http.StatusUnprocessableEntity
	case service.StatePending:
		status = http.StatusAccepted
	}
	middleware.WriteJSON(w, status, transferResponse{
		Status:        string(res.State),
		TransactionID: txID.String(),
		Reason:        res.Reason,
	})
}
