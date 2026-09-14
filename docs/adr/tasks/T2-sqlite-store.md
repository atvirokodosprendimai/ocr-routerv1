# Task ADR-0001-T2: Open SQLite as a writer and a read-only reader, and own all SQL behind a repository

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `store.Open()` returning `(read, write *sql.DB, err error)`, `store.Repo` with every query and write statement, embedded goose migrations, the aged-priority label-filtered claim statement
**Consumes:** `core.Job`, `core.User`, `core.Token`, `core.JobState`, `core.Err*` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the query_only refusal`, `the txlock failure count`, `the claim ORDER BY`

## Goal

Open one SQLite file as two handles — a serialised writer and a read-only reader pool — run
the schema migration, and put every SQL statement behind `store.Repo`, including the single
atomic claim that implements aged priority and the queue deadline.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/store/migrations/00001_init.sql` | add | `users`, `tokens`, `jobs`, `credit_entries`, `service_rates`. ⚠ Moved here from `db/migrations/` during execution: `//go:embed` cannot traverse upward out of its own package directory, so `internal/store` physically cannot embed `db/migrations`. A language constraint, not a design change — the SQL now sits beside the only code that runs it. |
| `internal/store/store.go` | add | `Open()`, DSN construction, pragmas, migration runner |
| `internal/store/migrations.go` | add | `//go:embed db/migrations/*.sql` |
| `internal/store/repo.go` | add | `Repo` — every query and write statement |
| `internal/store/store_test.go` | add | handle, pragma and migration tests |
| `internal/store/repo_test.go` | add | claim-ordering, atomicity and round-trip tests |

`store.Open` is selected by `cmd/router` in T8; T8's Affected Files carries that line, and
deleting it fails T8's end-to-end fence.

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write
   `store_test.go` asserting that a write through the read handle is refused and that the
   migration applies, before any implementation (TDD red). [proof: acceptance]
2. [S2] Write `00001_init.sql` with `-- +goose Up` / `-- +goose Down`, the five tables and
   `REFERENCES … ON DELETE CASCADE` on every child table. **All times are INTEGER unix
   seconds**, so the claim's aging arithmetic happens in SQL without date parsing.
   `users` carries `credits`, `buffer_limit`, **`priority INTEGER NOT NULL DEFAULT 0`** and
   **`job_ttl_secs INTEGER NOT NULL DEFAULT 0`** (0 = no deadline). `jobs` carries
   `queued_at`, `expires_at` (nullable), **`label TEXT NOT NULL DEFAULT 'ocr'`**,
   **`pipeline TEXT`** (JSON array), **`stage INTEGER NOT NULL DEFAULT 0`**,
   **`params TEXT`** (JSON object), **`has_blob INTEGER NOT NULL DEFAULT 1`** and
   **`accrued_credits INTEGER NOT NULL DEFAULT 0`**. `service_rates(label TEXT PRIMARY KEY,
   credits_per_unit INTEGER NOT NULL DEFAULT 1)` is admin-owned and deliberately NOT derived
   from workers. Index `tokens(hash)`, **`jobs(state, label, queued_at)`** — label is in the
   index because every claim filters on it — and `jobs(user_id, state)`.
3. [S3] Build two DSNs against the same file. Writer:
   `_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate`
   with `SetMaxOpenConns(1)`. Reader: the same pragmas **plus** `_pragma=query_only(1)`,
   **without** `_txlock`, and **without** `journal_mode` — setting WAL requires a write, and
   journal mode is a property of the file the writer has already set.
4. [S4] Set `foreign_keys(1)` explicitly on both handles. SQLite defaults it OFF, and the
   `ON DELETE CASCADE` clauses in S2 read as if they would fire when they would not.
5. [S5] Run goose with dialect `sqlite3` against the **writer** handle while the driver is
   registered as `sqlite`. The two names differ and the mismatch surfaces only at startup.
6. [S6] Write `Repo` read methods (`UserByID`, `UserByEmail`, `TokenByHash`, `JobByID`,
   `JobsByUserAndState`, `CountInFlight`, `ListJobs`, `ListUsers`, `Ledger`, `RateForLabel`,
   `ListRates`, `QueueDepthByLabel`) on the read handle, and write methods (`CreateUser`,
   `UpdateUser`, `CreateToken`, `RevokeToken`, `CreateJob`, `ClaimOneQueued`, `CompleteJob`,
   `AdvanceStage`, `FailJob`, `DeliverJob`, `RequeueExpired`, `ExpireOverdue`, `SetRate`,
   `ResetInFlightOnBoot`) on the write handle. `RateForLabel` returns **1** for a label with
   no row, so a live service always has a defined price without an admin having to act.
7. [S7] `ClaimOneQueued(now, agingStep, label, workerID, leaseUntil)` is **one** statement:

   ```sql
   UPDATE jobs SET state='processing', worker_id=?, lease_expires_at=?
   WHERE id = (
     SELECT j.id FROM jobs j JOIN users u ON u.id = j.user_id
     WHERE j.state='queued' AND j.label = :label
       AND (j.expires_at IS NULL OR j.expires_at > :now)
     ORDER BY (u.priority + (:now - j.queued_at) / :aging_step) DESC, j.id ASC
     LIMIT 1
   ) RETURNING id
   ```

   `:now` is one `time.Now().Unix()` computed by the caller and bound — never
   `strftime('now')`, which is re-evaluated per row. The deadline predicate sits in the
   same `WHERE` as the selection, so an expiring job cannot be claimed in a race. The
   `label` predicate is what makes one queue serve many services.
8. [S8] `ExpireOverdue(now)` sets `state='expired'` for `queued` rows past `expires_at`,
   `RETURNING id, user_id` so the caller can notify. It touches **only** `queued` — a
   `processing` job runs to completion.
9. [S9] `AdvanceStage(jobID, nextLabel, accrue)` sets `label=nextLabel`, `stage=stage+1`,
   `accrued_credits=accrued_credits+:accrue`, `state='queued'`, clears `worker_id` and
   `lease_expires_at`, and **leaves `queued_at` untouched** so a pipeline job keeps the age
   it has accrued. One statement, for the same reason the claim is one statement.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/store/... -count=1 -race 2>&1 | tee /tmp/adr1-t2.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t2.out
```

Red at authoring: `internal/store` does not exist, so `go build ./...` fails.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestOpenRunsMigration` | `internal/store/store_test.go` | after `Open`, all five tables exist | — | S2, S5 |
| `TestReadHandleRefusesWrite` | `internal/store/store_test.go` | an INSERT through the read handle fails — `query_only(1)` is honoured by this driver, not merely requested | — | S3 |
| `TestReadHandleStillReadsUnderWAL` | `internal/store/store_test.go` | dropping `journal_mode` from the reader DSN does not break reads of a WAL file | — | S3 |
| `TestForeignKeysAreEnforced` | `internal/store/store_test.go` | a token for a non-existent user is refused, and deleting a user cascades its tokens | — | S4 |
| `TestTxlockImmediateBothArms` | `internal/store/store_test.go` | concurrent read-then-write transactions fail without `_txlock=immediate` and succeed with it — **both arms asserted, so it cannot pass vacuously** | — | S3 |
| `TestGooseDialectMatchesDriver` | `internal/store/store_test.go` | `Open` succeeds, which it cannot if the goose dialect and driver name disagree | — | S5 |
| `TestClaimPrefersHigherPriority` | `internal/store/repo_test.go` | with two jobs queued at the same instant, the higher-`priority` user's is claimed first | — | S7 |
| `TestClaimAgingOvertakesPriority` | `internal/store/repo_test.go` | a `priority=0` job queued long enough is claimed **before** a fresh `priority=100` job — the overtake itself, which is the assertion that fails if the aging term is dropped or truncates to 0 | — | S7 |
| `TestClaimIsFifoWithinATier` | `internal/store/repo_test.go` | two jobs of equal effective priority are claimed in `id` (uuidv7 arrival) order | — | S7 |
| `TestClaimSkipsExpired` | `internal/store/repo_test.go` | a queued job past `expires_at` is never claimed, even when it has the highest effective priority | — | S7 |
| `TestClaimOneQueuedIsAtomic` | `internal/store/repo_test.go` | N goroutines claiming against 1 queued job yield exactly 1 winner | — | S7 |
| `TestClaimOneQueuedEmpty` | `internal/store/repo_test.go` | claiming with nothing queued returns `core.ErrNotFound`, not a zero-value job | — | S7 |
| `TestExpireOverdueLeavesProcessing` | `internal/store/repo_test.go` | `ExpireOverdue` expires an overdue `queued` job and leaves an overdue `processing` job untouched | — | S8 |
| `TestRepoRoundTrip` | `internal/store/repo_test.go` | every write method's effect is visible through the matching read method | — | S6 |
| `TestClaimFiltersByLabel` | `internal/store/repo_test.go` | a worker claiming `strip-html` never receives an `ocr` job, even when the `ocr` job has far higher effective priority — the isolation that makes one queue serve many services | — | S7 |
| `TestClaimLabelEmptyQueue` | `internal/store/repo_test.go` | with jobs queued under other labels only, claiming returns `core.ErrNotFound` rather than someone else's work | — | S7 |
| `TestAdvanceStagePreservesQueuedAt` | `internal/store/repo_test.go` | advancing a stage requeues with the next label, increments `stage`, accrues credits, and leaves `queued_at` unchanged | — | S9 |
| `TestRateForLabelDefaultsToOne` | `internal/store/repo_test.go` | a label with no `service_rates` row returns 1, not 0 — a missing row must not make a service free | — | S6 |
| `TestSetRateRoundTrip` | `internal/store/repo_test.go` | `SetRate` upserts and `ListRates` reflects it | — | S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the fourteen tests above |
| 2 — something selects it | `cmd/router` calls `store.Open` (T8) and `router.Service` calls every `Repo` method (T6); the mutations recorded there prove the composition root reaches them. Within this task, `TestOpenRunsMigration` fails if the migration is not wired into `Open`. |
| 3 — the caller can discover it | n/a: no declared interface — an internal Go package |
| 4 — it is used | every router request path reads or writes through `Repo`; T8's end-to-end test exercises it |

## Mutation Log

- 2026-09-15 · 1b23108* · mutant killed · exit 1 · `internal/store/repo_write.go` · Aging is what stops the bottom tier starving. Neutralising the term must be caught by the overtake test, not merely by a formula check. · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d
- 2026-09-15 · 1b23108* · mutant killed · exit 1 · `internal/store/repo_write.go` · Label isolation is what lets one queue serve many services; without it a strip-html worker receives OCR jobs. · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d
- 2026-09-15 · 1b23108* · mutant killed · exit 1 · `internal/store/repo_write.go` · The deadline predicate sits in the same WHERE as the selection so an expiring job cannot be claimed in a race; removing it must be caught. · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d
- 2026-09-15 · 1b23108* · mutant killed · exit 1 · `internal/store/repo_write.go` · The deadline predicate sits in the same WHERE as the selection so an expiring job cannot be claimed in a race; removing it must be caught. · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d
- 2026-09-15 · 1b23108* · mutant killed · exit 1 · `internal/store/repo_write.go` · Expiry must never kill work a worker has already started; the worker time spent is not recovered by discarding it. · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d
- 2026-09-15 · 1b23108* · mutant killed · exit 1 · `internal/store/repo_write.go` · The state='done' guard is what makes a second delivery charge nothing; without it a re-delivery debits twice. · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d
- 2026-09-15 · 1b23108* · mutant killed · exit 1 · `internal/store/store.go` · query_only is what makes 'read models never write' a driver-enforced invariant rather than a review rule; dropping it must be caught. · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · covers:the query_only refusal
- 2026-09-15 · 1b23108* · mutant killed · exit 1 · `internal/store/repo_write.go` · Reverting the claim to strict priority (no aging term) starves the bottom tier; the overtake test must catch it. · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · covers:the claim ORDER BY

## Invariants

- The read handle **never** writes, enforced by the driver rather than by review.
- Exactly one writer connection (`SetMaxOpenConns(1)`).
- Every SQL statement in the codebase lives in `internal/store`.
- `ClaimOneQueued` is one statement; a read-then-write claim is the bug it exists to avoid.
- All stored times are unix seconds, and `:now` is always bound by the caller, never
  evaluated by SQLite.
- `ExpireOverdue` never touches a `processing` row.

## Risks

- **`_txlock` is a documented `mattn` parameter and may be silently ignored by
  `modernc.org/sqlite`.** A silently-ignored DSN knob is indistinguishable from an honoured
  one, so `TestTxlockImmediateBothArms` asserts a **behaviour change** between the arms
  rather than asserting the string is present.
- **Integer division silently disables aging.** `(now - queued_at) / aging_step` is integer
  division in SQLite; with a large `aging_step` the term is 0 for a long time and the queue
  is strict priority in disguise. `TestClaimAgingOvertakesPriority` asserts the overtake
  rather than the formula, so it fails when that happens.
- **A `RETURNING` clause on `UPDATE`** requires SQLite ≥ 3.35. `modernc.org/sqlite` at the
  pinned version is well past that, but `TestClaimOneQueuedIsAtomic` fails loudly if not.

## Stop Condition

Stop and report if `TestTxlockImmediateBothArms` shows **no** difference between the arms —
that means the parameter is ignored by this driver and the concurrency story in ADR-0001 §9
needs a different mechanism, which is a decision, not an implementation detail.

## Out of Scope

- Business rules about when a job may be claimed — T6's; `Repo` only executes.
- Credit arithmetic — T6 owns the ledger transaction; `Repo` provides the statements.
- Notifying clients that a job expired — T6 publishes, T7 delivers.

## Verification Log
- 2026-09-15 · 1b23108* · exit 0 · `set -o pipefail …` · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · ms:2711
- 2026-09-15 · 1b23108* · exit 0 · `set -o pipefail …` · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · ms:2598
- 2026-09-15 · 1b23108* · exit 0 · `set -o pipefail …` · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · ms:2630
- 2026-09-15 · 1b23108* · exit 0 · `set -o pipefail …` · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · ms:3438
- 2026-09-15 · 1b23108* · exit 0 · `set -o pipefail …` · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · ms:3567
- 2026-09-15 · 1b23108* · exit 0 · `set -o pipefail …` · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · ms:3402
- 2026-09-15 · 1b23108* · exit 0 · `set -o pipefail …` · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · ms:2668
- 2026-09-15 · 1b23108* · exit 0 · `set -o pipefail …` · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · ms:3728
- 2026-09-15 · 1b23108* · exit 0 · `set -o pipefail …` · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · ms:2689
- 2026-09-15 · 1b23108* · exit 0 · `set -o pipefail …` · acceptance-sha256:8ad3c2f2b9088d1203a9f02b1f84b911a2e9d9dd819f9f1f10c6e5043456991d · ms:2672
