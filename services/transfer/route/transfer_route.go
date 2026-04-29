// Package route registers the HTTP routes for the transfer service.
package route

import (
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/Robustrade/wallet-transfer-assignment/common/middleware"
	"github.com/Robustrade/wallet-transfer-assignment/services/transfer/internal/handler"
)

// MaxRequestBodyBytes caps the size of any request body. The transfer payload
// is a few hundred bytes; 64 KiB is a generous ceiling.
const MaxRequestBodyBytes int64 = 64 * 1024

// InitRoutes attaches the transfer service routes (and shared middleware)
// to the supplied router.
func InitRoutes(router *mux.Router, h *handler.TransferHandler, logger *slog.Logger) {
	router.Use(middleware.RequestID)
	router.Use(middleware.Recovery(logger))
	router.Use(middleware.Logging(logger))
	router.Use(middleware.MaxBodyBytes(MaxRequestBodyBytes))

	// JSON 404 / 405 instead of gorilla/mux's plain-text defaults.
	router.NotFoundHandler = middleware.NotFoundHandler()
	router.MethodNotAllowedHandler = middleware.MethodNotAllowedHandler()

	router.HandleFunc("/transfers", h.CreateTransfer).Methods(http.MethodPost)
}
