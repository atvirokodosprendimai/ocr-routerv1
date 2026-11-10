-- +goose Up
-- F1 (REPORT 2026-11-10): a done job sent back to the queue — its result swept
-- uncollected, or the router restarted — reruns its LAST stage, and that rerun
-- adds its accrual to `accrued_credits` again. Delivery debits the total, so the
-- customer paid twice for one stage.
--
-- The column remembers what the last stage added, so the requeue of a done job
-- can take exactly that back. It is not recomputable later: the rate may have
-- been edited since.
--
-- Backfill for rows already `done`: the first boot after this migration resets
-- them, and without a value they would be double-charged exactly as before. The
-- rate in service_rates today (1 when unconfigured) is the best estimate there
-- is; raw jobs accrue nothing. ⚠ Capped by accrued_credits: a rate raised since
-- the job ran would otherwise take the total below zero, and a negative charge
-- at delivery CREDITS the customer. A rate lowered since leaves part of the old
-- double charge; that is the estimate's limit, and no worse than before.
ALTER TABLE jobs ADD COLUMN last_stage_accrued INTEGER NOT NULL DEFAULT 0;
UPDATE jobs SET last_stage_accrued = MIN(accrued_credits, units *
       COALESCE((SELECT credits_per_unit FROM service_rates WHERE service_rates.label = jobs.label), 1))
 WHERE state = 'done' AND raw = 0;

-- +goose Down
ALTER TABLE jobs DROP COLUMN last_stage_accrued;
