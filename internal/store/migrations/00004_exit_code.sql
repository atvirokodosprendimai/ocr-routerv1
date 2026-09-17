-- +goose Up
-- ADR-0007: a failed job's exit code, as DATA rather than as prose.
--
-- Before this, exitErr.ExitCode() appeared exactly once in the codebase,
-- interpolated into a message. A number that exists only inside a sentence
-- cannot be filtered, grouped or alerted on.
--
-- ⚠ NULLABLE, AND THAT IS THE WHOLE POINT. A timeout, an output-limit trip and a
-- contract violation all reach the failure path WITHOUT an exit status, and 0 is
-- the code for SUCCESS. Storing 0 for "no exit happened" would make the query an
-- operator most wants — "show me the clean exits" — silently include every job
-- that never exited at all.
ALTER TABLE jobs ADD COLUMN exit_code INTEGER NULL;

-- +goose Down
ALTER TABLE jobs DROP COLUMN exit_code;
