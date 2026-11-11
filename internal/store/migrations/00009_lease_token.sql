-- +goose Up
-- ADR-0013: every claim mints its own lease token.
--
-- worker_id is the worker's TOKEN id, so one worker token claiming the same job
-- twice (two slots of one process after a reclaim, two processes on one token)
-- passed the F5 holder guard, and the older claim's result landed on the newer
-- claim's row. The processing writers now also match lease_token.
--
-- DEFAULT '' and NOT NULL: no row that predates the column is held under a
-- token. The boot reset requeues every processing row before the router serves,
-- so no claim made before this column survives into a router that checks it.
ALTER TABLE jobs ADD COLUMN lease_token TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE jobs DROP COLUMN lease_token;
