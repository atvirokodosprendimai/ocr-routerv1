-- +goose Up
-- ADR-0010: per-customer usage counters over four windows.
--
-- ⚠ THIS INDEX IS WHAT BOUNDS A QUERY ON THE SSE PUSH PATH. UsageByUser filters
-- `created_at >= <the oldest window floor>` and groups by user_id, and the
-- dashboard runs it on every stream push — every fifteen seconds per connected
-- administrator. Without the index that is a full scan of every job the system
-- has ever accepted, growing for ever; with it, the scan is bounded by the oldest
-- floor, which is the previous calendar month's start — about 62 days of rows.
--
-- On `created_at` alone rather than `(user_id, created_at)`: the query has no
-- user_id to seek on. It wants every customer at once, so the time range is the
-- only thing that can narrow it, and grouping happens over the rows that survive.
-- `idx_jobs_user_state` cannot help — it carries no timestamp.
--
-- The counters are the only reader. Dropping this index makes them slow, never
-- wrong, which is why ADR-0010's Rollback calls the ordering a performance matter
-- rather than a correctness one.
CREATE INDEX idx_jobs_created_at ON jobs(created_at);

-- +goose Down
DROP INDEX idx_jobs_created_at;
