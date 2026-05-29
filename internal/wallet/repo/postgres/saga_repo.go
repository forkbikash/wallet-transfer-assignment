package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/repo/iface"
)

// SagaRepo persists the phase-status table. Transitions are atomic compare-and-set
// UPDATE ... RETURNING statements, so concurrent saga instances consuming a
// transfer's two legs (on different partitions) cannot lose updates: exactly one
// instance's CAS advances the row (RowsAffected == 1), the rest are no-ops.
type SagaRepo struct{ db *gorm.DB }

// NewSagaRepo constructs a SagaRepo.
func NewSagaRepo(db *gorm.DB) *SagaRepo { return &SagaRepo{db: db} }

const sagaCols = `saga_id, transaction_id, from_account, to_account, amount_minor, currency,
	status, debit_status, credit_status, compensate_status, COALESCE(failure_reason, '')`

// Create inserts a PENDING saga, or returns the existing row for a retried
// transaction_id (idempotency — a retry never starts a second transfer).
func (r *SagaRepo) Create(ctx context.Context, s iface.SagaState) (iface.SagaState, bool, error) {
	res := r.db.WithContext(ctx).Exec(
		`INSERT INTO saga_transactions
		   (saga_id, transaction_id, from_account, to_account, amount_minor, currency, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (transaction_id) DO NOTHING`,
		s.SagaID, s.TransactionID, s.FromAccount, s.ToAccount, s.AmountMinor, s.Currency, string(iface.SagaPending),
	)
	if res.Error != nil {
		return iface.SagaState{}, false, fmt.Errorf("saga: create: %w", res.Error)
	}
	if res.RowsAffected == 1 {
		s.Status = iface.SagaPending
		s.DebitStatus = iface.LegPending
		s.CreditStatus = iface.LegPending
		s.CompStatus = iface.CompNA
		return s, true, nil
	}
	existing, found, err := r.FindByTransaction(ctx, s.TransactionID)
	if err != nil {
		return iface.SagaState{}, false, err
	}
	if !found {
		return iface.SagaState{}, false, fmt.Errorf("saga: create conflict but row missing for tx %s", s.TransactionID)
	}
	return existing, false, nil
}

// FindByTransaction loads a saga by client transaction id.
func (r *SagaRepo) FindByTransaction(ctx context.Context, txID uuid.UUID) (iface.SagaState, bool, error) {
	row := r.db.WithContext(ctx).Raw(
		`SELECT `+sagaCols+` FROM saga_transactions WHERE transaction_id = ?`, txID,
	).Row()
	return scanOptionalSaga(row)
}

// MarkDebitDone transitions a PENDING debit to DONE (advance to the credit leg).
func (r *SagaRepo) MarkDebitDone(ctx context.Context, sagaID uuid.UUID) (iface.SagaState, bool, error) {
	return r.cas(ctx,
		`UPDATE saga_transactions SET debit_status='DONE', updated_at=NOW()
		 WHERE saga_id=? AND debit_status='PENDING'`, sagaID)
}

// FailDebit transitions a PENDING saga to FAILED (debit refused; no money moved).
func (r *SagaRepo) FailDebit(ctx context.Context, sagaID uuid.UUID, reason string) (iface.SagaState, bool, error) {
	return r.cas(ctx,
		`UPDATE saga_transactions SET debit_status='REJECTED', status='FAILED',
		     failure_reason=NULLIF(?, ''), updated_at=NOW()
		 WHERE saga_id=? AND status='PENDING'`, reason, sagaID)
}

// Complete transitions a PENDING saga with a pending credit to COMPLETED.
func (r *SagaRepo) Complete(ctx context.Context, sagaID uuid.UUID) (iface.SagaState, bool, error) {
	return r.cas(ctx,
		`UPDATE saga_transactions SET credit_status='DONE', status='COMPLETED', updated_at=NOW()
		 WHERE saga_id=? AND status='PENDING' AND credit_status='PENDING'`, sagaID)
}

// BeginCompensation moves a saga to COMPENSATING after a credit is refused.
func (r *SagaRepo) BeginCompensation(ctx context.Context, sagaID uuid.UUID, reason string) (iface.SagaState, bool, error) {
	return r.cas(ctx,
		`UPDATE saga_transactions SET credit_status='REJECTED', status='COMPENSATING',
		     compensate_status='PENDING', failure_reason=NULLIF(?, ''), updated_at=NOW()
		 WHERE saga_id=? AND compensate_status='NA'`, reason, sagaID)
}

// MarkCompensated transitions a COMPENSATING saga to COMPENSATED (refund applied).
func (r *SagaRepo) MarkCompensated(ctx context.Context, sagaID uuid.UUID) (iface.SagaState, bool, error) {
	return r.cas(ctx,
		`UPDATE saga_transactions SET compensate_status='DONE', status='COMPENSATED', updated_at=NOW()
		 WHERE saga_id=? AND status='COMPENSATING'`, sagaID)
}

// cas runs a conditional UPDATE ... RETURNING; advanced=false means the
// precondition didn't hold (already in/past that state) — an idempotent no-op.
func (r *SagaRepo) cas(ctx context.Context, update string, args ...any) (iface.SagaState, bool, error) {
	row := r.db.WithContext(ctx).Raw(update+` RETURNING `+sagaCols, args...).Row()
	s, found, err := scanOptionalSaga(row)
	if err != nil {
		return iface.SagaState{}, false, err
	}
	return s, found, nil
}

// SweepStale atomically claims up to limit non-terminal sagas idle longer than
// olderThan, bumping their updated_at so a peer won't re-claim them immediately.
// FOR UPDATE SKIP LOCKED shards the work across instances. Used to re-drive
// in-flight sagas after a crash and to surface stuck ones.
func (r *SagaRepo) SweepStale(ctx context.Context, olderThan time.Duration, limit int) ([]iface.SagaState, error) {
	secs := int64(olderThan.Seconds())
	var out []iface.SagaState
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rows, err := tx.Raw(
			`WITH claimed AS (
			     SELECT saga_id FROM saga_transactions
			      WHERE status NOT IN ('COMPLETED','FAILED','COMPENSATED')
			        AND updated_at < NOW() - make_interval(secs => ?)
			      ORDER BY updated_at
			      LIMIT ?
			      FOR UPDATE SKIP LOCKED
			 )
			 UPDATE saga_transactions s SET updated_at = NOW()
			 FROM claimed WHERE s.saga_id = claimed.saga_id
			 RETURNING `+prefixedSagaCols("s"),
			secs, limit,
		).Rows()
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			s, err := scanSaga(rows)
			if err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("saga: sweep stale: %w", err)
	}
	return out, nil
}

func prefixedSagaCols(alias string) string {
	return alias + `.saga_id, ` + alias + `.transaction_id, ` + alias + `.from_account, ` +
		alias + `.to_account, ` + alias + `.amount_minor, ` + alias + `.currency, ` +
		alias + `.status, ` + alias + `.debit_status, ` + alias + `.credit_status, ` +
		alias + `.compensate_status, COALESCE(` + alias + `.failure_reason, '')`
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSaga(row rowScanner) (iface.SagaState, error) {
	var s iface.SagaState
	var status, debit, credit, comp string
	if err := row.Scan(
		&s.SagaID, &s.TransactionID, &s.FromAccount, &s.ToAccount, &s.AmountMinor, &s.Currency,
		&status, &debit, &credit, &comp, &s.FailureReason,
	); err != nil {
		return iface.SagaState{}, err
	}
	s.Status = iface.SagaStatus(status)
	s.DebitStatus = iface.LegStatus(debit)
	s.CreditStatus = iface.LegStatus(credit)
	s.CompStatus = iface.CompensateStatus(comp)
	return s, nil
}

func scanOptionalSaga(row rowScanner) (iface.SagaState, bool, error) {
	s, err := scanSaga(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return iface.SagaState{}, false, nil
		}
		return iface.SagaState{}, false, fmt.Errorf("saga: scan: %w", err)
	}
	return s, true, nil
}
