package postgres

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
)

// LedgerRepo is the projector-owned double-entry read view. It is built from the
// event log, independent of the authoritative balance, and is idempotent under
// redelivery / outbox re-publish (UNIQUE(event_id), UNIQUE(transfer_id, type)).
type LedgerRepo struct{ db *gorm.DB }

// NewLedgerRepo constructs a LedgerRepo.
func NewLedgerRepo(db *gorm.DB) *LedgerRepo { return &LedgerRepo{db: db} }

// AppendFromEvent records one ledger entry for a balance-changing event. A
// non-balance-changing event (rejection) is a no-op. Duplicates are ignored.
func (r *LedgerRepo) AppendFromEvent(ctx context.Context, e domain.Event) error {
	if !e.ChangesBalance() {
		return nil
	}
	entryType := "CREDIT"
	if e.Type == domain.EventDebited {
		entryType = "DEBIT"
	}
	res := r.db.WithContext(ctx).Exec(
		`INSERT INTO ledger_entries (event_id, wallet_id, transfer_id, entry_type, amount_minor)
		 VALUES (?, ?, ?, ?, ?) ON CONFLICT (event_id) DO NOTHING`,
		e.EventID, e.Account, e.TransactionID, entryType, e.AmountMinor,
	)
	if res.Error != nil {
		return fmt.Errorf("ledger: append event %s: %w", e.EventID, res.Error)
	}
	return nil
}
