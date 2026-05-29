// Command wallet is the single binary for the event-sourced digital wallet.
// A --mode flag selects which role this process runs:
//
//	gateway            HTTP API + reverse proxy (push model)
//	command-processor  write-side state machine (commands -> events)
//	projector          CQRS read side (events -> Postgres read model)
//	saga               transfer orchestrator (transfers + events -> commands)
//	all                every role in one process (local dev / tests)
//	seed               publish a genesis credit to fund/establish an account
//
// In production each role is a separate container from this one image; `all`
// runs them together with an in-process push registry so POSTs return
// synchronously.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"

	"github.com/Robustrade/wallet-transfer-assignment/common/health"
	"github.com/Robustrade/wallet-transfer-assignment/common/util/money"
	config "github.com/Robustrade/wallet-transfer-assignment/config/init"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/command"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/gateway"
	walletkafka "github.com/Robustrade/wallet-transfer-assignment/internal/wallet/kafka"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/projector"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/registry"
	pg "github.com/Robustrade/wallet-transfer-assignment/internal/wallet/repo/postgres"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/saga"
	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/service"
)

func main() {
	if err := run(); err != nil {
		_, _ = os.Stderr.WriteString("wallet: " + err.Error() + "\n")
		os.Exit(1)
	}
}

func run() error {
	mode := flag.String("mode", "all", "run mode: gateway|command-processor|projector|saga|all|seed")
	seedAccount := flag.String("account", "", "seed mode: account id")
	seedAmount := flag.String("amount", "", "seed mode: amount (decimal string)")
	seedCurrency := flag.String("currency", "USD", "seed mode: ISO-4217 currency")
	flag.Parse()

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger, err := config.NewLogger()
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	kcfg, err := config.LoadKafka()
	if err != nil {
		return err
	}

	// Best-effort topic creation so local/dev runs work without a separate
	// provisioning step. In production topics are provisioned out of band.
	if err := walletkafka.EnsureTopics(rootCtx, kcfg.Brokers, kcfg.Partitions, kcfg.ReplicationFactor); err != nil {
		logger.WarnContext(rootCtx, "ensure topics (continuing)", "error", err)
	}

	switch *mode {
	case "seed":
		return runSeed(rootCtx, kcfg, *seedAccount, *seedAmount, *seedCurrency)
	case "command-processor":
		return runCommandProcessor(rootCtx, kcfg, logger)
	case "projector":
		return runProjector(rootCtx, kcfg, logger)
	case "saga":
		return runSaga(rootCtx, kcfg, logger)
	case "gateway":
		return runGateway(rootCtx, kcfg, logger)
	case "all":
		return runAll(rootCtx, kcfg, logger)
	default:
		return fmt.Errorf("unknown --mode %q", *mode)
	}
}

func runSeed(ctx context.Context, kcfg config.KafkaConfig, account, amount, currency string) error {
	if account == "" || amount == "" {
		return fmt.Errorf("seed: --account and --amount are required")
	}
	amt, err := money.ParseMinor(amount, currency)
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	producer := walletkafka.NewProducer(kcfg.Brokers)
	defer func() { _ = producer.Close() }()

	// A genesis credit is not part of a transfer: SagaID is Nil so the Saga
	// ignores it. It establishes the account's currency and opening balance.
	cmd := domain.Command{
		TransactionID: uuid.New(),
		SagaID:        uuid.Nil,
		Leg:           domain.LegCredit,
		Account:       account,
		AmountMinor:   amt.Minor(),
		Currency:      currency,
		IssuedAt:      time.Now().UTC(),
	}
	value, err := cmd.Encode()
	if err != nil {
		return err
	}
	if err := producer.Publish(ctx, walletkafka.TopicCommands, walletkafka.PartitionKey(account), value); err != nil {
		return err
	}
	fmt.Printf("seeded %s with %s %s\n", account, amount, currency)
	return nil
}

func runCommandProcessor(ctx context.Context, kcfg config.KafkaConfig, logger *slog.Logger) error {
	// The command-processor is stateless: Postgres is the authoritative balance
	// store (written under a row lock) + transactional outbox; no in-memory state.
	db, err := config.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer closeDB(db, logger)
	producer := walletkafka.NewProducer(kcfg.Brokers)
	defer func() { _ = producer.Close() }()
	store := pg.NewCommandStore(db)
	return command.NewProcessor(kcfg.Brokers, producer, store, store, logger).Run(ctx)
}

func runProjector(ctx context.Context, kcfg config.KafkaConfig, logger *slog.Logger) error {
	db, err := config.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer closeDB(db, logger)
	return newProjector(kcfg, db, logger).Run(ctx)
}

func runSaga(ctx context.Context, kcfg config.KafkaConfig, logger *slog.Logger) error {
	db, err := config.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer closeDB(db, logger)
	producer := walletkafka.NewProducer(kcfg.Brokers)
	defer func() { _ = producer.Close() }()
	// Resolver is nil: a standalone saga cannot reach the gateway's in-process
	// registry, so the gateway uses the 202 + poll fallback.
	return newCoordinator(kcfg, db, producer, nil, logger).Run(ctx)
}

func runGateway(ctx context.Context, kcfg config.KafkaConfig, logger *slog.Logger) error {
	db, err := config.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer closeDB(db, logger)
	producer := walletkafka.NewProducer(kcfg.Brokers)
	defer func() { _ = producer.Close() }()

	handler := newHandler(kcfg, db, producer, registry.New(), logger)
	return serveHTTP(ctx, handler, logger)
}

// runAll runs every role in one process. The gateway and saga share one push
// registry, so a POST resolves synchronously when the transfer settles.
func runAll(ctx context.Context, kcfg config.KafkaConfig, logger *slog.Logger) error {
	db, err := config.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer closeDB(db, logger)

	producer := walletkafka.NewProducer(kcfg.Brokers)
	defer func() { _ = producer.Close() }()
	reg := registry.New()

	cmdStore := pg.NewCommandStore(db)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return command.NewProcessor(kcfg.Brokers, producer, cmdStore, cmdStore, logger).Run(gctx)
	})
	g.Go(func() error { return newProjector(kcfg, db, logger).Run(gctx) })
	g.Go(func() error { return newCoordinator(kcfg, db, producer, reg, logger).Run(gctx) })
	g.Go(func() error { return serveHTTP(gctx, newHandler(kcfg, db, producer, reg, logger), logger) })
	return g.Wait()
}

// --- wiring helpers ---

func newProjector(kcfg config.KafkaConfig, db *gorm.DB, logger *slog.Logger) *projector.Projector {
	return projector.New(projector.Config{
		Brokers: kcfg.Brokers,
		Ledger:  pg.NewLedgerRepo(db),
		Logger:  logger,
	})
}

func newCoordinator(kcfg config.KafkaConfig, db *gorm.DB, producer saga.Producer, resolver saga.Resolver, logger *slog.Logger) *saga.Coordinator {
	return saga.New(saga.Deps{
		Brokers:  kcfg.Brokers,
		Sagas:    pg.NewSagaRepo(db),
		Outcomes: pg.NewOutcomeRepo(db),
		Producer: producer,
		Resolver: resolver,
		Logger:   logger,
	})
}

func newHandler(kcfg config.KafkaConfig, db *gorm.DB, producer service.Producer, reg *registry.Registry, logger *slog.Logger) http.Handler {
	svc := service.New(service.Deps{
		Producer:    producer,
		Registry:    reg,
		Accounts:    pg.NewAccountRepo(db),
		Sagas:       pg.NewSagaRepo(db),
		Outcomes:    pg.NewOutcomeRepo(db),
		Logger:      logger,
		WaitTimeout: kcfg.GatewayWaitTimeout,
	})
	router := mux.NewRouter()
	gateway.InitRoutes(router, gateway.NewHandler(svc), logger)
	// Liveness: process up. Readiness: dependencies (Postgres + Kafka) reachable —
	// a load balancer routes only when /readyz is 200.
	router.HandleFunc("/healthz", health.Handler(nil)).Methods(http.MethodGet)
	router.HandleFunc("/readyz", health.Readiness(
		health.DBCheck(db),
		health.Check{Name: "kafka", Probe: func(ctx context.Context) error {
			return walletkafka.Ping(ctx, kcfg.Brokers)
		}},
	)).Methods(http.MethodGet)
	return router
}

func serveHTTP(ctx context.Context, handler http.Handler, logger *slog.Logger) error {
	shutdownTimeout, err := config.ShutdownTimeoutOrDefault()
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              ":" + config.Port(),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serverErr := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, "gateway listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-ctx.Done():
	case err, open := <-serverErr:
		if open && err != nil {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func closeDB(db *gorm.DB, logger *slog.Logger) {
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	} else {
		logger.Warn("close db", "error", err)
	}
}
