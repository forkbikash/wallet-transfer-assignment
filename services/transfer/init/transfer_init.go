// Package transferinit wires up the transfer service: it constructs the
// repositories, transaction manager, service implementation, and HTTP handler,
// and registers the routes on the supplied router.
package transferinit

import (
	"log/slog"

	"github.com/gorilla/mux"
	"gorm.io/gorm"

	"github.com/Robustrade/wallet-transfer-assignment/services/transfer/internal/handler"
	"github.com/Robustrade/wallet-transfer-assignment/services/transfer/internal/repo/postgres"
	svcimpl "github.com/Robustrade/wallet-transfer-assignment/services/transfer/internal/svc/impls"
	"github.com/Robustrade/wallet-transfer-assignment/services/transfer/route"
)

// Config holds the dependencies needed to initialize the transfer service.
type Config struct {
	DB     *gorm.DB
	Logger *slog.Logger
}

// InitTransferService builds the dependency graph for the transfer service
// and registers its HTTP routes on the supplied router.
func InitTransferService(router *mux.Router, cfg Config) {
	walletRepo := postgres.NewWalletRepository(cfg.DB)
	transferRepo := postgres.NewTransferRepository(cfg.DB)
	ledgerRepo := postgres.NewLedgerRepository(cfg.DB)
	tx := postgres.NewTxManager(cfg.DB, cfg.Logger)

	svc := svcimpl.NewTransferService(svcimpl.Deps{
		TxManager:    tx,
		WalletRepo:   walletRepo,
		TransferRepo: transferRepo,
		LedgerRepo:   ledgerRepo,
		Logger:       cfg.Logger,
	})

	h := handler.NewTransferHandler(svc)
	route.InitRoutes(router, h, cfg.Logger)
}
