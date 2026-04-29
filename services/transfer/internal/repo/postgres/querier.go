// Package postgres holds the GORM-backed implementations of the repository
// interfaces declared in the sibling iface package.
//
// We use GORM strictly as a connection / transaction manager and as a
// raw-SQL executor (`tx.Raw(...).Row().Scan(...)`, `tx.Exec(...)`). We do
// NOT use GORM's ORM, query-builder, AutoMigrate, hooks, or struct-scan
// features — every statement issued from this package is hand-written
// parameterized SQL, so the database schema is owned by the migrations
// (`migrations/0001_init.up.sql`) rather than by reflection over Go structs.
package postgres

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// txKey is the private context key under which an active transactional
// *gorm.DB is stored.
type txKey struct{}

// withTx returns a new context carrying the given transactional *gorm.DB.
func withTx(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// txFromContext returns the active transactional *gorm.DB from the context,
// if any.
func txFromContext(ctx context.Context) (*gorm.DB, bool) {
	tx, ok := ctx.Value(txKey{}).(*gorm.DB)
	return tx, ok
}

// errMissingTx is returned by mustTxQuerier when no transaction is in the
// context. Repository write paths require a transaction so that a
// SELECT FOR UPDATE actually holds row locks past the end of the statement;
// falling back to the raw pool would silently break the concurrency contract.
var errMissingTx = errors.New("repository: operation requires an active transaction in the context")

// mustTxQuerier returns the active transactional *gorm.DB from the context,
// or errMissingTx if no transaction is wired. Use this in any repository
// method that must run inside the same transaction as its surrounding work
// (i.e., everything that writes to the DB or holds row locks).
func mustTxQuerier(ctx context.Context) (*gorm.DB, error) {
	tx, ok := txFromContext(ctx)
	if !ok {
		return nil, errMissingTx
	}
	return tx.WithContext(ctx), nil
}

// Postgres SQLSTATE codes used by repository error mapping.
const (
	sqlstateForeignKeyViolation = "23503"
	sqlstateCheckViolation      = "23514"
)

// isPgSQLState reports whether err is a Postgres error with the given SQLSTATE.
// We avoid pulling pgx/pgconn into the application layer by relying on the
// `SQLState() string` interface that pgx errors satisfy in v5. GORM passes
// the underlying driver error through verbatim (we set TranslateError=false
// in the gorm config), so this still works.
func isPgSQLState(err error, code string) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == code
	}
	return false
}
