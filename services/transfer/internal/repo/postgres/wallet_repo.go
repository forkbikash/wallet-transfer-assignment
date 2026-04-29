package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"

	apperr "github.com/Robustrade/wallet-transfer-assignment/common/error"
	"github.com/Robustrade/wallet-transfer-assignment/common/util/money"
	"github.com/Robustrade/wallet-transfer-assignment/services/transfer/internal/data/model"
	repoiface "github.com/Robustrade/wallet-transfer-assignment/services/transfer/internal/repo/iface"
)

// walletRepository is the GORM-backed implementation of WalletRepoIface.
type walletRepository struct {
	db *gorm.DB
}

// NewWalletRepository constructs a wallet repository.
func NewWalletRepository(db *gorm.DB) repoiface.WalletRepoIface {
	return &walletRepository{db: db}
}

// LockByIDs locks all requested wallet rows in a single round-trip, in
// lexicographic id order. Lex ordering is what makes concurrent transfers
// over the same pair deadlock-free.
func (r *walletRepository) LockByIDs(ctx context.Context, ids []string) (model.Wallets, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// Sort caller-supplied IDs to make the lock-acquisition order deterministic
	// even if the caller forgot. (We also rely on the SQL `ORDER BY id`.)
	sortedIDs := make([]string, len(ids))
	copy(sortedIDs, ids)
	sort.Strings(sortedIDs)

	q, err := mustTxQuerier(ctx)
	if err != nil {
		return nil, err
	}

	const sqlText = `
SELECT id, balance_minor, currency, created_at, updated_at
FROM wallets
WHERE id = ANY($1)
ORDER BY id
FOR UPDATE`

	rows, err := q.Raw(sqlText, sortedIDs).Rows()
	if err != nil {
		return nil, fmt.Errorf("wallet_repo: lock by ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(model.Wallets, 0, len(sortedIDs))
	for rows.Next() {
		var (
			w       model.Wallet
			balance int64
		)
		if err := rows.Scan(&w.ID, &balance, &w.Currency, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("wallet_repo: scan: %w", err)
		}
		w.Balance = money.FromMinor(balance)
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("wallet_repo: rows: %w", err)
	}

	if len(out) != len(sortedIDs) {
		return nil, apperr.ErrWalletNotFound
	}
	return out, nil
}

// ApplyBalanceDeltas applies all supplied deltas in a single UPDATE,
// using a CASE expression to fan the per-row delta out by id. Saves a
// round-trip on every transfer relative to one UPDATE per wallet.
//
// The DB CHECK (balance_minor >= 0) is the last-line invariant.
func (r *walletRepository) ApplyBalanceDeltas(ctx context.Context, deltas []repoiface.BalanceDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	q, err := mustTxQuerier(ctx)
	if err != nil {
		return err
	}

	// Build:  UPDATE wallets SET balance_minor = balance_minor + CASE id
	//             WHEN $1 THEN $2 WHEN $3 THEN $4 ... END,
	//             updated_at = NOW()
	//         WHERE id IN ($1, $3, ...)
	var (
		sb       strings.Builder
		args     = make([]any, 0, len(deltas)*2)
		idArgsCS strings.Builder
	)
	sb.WriteString("UPDATE wallets SET balance_minor = balance_minor + CASE id")
	for i, d := range deltas {
		idIdx := i*2 + 1
		valIdx := i*2 + 2
		fmt.Fprintf(&sb, " WHEN $%d THEN $%d", idIdx, valIdx)
		args = append(args, d.WalletID, d.Delta.Minor())
		if i > 0 {
			idArgsCS.WriteString(", ")
		}
		fmt.Fprintf(&idArgsCS, "$%d", idIdx)
	}
	sb.WriteString(" END, updated_at = NOW() WHERE id IN (")
	sb.WriteString(idArgsCS.String())
	sb.WriteString(")")

	res := q.Exec(sb.String(), args...)
	if res.Error != nil {
		// A CHECK violation surfaces here; treat as insufficient funds for the
		// rare case validation under-lock failed.
		if isPgSQLState(res.Error, sqlstateCheckViolation) {
			return apperr.ErrInsufficientFunds.WithWrap(res.Error)
		}
		return fmt.Errorf("wallet_repo: apply balance deltas: %w", res.Error)
	}
	if res.RowsAffected != int64(len(deltas)) {
		return apperr.ErrWalletNotFound
	}
	return nil
}
