-- Digital wallet schema — distributed, DB-authoritative ledger with a Kafka
-- event log for the saga, audit, and reproducibility.
--
-- The command-processor is the authoritative writer of `accounts` (under a
-- row lock), atomically recording each balance change, its command-dedup row,
-- and an outbox event in ONE transaction. A relay publishes outbox events to the
-- Kafka `wallet.events` topic. Balances are therefore strongly consistent; the
-- event log remains complete (one event per balance change) so the saga can
-- coordinate, the projector can build the ledger view, and replay can
-- reconstruct balances (balance = sum of deltas, order-independent).

-- accounts : AUTHORITATIVE balance state, written by the command-processor under
-- SELECT ... FOR UPDATE (serializes per-account writers across all instances).
--   version is the per-account monotonic sequence, incremented under the lock.
CREATE TABLE accounts (
    account_id    VARCHAR(64)  PRIMARY KEY,
    balance_minor BIGINT       NOT NULL DEFAULT 0
                  CONSTRAINT accounts_balance_minor_check CHECK (balance_minor >= 0),
    currency      VARCHAR(3)   NOT NULL
                  CONSTRAINT accounts_currency_check CHECK (char_length(currency) = 3),
    version       BIGINT       NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- processed_commands : write-side idempotency ledger. The command_key
-- (transaction_id:leg) is INSERTed in the SAME transaction as the balance update
-- + outbox event, so a redelivered command (Kafka is at-least-once) hits the
-- primary key and is handled exactly once. This is what makes a stateless,
-- horizontally-scaled, rebalancing command-processor safe.
CREATE TABLE processed_commands (
    command_key VARCHAR(96)  PRIMARY KEY,   -- "<transaction_id>:<LEG>"
    account_id  VARCHAR(64)  NOT NULL,
    applied_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- event_outbox : transactional outbox. The event is written in the same
-- transaction as the balance change (no dual-write gap). A relay publishes
-- unpublished rows to wallet.events in `id` order and marks them published.
-- event_id UNIQUE + downstream dedup make re-publishes (after a crash) safe.
CREATE TABLE event_outbox (
    id           BIGSERIAL    PRIMARY KEY,
    event_id     UUID         NOT NULL UNIQUE,
    command_key  VARCHAR(96)  NOT NULL UNIQUE,   -- ties the event to its command (re-publish on redelivery)
    account_id   VARCHAR(64)  NOT NULL,          -- Kafka partition key
    payload      JSONB        NOT NULL,          -- encoded domain.Event
    published    BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Relay scan: unpublished rows in insertion order.
CREATE INDEX idx_outbox_unpublished ON event_outbox (id) WHERE NOT published;

-- ledger_entries : double-entry ledger projection (read view), written by the
-- projector from the event log. One DEBIT + one CREDIT per transfer.
--   event_id UNIQUE           -> idempotent under redelivery / re-publish.
--   UNIQUE(transfer_id, type) -> "exactly two entries per transfer"
--                                (a genesis seed deposit is a single CREDIT).
CREATE TABLE ledger_entries (
    entry_id     BIGSERIAL    PRIMARY KEY,
    event_id     UUID         NOT NULL UNIQUE,
    wallet_id    VARCHAR(64)  NOT NULL,
    transfer_id  UUID         NOT NULL,
    entry_type   VARCHAR(8)   NOT NULL CHECK (entry_type IN ('DEBIT', 'CREDIT')),
    amount_minor BIGINT       NOT NULL CHECK (amount_minor > 0),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT ledger_one_entry_per_transfer_side UNIQUE (transfer_id, entry_type)
);

CREATE INDEX idx_ledger_wallet ON ledger_entries (wallet_id, created_at DESC);
CREATE INDEX idx_ledger_transfer ON ledger_entries (transfer_id);

-- saga_transactions : the phase-status table. The Saga coordinator advances it
-- via atomic conditional UPDATEs (compare-and-set), so concurrent saga instances
-- consuming a transfer's two legs (on different partitions) cannot lose updates.
--   transaction_id UNIQUE -> client idempotency.
--   updated_at            -> the sweeper finds stale in-flight sagas.
CREATE TABLE saga_transactions (
    saga_id           UUID         PRIMARY KEY,
    transaction_id    UUID         NOT NULL UNIQUE,
    from_account      VARCHAR(64)  NOT NULL,
    to_account        VARCHAR(64)  NOT NULL,
    amount_minor      BIGINT       NOT NULL CHECK (amount_minor > 0),
    currency          VARCHAR(3)   NOT NULL,
    status            VARCHAR(16)  NOT NULL
                      CHECK (status IN ('PENDING','COMPLETED','FAILED','COMPENSATING','COMPENSATED')),
    debit_status      VARCHAR(16)  NOT NULL DEFAULT 'PENDING'
                      CHECK (debit_status IN ('PENDING','DONE','REJECTED')),
    credit_status     VARCHAR(16)  NOT NULL DEFAULT 'PENDING'
                      CHECK (credit_status IN ('PENDING','DONE','REJECTED')),
    compensate_status VARCHAR(16)  NOT NULL DEFAULT 'NA'
                      CHECK (compensate_status IN ('NA','PENDING','DONE')),
    failure_reason    TEXT,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT saga_distinct_accounts CHECK (from_account <> to_account)
);

-- Sweeper: claim non-terminal sagas that have gone stale (SELECT ... FOR UPDATE
-- SKIP LOCKED ordered by updated_at).
CREATE INDEX idx_saga_unfinished ON saga_transactions (status, updated_at);

-- transaction_outcomes : terminal result keyed by client transaction_id. The
-- gateway resolves a blocked POST from here (or the in-process registry); a late
-- poll or a retry after a timeout reads the settled outcome here.
CREATE TABLE transaction_outcomes (
    transaction_id UUID         PRIMARY KEY,
    status         VARCHAR(16)  NOT NULL CHECK (status IN ('SUCCESS','FAILED')),
    failure_reason TEXT,
    settled_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
