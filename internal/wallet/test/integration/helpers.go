//go:build integration

// Package integration exercises the full event-sourced pipeline end-to-end
// against real Kafka (KRaft) and Postgres containers via testcontainers. It
// wires the `all` run-mode in-process (command-processor + projector + saga +
// gateway sharing a push registry) so a POST resolves synchronously.
//
// Run with: go test -tags=integration ./internal/wallet/test/integration/...
// Requires a working Docker daemon.
package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // file:// migration source
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver used by golang-migrate
	"github.com/stretchr/testify/require"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Robustrade/wallet-transfer-assignment/common/util/money"
	"github.com/Robustrade/wallet-transfer-assignment/config/infra"
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

type transferReq struct {
	FromAccount   string `json:"from_account"`
	ToAccount     string `json:"to_account"`
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
	TransactionID string `json:"transaction_id"`
}

type transferResp struct {
	Status        string `json:"status"`
	TransactionID string `json:"transaction_id"`
	Reason        string `json:"reason"`
}

type balanceResp struct {
	Account  string `json:"account"`
	Balance  string `json:"balance"`
	Currency string `json:"currency"`
}

type env struct {
	t        *testing.T
	brokers  []string
	producer *walletkafka.Producer
	server   *httptest.Server
	client   *http.Client
	sqlDB    *sql.DB

	// For starting/restarting command-processor instances (rebalance/multi-instance tests).
	cmdStore  *pg.CommandStore
	logger    *slog.Logger
	baseCtx   context.Context
	cmdCancel context.CancelFunc
}

// setupEnv brings up Kafka + Postgres, applies migrations, wires the full
// pipeline, and starts the background workers. Everything is torn down via
// t.Cleanup.
func setupEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()

	// --- Postgres ---
	pgC, err := tcpostgres.Run(ctx, "postgres:15-alpine",
		tcpostgres.WithDatabase("wallet"),
		tcpostgres.WithUsername("wallet"),
		tcpostgres.WithPassword("wallet"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgC.Terminate(context.Background()) })

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	db, err := infra.InitPostgres(ctx, infra.PostgresConfig{DSN: dsn})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, applyMigrations(sqlDB))

	// --- Kafka (KRaft) ---
	kC, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.5.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = kC.Terminate(context.Background()) })
	brokers, err := kC.Brokers(ctx)
	require.NoError(t, err)

	require.NoError(t, walletkafka.EnsureTopics(ctx, brokers, 4, 1))

	logLevel := "error"
	if lv := os.Getenv("WALLET_TEST_LOG"); lv != "" {
		logLevel = lv
	}
	logger := infra.InitLogger(infra.LoggerConfig{Level: logLevel})

	// --- wire the `all` pipeline ---
	producer := walletkafka.NewProducer(brokers)
	reg := registry.New()
	accounts := pg.NewAccountRepo(db) // read model (gateway/service)
	cmdStore := pg.NewCommandStore(db)
	ledger := pg.NewLedgerRepo(db)
	sagas := pg.NewSagaRepo(db)
	outcomes := pg.NewOutcomeRepo(db)

	workerCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = producer.Close()
		_ = sqlDB.Close()
	})

	proj := projector.New(projector.Config{Brokers: brokers, Ledger: ledger, Logger: logger})
	coord := saga.New(saga.Deps{
		Brokers: brokers, Sagas: sagas, Outcomes: outcomes, Producer: producer, Resolver: reg, Logger: logger,
	})
	runWorker := func(name string, ctx context.Context, fn func(context.Context) error) {
		go func() {
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				logger.Error("worker stopped with error", "worker", name, "error", err)
			}
		}()
	}
	runWorker("projector", workerCtx, proj.Run)
	runWorker("saga", workerCtx, coord.Run)

	e := &env{
		t: t, brokers: brokers, producer: producer,
		client: &http.Client{Timeout: 90 * time.Second}, sqlDB: sqlDB,
		cmdStore: cmdStore, logger: logger, baseCtx: workerCtx,
	}
	// The command-processor gets its own cancel so tests can restart it.
	//nolint:gosec // cmdCancel is stored on env and invoked by restartCommandProcessor / parent cleanup.
	cmdCtx, cmdCancel := context.WithCancel(workerCtx)
	e.cmdCancel = cmdCancel
	runWorker("command-processor", cmdCtx, command.NewProcessor(brokers, producer, cmdStore, cmdStore, logger).Run)

	svc := service.New(service.Deps{
		Producer: producer, Registry: reg, Accounts: accounts, Sagas: sagas, Outcomes: outcomes,
		Logger: logger, WaitTimeout: 60 * time.Second,
	})
	router := mux.NewRouter()
	gateway.InitRoutes(router, gateway.NewHandler(svc), logger)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	e.server = server

	return e
}

// startCommandProcessor launches an additional stateless command-processor in the
// same consumer group (for multi-instance tests). Correctness comes from the DB
// row lock, so any number of instances is safe.
func (e *env) startCommandProcessor() {
	e.t.Helper()
	go func() {
		_ = command.NewProcessor(e.brokers, e.producer, e.cmdStore, e.cmdStore, e.logger).Run(e.baseCtx)
	}()
	time.Sleep(4 * time.Second) // let it join the group
}

// restartCommandProcessor stops the running command-processor and starts a fresh
// one (rebalance). Because the processor is stateless and the DB row lock is
// authoritative, the new instance is immediately correct — no rebuild needed.
func (e *env) restartCommandProcessor() {
	e.t.Helper()
	e.cmdCancel()               // stop the old instance (leaves the consumer group)
	time.Sleep(3 * time.Second) // allow the group to rebalance
	//nolint:gosec // cancel replaces env.cmdCancel; invoked on the next restart or via parent on cleanup.
	ctx, cancel := context.WithCancel(e.baseCtx)
	e.cmdCancel = cancel
	go func() {
		if err := command.NewProcessor(e.brokers, e.producer, e.cmdStore, e.cmdStore, e.logger).Run(ctx); err != nil && ctx.Err() == nil {
			e.logger.Error("worker stopped with error", "worker", "command-processor-restart", "error", err)
		}
	}()
	time.Sleep(5 * time.Second) // let it rejoin
}

func applyMigrations(db *sql.DB) error {
	driver, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		return err
	}
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "migration")
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	m, err := migrate.NewWithDatabaseInstance("file://"+abs, "postgres", driver)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// seed publishes a genesis credit (SagaID nil) to establish/fund an account.
func (e *env) seed(account, amount, currency string) {
	e.t.Helper()
	amt, err := money.ParseMinor(amount, currency)
	require.NoError(e.t, err)
	cmd := domain.Command{
		TransactionID: uuid.New(), SagaID: uuid.Nil, Leg: domain.LegCredit,
		Account: account, AmountMinor: amt.Minor(), Currency: currency, IssuedAt: time.Now().UTC(),
	}
	value, err := cmd.Encode()
	require.NoError(e.t, err)
	require.NoError(e.t, e.producer.Publish(context.Background(), walletkafka.TopicCommands, walletkafka.PartitionKey(account), value))
}

// transferRaw POSTs a balance transfer and returns (status, body, transport
// error). Goroutine-safe: it never calls require (which would FailNow off the
// test goroutine), so concurrency tests can call it from many goroutines.
func (e *env) transferRaw(req transferReq) (int, transferResp, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return 0, transferResp{}, err
	}
	resp, err := e.client.Post(e.server.URL+"/v1/wallet/balance_transfer", "application/json", bytes.NewReader(b))
	if err != nil {
		return 0, transferResp{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var tr transferResp
	_ = json.Unmarshal(raw, &tr)
	return resp.StatusCode, tr, nil
}

// transfer is the sequential-test wrapper that fails the test on transport error.
func (e *env) transfer(req transferReq) (int, transferResp) {
	e.t.Helper()
	code, tr, err := e.transferRaw(req)
	require.NoError(e.t, err)
	return code, tr
}

// ledgerByTransfer returns the summed DEBIT and CREDIT amounts recorded for a
// transfer in the double-entry ledger projection.
func (e *env) ledgerByTransfer(txID string) (debit, credit int64) {
	e.t.Helper()
	rows, err := e.sqlDB.Query(`SELECT entry_type, amount_minor FROM ledger_entries WHERE transfer_id = $1`, txID)
	require.NoError(e.t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var typ string
		var amt int64
		require.NoError(e.t, rows.Scan(&typ, &amt))
		switch typ {
		case "DEBIT":
			debit += amt
		case "CREDIT":
			credit += amt
		}
	}
	require.NoError(e.t, rows.Err())
	return debit, credit
}

// eventuallyBalance polls the balance read model until it equals want (the read
// side is eventually consistent) or the deadline passes.
func (e *env) eventuallyBalance(account, want, currency string) {
	e.t.Helper()
	// Generous deadline: on a cold start the projector's consumer group must
	// join before the first read model update lands.
	deadline := time.Now().Add(45 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		code, b := e.getBalance(account)
		if code == http.StatusOK {
			last = b.Balance
			if b.Balance == want && b.Currency == currency {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	e.t.Fatalf("balance of %s did not reach %s %s (last=%q)", account, want, currency, last)
}

func (e *env) getBalance(account string) (int, balanceResp) {
	e.t.Helper()
	resp, err := e.client.Get(e.server.URL + "/v1/wallet/" + account + "/balance")
	require.NoError(e.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var br balanceResp
	_ = json.Unmarshal(raw, &br)
	return resp.StatusCode, br
}

// replayBalances reconstructs balances directly from the Kafka event log via the
// pure Apply function — independent of the projector — to verify reproducibility.
func (e *env) replayBalances() map[string]int64 {
	e.t.Helper()
	state := map[string]domain.Balance{}
	err := walletkafka.ReplayTopic(context.Background(), e.brokers, walletkafka.TopicEvents, nil, func(m walletkafka.ReplayedMessage) error {
		evt, err := domain.DecodeEvent(m.Value)
		if err != nil {
			return err
		}
		if evt.ChangesBalance() {
			state[evt.Account] = domain.Apply(state[evt.Account], evt)
		}
		return nil
	})
	require.NoError(e.t, err)
	out := map[string]int64{}
	for acc, b := range state {
		out[acc] = b.Minor
	}
	return out
}

func (e *env) dbBalance(account string) int64 {
	e.t.Helper()
	var minor int64
	err := e.sqlDB.QueryRow(`SELECT balance_minor FROM accounts WHERE account_id = $1`, account).Scan(&minor)
	require.NoError(e.t, err)
	return minor
}

func newTxID() string { return uuid.New().String() }
