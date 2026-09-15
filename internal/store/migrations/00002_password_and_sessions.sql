-- +goose Up
-- +goose StatementBegin

-- ADR-0003: administrators log into the dashboard with an email and a password.
--
-- ⚠ DEFAULT '' IS WHAT MAKES THIS SAFE TO APPLY TO A LIVE DATABASE. Every
-- existing row becomes valid the instant the column exists, with no backfill and
-- no window where a NOT NULL column has no value. An empty hash means "this
-- account cannot log in with a form", which is the correct and permanent state
-- for every client and worker account — they authenticate with a bearer token
-- and have no UI to log into.
--
-- It is also what makes the binary rollback-safe: a previous build runs
-- unchanged against a migrated database, because it never reads this column.
ALTER TABLE users ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';

-- A session is a token row with a deadline, and it is deliberately shaped like
-- the tokens table rather than like something new: a random secret lives in the
-- browser's cookie, and only its SHA-256 is stored here. A read of this table
-- cannot impersonate anyone.
CREATE TABLE sessions (
    id          TEXT    PRIMARY KEY,
    user_id     TEXT    NOT NULL REFERENCES users(id),
    -- SHA-256 of the cookie value. The plaintext is returned once, at creation,
    -- and written nowhere.
    token_hash  TEXT    NOT NULL UNIQUE,
    created_at  INTEGER NOT NULL,
    -- ABSOLUTE, set once, never moved. A sliding expiry refreshed on each
    -- request never ends for anyone who leaves a tab open, which is how a
    -- browser on an unlocked laptop becomes a permanent admin credential.
    expires_at  INTEGER NOT NULL,
    revoked     INTEGER NOT NULL DEFAULT 0
);

-- The only column ever queried by: every request under /admin resolves a cookie
-- through this index. Without it the lookup is a table scan on the request path.
CREATE INDEX idx_sessions_token_hash ON sessions(token_hash);

-- The sweep deletes by deadline, so it reads this rather than scanning.
CREATE INDEX idx_sessions_expires_at ON sessions(expires_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_expires_at;
DROP INDEX IF EXISTS idx_sessions_token_hash;
DROP TABLE IF EXISTS sessions;
ALTER TABLE users DROP COLUMN password_hash;
-- +goose StatementEnd
