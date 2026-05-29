package gateway

import (
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/Robustrade/wallet-transfer-assignment/common/middleware"
)

// maxBodyBytes bounds the request body; transfer payloads are tiny.
const maxBodyBytes = 64 << 10

// InitRoutes registers the wallet HTTP API and the shared middleware chain.
func InitRoutes(router *mux.Router, h *Handler, logger *slog.Logger) {
	router.Use(middleware.RequestID)
	router.Use(middleware.Recovery(logger))
	router.Use(middleware.Logging(logger))
	router.Use(middleware.MaxBodyBytes(maxBodyBytes))

	router.NotFoundHandler = middleware.NotFoundHandler()
	router.MethodNotAllowedHandler = middleware.MethodNotAllowedHandler(nil)

	router.HandleFunc("/v1/wallet/balance_transfer", h.CreateTransfer).Methods(http.MethodPost)
	// Registered before the {account} route so "transaction" is not captured.
	router.HandleFunc("/v1/wallet/transaction/{transaction_id}", h.GetTransaction).Methods(http.MethodGet)
	router.HandleFunc("/v1/wallet/{account}/balance", h.GetBalance).Methods(http.MethodGet)
}
