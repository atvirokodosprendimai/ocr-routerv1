-- +goose Up
-- +goose StatementBegin

-- All times are INTEGER unix seconds. The claim statement does arithmetic on
-- them (aged priority), and doing that on ISO strings would mean parsing per row
-- inside the ORDER BY.

CREATE TABLE users (
    id            TEXT    PRIMARY KEY,
    email         TEXT    NOT NULL UNIQUE,
    role          TEXT    NOT NULL,
    -- May go negative by at most one job: the size of a result is unknowable
    -- before the work runs, so admission is gated on credits > 0 rather than on
    -- the eventual cost.
    credits       INTEGER NOT NULL DEFAULT 0,
    -- In-flight cap: queued + processing + done. Back-pressure per CUSTOMER, so
    -- issuing a second token does not double a customer's share of the pool.
    buffer_limit  INTEGER NOT NULL DEFAULT 4,
    -- Higher wins. An integer rather than a named tier so adding a tier is an
    -- UPDATE, not a deploy.
    priority      INTEGER NOT NULL DEFAULT 0,
    -- Queued deadline applied at upload. 0 = no deadline.
    job_ttl_secs  INTEGER NOT NULL DEFAULT 0,
    active        INTEGER NOT NULL DEFAULT 1,
    created_at    INTEGER NOT NULL
);

CREATE TABLE tokens (
    id           TEXT    PRIMARY KEY,
    user_id      TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role         TEXT    NOT NULL,
    -- Hex SHA-256 of the plaintext. The plaintext is returned once by the mint
    -- call and is recoverable from nowhere in this schema.
    hash         TEXT    NOT NULL UNIQUE,
    label        TEXT    NOT NULL DEFAULT '',
    revoked      INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER
);

CREATE TABLE jobs (
    id        TEXT    PRIMARY KEY,
    user_id   TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    filename  TEXT    NOT NULL DEFAULT '',
    size_byte INTEGER NOT NULL DEFAULT 0,

    -- The service wanted RIGHT NOW. Denormalised from pipeline[stage] so the
    -- claim can filter and index on it without parsing JSON per row.
    label            TEXT    NOT NULL DEFAULT 'ocr',
    pipeline         TEXT    NOT NULL DEFAULT '[]',
    stage            INTEGER NOT NULL DEFAULT 0,
    params           TEXT    NOT NULL DEFAULT '{}',
    -- Explicit rather than inferred from an empty filename: a crawler job has
    -- parameters and no file, which is a normal shape and not a corrupt upload.
    has_blob         INTEGER NOT NULL DEFAULT 1,

    state            TEXT    NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    units            INTEGER NOT NULL DEFAULT 0,
    -- Running cost across completed stages, each at its own rate. Debited once,
    -- at delivery.
    accrued_credits  INTEGER NOT NULL DEFAULT 0,

    worker_id        TEXT    NOT NULL DEFAULT '',
    lease_expires_at INTEGER,
    last_error       TEXT    NOT NULL DEFAULT '',

    -- Stamped once at upload and never rewritten — not on retry, not on a stage
    -- advance. A job that has already waited keeps the age it accrued, which is
    -- what stops pipelines and retries starving behind fresh work.
    queued_at        INTEGER NOT NULL,
    -- Whole-pipeline deadline; NULL when the customer's TTL is 0. Bounds only
    -- the queued state.
    expires_at       INTEGER,

    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

-- The claim's covering index: it filters state and label, then orders by an
-- expression over queued_at. Without label in the index every claim scans every
-- other service's backlog.
CREATE INDEX idx_jobs_claim ON jobs(state, label, queued_at);
CREATE INDEX idx_jobs_user_state ON jobs(user_id, state);
CREATE INDEX idx_jobs_expires ON jobs(state, expires_at);

-- Append-only. This is the one aggregate here that IS event-sourced, because a
-- ledger is the canonical case where the log is the domain: a correction is a
-- compensating entry, never an edit.
CREATE TABLE credit_entries (
    id         TEXT    PRIMARY KEY,
    user_id    TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    job_id     TEXT,
    delta      INTEGER NOT NULL,
    reason     TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_credit_entries_user ON credit_entries(user_id, created_at);

-- Admin-owned, and deliberately NOT derived from workers. Which labels are
-- VALID is derived from live workers; what they COST is not, because a worker
-- runs on a host we do not control and must never be able to set a price.
-- A label with no row here costs 1 per unit.
CREATE TABLE service_rates (
    label            TEXT    PRIMARY KEY,
    credits_per_unit INTEGER NOT NULL DEFAULT 1,
    updated_at       INTEGER NOT NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS service_rates;
DROP TABLE IF EXISTS credit_entries;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS tokens;
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
