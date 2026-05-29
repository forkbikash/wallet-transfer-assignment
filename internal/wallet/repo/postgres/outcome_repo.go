package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/repo/iface"
)

// OutcomeRepo persists terminal transaction results. The gateway reads these to
// answer late polls / retries after a push-deadline timeout.
type OutcomeRepo struct{ db *gorm.DB }

// NewOutcomeRepo constructs an OutcomeRepo.
func NewOutcomeRepo(db *gorm.DB) *OutcomeRepo { return &OutcomeRepo{db: db} }

// Upsert records the terminal outcome. Outcomes are immutable once set, so a
// duplicate write (e.g. saga reprocessing) is a no-op (first writer wins).
func (r *OutcomeRepo) Upsert(ctx context.Context, o iface.Outcome) error {
	res := r.db.WithContext(ctx).Exec(
		`INSERT INTO transaction_outcomes (transaction_id, status, failure_reason)
		 VALUES (?, ?, NULLIF(?, '')) ON CONFLICT (transaction_id) DO NOTHING`,
		o.TransactionID, string(o.Status), o.FailureReason,
	)
	if res.Error != nil {
		return fmt.Errorf("outcome: upsert %s: %w", o.TransactionID, res.Error)
	}
	return nil
}

// Get loads the outcome for a transaction id.
func (r *OutcomeRepo) Get(ctx context.Context, txID uuid.UUID) (iface.Outcome, bool, error) {
	var o iface.Outcome
	var status string
	row := r.db.WithContext(ctx).Raw(
		`SELECT transaction_id, status, COALESCE(failure_reason, ''), settled_at
		 FROM transaction_outcomes WHERE transaction_id = ?`,
		txID,
	).Row()
	if err := row.Scan(&o.TransactionID, &status, &o.FailureReason, &o.SettledAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return iface.Outcome{}, false, nil
		}
		return iface.Outcome{}, false, fmt.Errorf("outcome: get %s: %w", txID, err)
	}
	o.Status = iface.OutcomeStatus(status)
	return o, true, nil
}
