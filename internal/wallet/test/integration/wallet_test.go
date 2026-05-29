//go:build integration

package integration

import (
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Happy path: a valid transfer settles, returns success, and moves money.
func TestTransferHappyPath(t *testing.T) {
	e := setupEnv(t)
	e.seed("alice", "100.00", "USD")
	e.seed("bob", "10.00", "USD")

	code, resp := e.transfer(transferReq{
		FromAccount: "alice", ToAccount: "bob", Amount: "30.00", Currency: "USD", TransactionID: newTxID(),
	})
	require.Equal(t, http.StatusOK, code, "resp=%+v", resp)
	require.Equal(t, "success", resp.Status)

	e.eventuallyBalance("alice", "70.00", "USD")
	e.eventuallyBalance("bob", "40.00", "USD")
}

// Insufficient funds is a business failure (422) and moves no money.
func TestTransferInsufficientFunds(t *testing.T) {
	e := setupEnv(t)
	e.seed("carol", "5.00", "USD")
	e.seed("dave", "0.01", "USD")

	code, resp := e.transfer(transferReq{
		FromAccount: "carol", ToAccount: "dave", Amount: "9.99", Currency: "USD", TransactionID: newTxID(),
	})
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, "failed", resp.Status)
	require.Equal(t, "INSUFFICIENT_FUNDS", resp.Reason)

	e.eventuallyBalance("carol", "5.00", "USD")
	e.eventuallyBalance("dave", "0.01", "USD")
}

// Idempotent replay: the same transaction_id posted twice settles exactly once.
func TestTransferIdempotentReplay(t *testing.T) {
	e := setupEnv(t)
	e.seed("erin", "100.00", "USD")
	e.seed("frank", "0.01", "USD")
	txID := newTxID()
	req := transferReq{FromAccount: "erin", ToAccount: "frank", Amount: "25.00", Currency: "USD", TransactionID: txID}

	code1, resp1 := e.transfer(req)
	require.Equal(t, http.StatusOK, code1)
	require.Equal(t, "success", resp1.Status)
	e.eventuallyBalance("erin", "75.00", "USD")

	// Replay with the same id: same outcome, no second settlement.
	code2, resp2 := e.transfer(req)
	require.Equal(t, http.StatusOK, code2)
	require.Equal(t, "success", resp2.Status)
	require.Equal(t, resp1.TransactionID, resp2.TransactionID)

	e.eventuallyBalance("erin", "75.00", "USD")
	e.eventuallyBalance("frank", "25.01", "USD")
}

// Same transaction_id with a different body is an idempotency conflict (409).
func TestTransferIdempotencyConflict(t *testing.T) {
	e := setupEnv(t)
	e.seed("gina", "100.00", "USD")
	e.seed("hank", "0.01", "USD")
	txID := newTxID()

	code1, _ := e.transfer(transferReq{FromAccount: "gina", ToAccount: "hank", Amount: "10.00", Currency: "USD", TransactionID: txID})
	require.Equal(t, http.StatusOK, code1)

	code2, _ := e.transfer(transferReq{FromAccount: "gina", ToAccount: "hank", Amount: "99.00", Currency: "USD", TransactionID: txID})
	require.Equal(t, http.StatusConflict, code2)
}

// Credit rejected after debit committed -> Saga compensates -> source restored.
func TestTransferCompensationOnCurrencyMismatch(t *testing.T) {
	e := setupEnv(t)
	e.seed("ivan", "100.00", "USD")
	e.seed("jane", "50.00", "EUR") // destination is established as EUR

	code, resp := e.transfer(transferReq{
		FromAccount: "ivan", ToAccount: "jane", Amount: "40.00", Currency: "USD", TransactionID: newTxID(),
	})
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, "failed", resp.Status)
	require.Equal(t, "CURRENCY_MISMATCH", resp.Reason)

	// Source must be fully restored by the compensating credit; dest untouched.
	e.eventuallyBalance("ivan", "100.00", "USD")
	e.eventuallyBalance("jane", "50.00", "EUR")
}

// Double-entry: a settled transfer records exactly one DEBIT and one CREDIT of
// equal amount in the ledger projection (the ledger balances).
func TestLedgerDoubleEntry(t *testing.T) {
	e := setupEnv(t)
	e.seed("mike", "100.00", "USD")
	e.seed("nina", "5.00", "USD")

	txID := newTxID()
	code, _ := e.transfer(transferReq{FromAccount: "mike", ToAccount: "nina", Amount: "30.00", Currency: "USD", TransactionID: txID})
	require.Equal(t, http.StatusOK, code)
	e.eventuallyBalance("mike", "70.00", "USD")
	e.eventuallyBalance("nina", "35.00", "USD")

	debit, credit := e.ledgerByTransfer(txID)
	require.Equal(t, int64(3000), debit, "exactly one DEBIT of 30.00")
	require.Equal(t, int64(3000), credit, "exactly one CREDIT of 30.00")
	require.Equal(t, debit, credit, "ledger must balance per transfer")
}

// Concurrency: many simultaneous debits on one wallet must not double-spend.
// The source can fund exactly 3 of 10 transfers of 30.00 from a 100.00 balance;
// the rest must fail INSUFFICIENT_FUNDS, and no balance goes negative.
func TestConcurrentDebitsNoDoubleSpend(t *testing.T) {
	e := setupEnv(t)
	e.seed("src", "100.00", "USD")
	e.seed("dst", "1.00", "USD")
	e.eventuallyBalance("src", "100.00", "USD")

	const n = 10
	codes := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			codes[i], _, errs[i] = e.transferRaw(transferReq{
				FromAccount: "src", ToAccount: "dst", Amount: "30.00", Currency: "USD", TransactionID: newTxID(),
			})
		}(i)
	}
	wg.Wait()

	success, failed := 0, 0
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		switch codes[i] {
		case http.StatusOK:
			success++
		case http.StatusUnprocessableEntity:
			failed++
		default:
			t.Fatalf("unexpected status %d", codes[i])
		}
	}
	require.Equal(t, 3, success, "exactly 3 transfers of 30.00 fit in 100.00")
	require.Equal(t, 7, failed, "the rest must fail insufficient funds")

	// No double spend: source debited exactly 90.00, destination credited 90.00.
	e.eventuallyBalance("src", "10.00", "USD")
	e.eventuallyBalance("dst", "91.00", "USD")
}

// Rebalance safety: restarting the (stateless) command-processor mid-stream must
// not corrupt balances — the authoritative DB row + row lock are independent of
// any in-memory state or partition ownership.
func TestCommandProcessorRebalanceSafe(t *testing.T) {
	e := setupEnv(t)
	e.seed("olga", "100.00", "USD")
	e.seed("pete", "1.00", "USD")

	code, _ := e.transfer(transferReq{FromAccount: "olga", ToAccount: "pete", Amount: "60.00", Currency: "USD", TransactionID: newTxID()})
	require.Equal(t, http.StatusOK, code)
	e.eventuallyBalance("olga", "40.00", "USD")

	// Restart the write-side processor (simulates a rebalance / rolling deploy).
	e.restartCommandProcessor()

	// Balance is still authoritative: 35.00 fits (rules out a zeroed balance)...
	code, resp := e.transfer(transferReq{FromAccount: "olga", ToAccount: "pete", Amount: "35.00", Currency: "USD", TransactionID: newTxID()})
	require.Equal(t, http.StatusOK, code, "resp=%+v", resp)
	e.eventuallyBalance("olga", "5.00", "USD")

	// ...and a second 35.00 must fail (rules out a stale 100.00 balance).
	code, resp = e.transfer(transferReq{FromAccount: "olga", ToAccount: "pete", Amount: "35.00", Currency: "USD", TransactionID: newTxID()})
	require.Equal(t, http.StatusUnprocessableEntity, code, "resp=%+v", resp)
	require.Equal(t, "INSUFFICIENT_FUNDS", resp.Reason)
}

// Multi-instance no-double-spend: with TWO command-processors in the same group,
// concurrent debits on one wallet are still serialized by the per-account DB row
// lock (SELECT ... FOR UPDATE), so exactly the affordable number succeed
// regardless of which instance processes which command.
func TestMultiInstanceNoDoubleSpend(t *testing.T) {
	e := setupEnv(t)
	e.seed("hot", "100.00", "USD")
	e.seed("sink", "1.00", "USD")
	e.eventuallyBalance("hot", "100.00", "USD")

	e.startCommandProcessor() // a second instance joins the group

	const n = 12
	codes := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			codes[i], _, errs[i] = e.transferRaw(transferReq{
				FromAccount: "hot", ToAccount: "sink", Amount: "30.00", Currency: "USD", TransactionID: newTxID(),
			})
		}(i)
	}
	wg.Wait()

	success, failed := 0, 0
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		switch codes[i] {
		case http.StatusOK:
			success++
		case http.StatusUnprocessableEntity:
			failed++
		default:
			t.Fatalf("unexpected status %d", codes[i])
		}
	}
	require.Equal(t, 3, success, "exactly 3 transfers of 30.00 fit in 100.00, even across 2 instances")
	require.Equal(t, n-3, failed)
	e.eventuallyBalance("hot", "10.00", "USD")
	e.eventuallyBalance("sink", "91.00", "USD")
}

// Reproducibility: balances reconstructed by replaying the event log match the
// projector's read model.
func TestReproducibilityFromEventLog(t *testing.T) {
	e := setupEnv(t)
	e.seed("kyle", "100.00", "USD")
	e.seed("lana", "20.00", "USD")

	code, _ := e.transfer(transferReq{FromAccount: "kyle", ToAccount: "lana", Amount: "15.00", Currency: "USD", TransactionID: newTxID()})
	require.Equal(t, http.StatusOK, code)
	e.eventuallyBalance("kyle", "85.00", "USD")
	e.eventuallyBalance("lana", "35.00", "USD")

	replayed := e.replayBalances()
	require.Equal(t, e.dbBalance("kyle"), replayed["kyle"])
	require.Equal(t, e.dbBalance("lana"), replayed["lana"])
	require.Equal(t, int64(8500), replayed["kyle"])
	require.Equal(t, int64(3500), replayed["lana"])
}
