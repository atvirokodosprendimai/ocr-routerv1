-- +goose Up
-- ADR-0006: a per-service RAW mode, carried by three parties that must agree.
--
-- `raw` sits on service_rates, beside credits_per_unit, because it IS a price:
-- a raw job costs a flat 1 credit instead of len(units) * credits_per_unit. That
-- is the same reason credits_per_unit is admin-owned rather than derived from
-- workers (00001_init.sql) — a worker able to declare its own mode would be a
-- worker able to set what customers are charged.
--
-- Both columns default to 0. That default is the whole safety property of this
-- migration: every service and every job that exists today reads as units-mode,
-- so nothing already deployed is silently promoted to flat-1 pricing.
ALTER TABLE service_rates ADD COLUMN raw INTEGER NOT NULL DEFAULT 0;

-- jobs.raw is stamped once, at admission, and never updated. A job carries the
-- mode it was admitted under, so an administrator editing a service mid-flight
-- cannot reprice work that is already running.
ALTER TABLE jobs ADD COLUMN raw INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE jobs DROP COLUMN raw;
ALTER TABLE service_rates DROP COLUMN raw;
