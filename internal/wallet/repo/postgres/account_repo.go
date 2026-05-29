// Package postgres implements the iface persistence contracts over a
// GORM-managed *gorm.DB. GORM is used only as a connection/transaction manager
// and raw-SQL executor — every statement is hand-written parameterized SQL; the
// schema is owned by the migrations.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
)

// AccountRepo reads the authoritative `accounts` balance (written by the
// command-processor's CommandStore). Balances are strongly consistent, so the
// gateway's balance query is read-your-writes correct.
type AccountRepo struct{ db *gorm.DB }

// NewAccountRepo constructs an AccountRepo.
func NewAccountRepo(db *gorm.DB) *AccountRepo { return &AccountRepo{db: db} }

// Get returns the materialized balance for an account.
func (r *AccountRepo) Get(ctx context.Context, account string) (domain.Balance, bool, error) {
	var b domain.Balance
	row := r.db.WithContext(ctx).Raw(
		`SELECT account_id, balance_minor, currency, version FROM accounts WHERE account_id = ?`,
		account,
	).Row()
	if err := row.Scan(&b.Account, &b.Minor, &b.Currency, &b.Version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Balance{}, false, nil
		}
		return domain.Balance{}, false, fmt.Errorf("account: get %s: %w", account, err)
	}
	return b, true, nil
}
