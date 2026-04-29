-- 0001_init.up.sql
-- Wallet transfer service initial schema.
--
-- Design notes:
--   wallets         : materialized read-view of the ledger; balance_minor is a denormalized
--                     cache for O(1) authorization checks. The ledger is the source of truth.
--   transfers       : request->outcome record; the unique constraint on idempotency_key is
--                     the deduplication mechanism. status PENDING is a transient mid-transaction
--                     state and is never observed in committed state under normal operation.
--   ledger_entries  : append-only event log. Exactly one DEBIT and one CREDIT per transfer is
--                     enforced by UNIQUE(transfer_id, entry_type). No UPDATE / DELETE in app code.
--
-- Money is stored as int64 minor units (paisa). Currency is ISO-4217.

CREATE TABLE wallets (
    id            VARCHAR(64)  PRIMARY KEY,
    balance_minor BIGINT       NOT NULL DEFAULT 0 CHECK (balance_minor >= 0),
    currency      VARCHAR(3)   NOT NULL CHECK (char_length(currency) = 3),
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE transfers (
    id              UUID         PRIMARY KEY,
    idempotency_key VARCHAR(255) NOT NULL UNIQUE,
    request_hash    VARCHAR(64)  NOT NULL CHECK (char_length(request_hash) = 64),
    from_wallet_id  VARCHAR(64)  NOT NULL REFERENCES wallets(id),
    to_wallet_id    VARCHAR(64)  NOT NULL REFERENCES wallets(id),
    amount_minor    BIGINT       NOT NULL CHECK (amount_minor > 0),
    currency        VARCHAR(3)   NOT NULL CHECK (char_length(currency) = 3 OR currency = ''),
    status          VARCHAR(16)  NOT NULL CHECK (status IN ('PENDING','PROCESSED','FAILED')),
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (from_wallet_id <> to_wallet_id)
);

CREATE TABLE ledger_entries (
    id           BIGSERIAL    PRIMARY KEY,
    transfer_id  UUID         NOT NULL REFERENCES transfers(id),
    wallet_id    VARCHAR(64)  NOT NULL REFERENCES wallets(id),
    entry_type   VARCHAR(8)   NOT NULL CHECK (entry_type IN ('DEBIT','CREDIT')),
    amount_minor BIGINT       NOT NULL CHECK (amount_minor > 0),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (transfer_id, entry_type)
);

CREATE INDEX idx_ledger_wallet_created ON ledger_entries(wallet_id, created_at DESC);

-- FK columns on transfers — Postgres does not auto-index the referencing side
-- of a foreign key, only the referenced primary key. These indices keep FK
-- enforcement (and any future "transfers from/to wallet X" lookup) off a
-- sequential scan of the transfers table.
CREATE INDEX idx_transfers_from_wallet ON transfers(from_wallet_id);
CREATE INDEX idx_transfers_to_wallet   ON transfers(to_wallet_id);
