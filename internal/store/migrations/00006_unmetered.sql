-- +goose Up
-- ADR-0009: a customer exempt from metering, by a flag rather than by a sentinel
-- in the balance.
--
-- ⚠ WHY NOT `-1` IN `credits`, which is what was asked for. `credits` is a
-- BALANCE and `credit_entries` is the append-only audit of every movement, so
-- the only control anywhere in this system is ADJUST and never SET (ADR-0004,
-- pinned by TestCreditsAreNeverSetDirectly). `-1 + 50` is `49`, not "infinite
-- plus fifty". Worse, two existing code paths would destroy such a sentinel
-- while behaving perfectly correctly: the delivery debit subtracts from that
-- column, and the admin credit control adds to it. Neither would report it.
--
-- DEFAULT 0 and NOT NULL: every customer already in the database is METERED,
-- which is the direction a billing default has to fail in. The inverse spelling
-- — a `metered` column defaulting to 1 — has identical mechanics and gets it
-- wrong the one time somebody forgets to set it.
--
-- `-1` survives only as prose: nothing stores it, and the dashboard renders
-- `unlimited`.
ALTER TABLE users ADD COLUMN unmetered INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE users DROP COLUMN unmetered;
