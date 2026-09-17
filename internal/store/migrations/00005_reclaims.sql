-- +goose Up
-- ADR-0008: abandonment counted separately from failure.
--
-- `attempts` is the RETRY BUDGET: three failures of the command and the job is
-- dead. Before this column, a lease taken back from a worker that vanished spent
-- that budget too — so restarting the router three times killed work whose
-- command never failed once, and the job's last_error said "lease expired",
-- which is true and says nothing about the work.
--
-- DEFAULT 0 and NOT NULL: every row that predates the column was never abandoned
-- under a counter that did not exist, which is exactly what 0 means here. Unlike
-- exit_code (00004), there is no third state to represent — a job has been
-- abandoned some number of times, possibly none.
ALTER TABLE jobs ADD COLUMN reclaims INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE jobs DROP COLUMN reclaims;
