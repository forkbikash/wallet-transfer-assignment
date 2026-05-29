package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Robustrade/wallet-transfer-assignment/internal/wallet/domain"
)

// CommandStore is the AUTHORITATIVE write side. ApplyCommand performs the whole
// balance change atomically under a per-account row lock, which serializes
// writers across every instance — so it is safe regardless of Kafka consumer
// rebalancing or partition ownership. The event is written to a transactional
// outbox in the same transaction (no dual-write gap) and published by the caller
// / relay.
type CommandStore struct{ db *gorm.DB }

// NewCommandStore constructs a CommandStore.
func NewCommandStore(db *gorm.DB) *CommandStore { return &CommandStore{db: db} }

// ApplyCommand validates a leg command against the locked account row, applies
// the balance change, records command-dedup, and writes the resulting event to
// the outbox — all in one transaction. It returns the event to publish.
//
// Idempotent: a command already in processed_commands is not re-applied; instead
// the previously-recorded outbox event is returned so the caller can re-publish
// it (downstream dedups by event_id). This makes a stateless, horizontally
// scaled command-processor exactly-once.
func (s *CommandStore) ApplyCommand(ctx context.Context, cmd domain.Command) (domain.Event, error) {
	var out domain.Event
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		claimed, err := claimCommand(tx, cmd)
		if err != nil {
			return err
		}
		if !claimed { // already handled: return its recorded event to re-publish
			out, err = loadRecordedEvent(tx, cmd.Key())
			return err
		}

		cur, err := lockBalance(tx, cmd.Account)
		if err != nil {
			return err
		}
		evt := buildEvent(cmd, domain.Decide(cmd, cur), cur.Version)
		if evt.ChangesBalance() {
			if err := applyBalanceChange(tx, evt); err != nil {
				return err
			}
		}
		if err := writeOutbox(tx, evt); err != nil {
			return err
		}
		out = evt
		return nil
	})
	if err != nil {
		return domain.Event{}, fmt.Errorf("command store: apply %s: %w", cmd.Key(), err)
	}
	return out, nil
}

// claimCommand records the command for exactly-once handling. Returns false if it
// was already claimed (a redelivery).
func claimCommand(tx *gorm.DB, cmd domain.Command) (bool, error) {
	res := tx.Exec(
		`INSERT INTO processed_commands (command_key, account_id) VALUES (?, ?)
		 ON CONFLICT (command_key) DO NOTHING`,
		cmd.Key(), cmd.Account,
	)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// loadRecordedEvent returns the outbox event for a command. A missing row (the
// event was published and the outbox purged) yields a zero event = nothing to
// re-publish.
func loadRecordedEvent(tx *gorm.DB, commandKey string) (domain.Event, error) {
	var payload []byte
	row := tx.Raw(`SELECT payload FROM event_outbox WHERE command_key = ?`, commandKey).Row()
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Event{}, nil
		}
		return domain.Event{}, fmt.Errorf("load recorded event: %w", err)
	}
	return domain.DecodeEvent(payload)
}

// lockBalance loads + locks the account row (SELECT ... FOR UPDATE), serializing
// per-account writers across all instances. A missing row is the zero balance.
func lockBalance(tx *gorm.DB, account string) (domain.Balance, error) {
	cur := domain.Balance{Account: account}
	row := tx.Raw(
		`SELECT balance_minor, currency, version FROM accounts WHERE account_id = ? FOR UPDATE`,
		account,
	).Row()
	if err := row.Scan(&cur.Minor, &cur.Currency, &cur.Version); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.Balance{}, err
	}
	return cur, nil
}

// buildEvent constructs the event from the decision; a balance-changing event
// takes the next per-account version as its Seq, a rejection keeps the current.
func buildEvent(cmd domain.Command, decision domain.Decision, curVersion int64) domain.Event {
	evt := domain.Event{
		EventID:       cmd.EventID(),
		TransactionID: cmd.TransactionID,
		SagaID:        cmd.SagaID,
		Leg:           cmd.Leg,
		Type:          decision.Type,
		Account:       cmd.Account,
		AmountMinor:   cmd.AmountMinor,
		Currency:      cmd.Currency,
		Reason:        decision.Reason,
		OccurredAt:    time.Now().UTC(),
	}
	if evt.ChangesBalance() {
		evt.Seq = curVersion + 1
	} else {
		evt.Seq = curVersion
	}
	return evt
}

// applyBalanceChange updates the locked account (or inserts it on first credit).
func applyBalanceChange(tx *gorm.DB, evt domain.Event) error {
	delta := evt.AmountMinor
	if evt.Type == domain.EventDebited {
		delta = -delta
	}
	upd := tx.Exec(
		`UPDATE accounts SET balance_minor = balance_minor + ?, version = ?, updated_at = NOW()
		 WHERE account_id = ?`,
		delta, evt.Seq, evt.Account,
	)
	if upd.Error != nil {
		return upd.Error
	}
	if upd.RowsAffected == 0 {
		ins := tx.Exec(
			`INSERT INTO accounts (account_id, balance_minor, currency, version) VALUES (?, ?, ?, ?)`,
			evt.Account, delta, evt.Currency, evt.Seq,
		)
		if ins.Error != nil {
			return ins.Error
		}
	}
	return nil
}

// writeOutbox records the event atomically with the balance change.
func writeOutbox(tx *gorm.DB, evt domain.Event) error {
	payload, err := evt.Encode()
	if err != nil {
		return err
	}
	res := tx.Exec(
		`INSERT INTO event_outbox (event_id, command_key, account_id, payload) VALUES (?, ?, ?, ?::jsonb)`,
		evt.EventID, evt.CommandKey(), evt.Account, string(payload),
	)
	return res.Error
}

// MarkPublished flags an outbox row as published after its event reached Kafka.
func (s *CommandStore) MarkPublished(ctx context.Context, eventID uuid.UUID) error {
	res := s.db.WithContext(ctx).Exec(`UPDATE event_outbox SET published = TRUE WHERE event_id = ?`, eventID)
	if res.Error != nil {
		return fmt.Errorf("command store: mark published %s: %w", eventID, res.Error)
	}
	return nil
}

// ClaimUnpublished returns up to limit unpublished outbox events older than
// olderThan (so it doesn't race the hot path on fresh rows). FOR UPDATE SKIP
// LOCKED lets multiple relay instances share the work without contention.
func (s *CommandStore) ClaimUnpublished(ctx context.Context, olderThan time.Duration, limit int) ([]domain.Event, error) {
	var events []domain.Event
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rows, err := tx.Raw(
			`SELECT payload FROM event_outbox
			 WHERE NOT published AND created_at < NOW() - make_interval(secs => ?)
			 ORDER BY id LIMIT ? FOR UPDATE SKIP LOCKED`,
			int64(olderThan.Seconds()), limit,
		).Rows()
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var payload []byte
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			e, err := domain.DecodeEvent(payload)
			if err != nil {
				return err
			}
			events = append(events, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("command store: claim unpublished: %w", err)
	}
	return events, nil
}
