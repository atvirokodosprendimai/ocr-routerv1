# Task ADR-0006-T1: Store a service's raw mode where the admin owns it, and stamp it on the job

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `Repo.ServiceMode(ctx, label) (int, bool, error)`, `Repo.SetRate(ctx, label, creditsPerUnit int, raw bool, now)`, `core.Job.Raw`, migration `00003`
**Consumes:** none
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the migration applying both columns`, `the default being units-mode`, `SetRate round-tripping raw`

## Goal

`service_rates` carries an admin-owned `raw` flag and `jobs` carries an immutable per-job `raw`
stamp, both readable and writable through the repository.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/store/migrations/00003_raw_mode.sql` | add | The two columns, with `-- +goose Down` so Rollback step 1 is real |
| `internal/store/repo.go` | edit | `ServiceMode` read; `JobByID` and the job scanners select `jobs.raw` |
| `internal/store/repo_write.go` | edit | `SetRate` gains `raw`; `CreateJob` writes `jobs.raw` |
| `internal/core/job.go` | edit | `Job.Raw bool` — the field every later task branches on |
| `internal/web/web.go` | edit | `SetRate` call site at `:510` — the compiler's list of what the signature break reaches, and the line that SELECTS a raw service today |
| `internal/store/rawmode_test.go` | add | The mode tests below |
| `internal/store/migrate_internal_test.go` | add | The rollback test. It is `package store` because goose and the embedded migration FS are both unexported, and exporting a MigrateDown for a test to call would widen production surface for a test's benefit |
| `internal/store/repo_test.go` | edit | `TestSetRateRoundTrip`'s two `SetRate` calls — the compiler's list of what the signature break reaches inside the package's own tests |

## Ordered Steps

1. [S1] Write the failing tests: `TestSetRateCarriesRawMode` (round-trips `raw=true`) and
   `TestServiceModeDefaultsToUnits` (a label with no row, and a row written before this migration,
   both read as units-mode). Confirm red — `ServiceMode` does not exist yet, so the package does not
   compile, which is red for the right reason. [proof: acceptance]
2. [S2] Write migration `00003_raw_mode.sql`: `ALTER TABLE service_rates ADD COLUMN raw INTEGER NOT
   NULL DEFAULT 0` and `ALTER TABLE jobs ADD COLUMN raw INTEGER NOT NULL DEFAULT 0`, with a Down
   that drops both.
3. [S3] Add `core.Job.Raw bool`, and select it wherever a job row is scanned.
4. [S4] Add `Repo.ServiceMode`; widen `Repo.SetRate` to take `raw bool` and upsert it.
5. [S5] Fix the `SetRate` call site in `internal/web/web.go:510`, passing the existing row's raw
   value so this task changes no admin behaviour — T8 adds the control. [proof: acceptance]
6. [S6] Run the full suite: the signature break must not have silently skipped a caller.
   [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/store/... -run 'TestSetRateCarriesRawMode|TestServiceModeDefaultsToUnits|TestCreateJobPersistsRawStamp|TestRateForLabelStillAnswers|TestMigrationDownDropsRawColumns|TestExistingServiceRowIsNotPromotedToRaw' -count=1 -v 2>&1 | tee /tmp/acc-0006-T1.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T1.out \
  && go test ./internal/store/... ./internal/router/... ./internal/web/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestSetRateCarriesRawMode` | `internal/store/rawmode_test.go` | `SetRate(label, n, true)` then `ServiceMode(label)` reports raw; setting it back to false clears it | — | S2, S4 |
| `TestServiceModeDefaultsToUnits` | `internal/store/rawmode_test.go` | A label with no `service_rates` row, and one configured with a rate, both read as units-mode — the migration's `DEFAULT 0` is what an existing deployment gets | — | S2, S4 |
| `TestCreateJobPersistsRawStamp` | `internal/store/rawmode_test.go` | A job created with `Raw: true` reads back `Raw: true`; a job created without one reads false | — | S3 |
| `TestRateForLabelStillAnswers` | `internal/store/rawmode_test.go` | `RateForLabel` still answers for configured and unconfigured labels after being reimplemented on top of `ServiceMode` | — | S4 |
| `TestMigrationDownDropsRawColumns` | `internal/store/migrate_internal_test.go` | `goose.Down` really removes both columns, read back from SQLite's own `pragma_table_info` rather than from the migration text — Rollback step 1 is executable, not asserted | — | S2 |
| `TestExistingServiceRowIsNotPromotedToRaw` | `internal/store/migrate_internal_test.go` | A `service_rates` row written under the PRE-00003 schema reads back units-mode and keeps its price after the migration — the only shape in which the column's `DEFAULT 0` is ever consulted | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestSetRateCarriesRawMode`, `TestCreateJobPersistsRawStamp` |
| 2 — something selects it | `internal/web/web.go:510` is the only `SetRate` caller today; deleting the `raw` argument there fails the build, and `TestCreateJobPersistsRawStamp` goes red if `CreateJob` stops writing the column |
| 3 — the caller can discover it | The migration is the declared interface; `TestMigrationDownDropsRawColumns` reads the real schema. No wire surface yet — T2/T3 add it |
| 4 — it is used | Nothing measures this yet. `ocrr_raw_jobs` is T6's |

## Mutation Log

- 2026-09-16 · 426571f* · mutant killed · exit 1 · `internal/store/repo_write.go` · SetRate silently discards the admin raw flag, so a service can never be marked raw — the exact silent-downgrade this task exists to make impossible · acceptance-sha256:8476b6d179dfc7e9199c7a31ed6165e71dae63e3e72f774a7db1cf00adb56a89
- 2026-09-16 · 426571f* · mutant killed · exit 1 · `internal/store/repo_write.go` · SetRate silently discards the admin raw flag, so no service can ever be marked raw · acceptance-sha256:1ff4bb5c58f4359a57beb4e8967af852c50f492f91498dff066972254e7a4e30 · covers:SetRate round-tripping raw
- 2026-09-16 · 426571f* · mutant survived · exit 0 · `internal/store/migrations/00003_raw_mode.sql` · every existing service silently becomes raw on migration — the flat-1 repricing of already-deployed paid services this ADR is shaped to prevent · acceptance-sha256:1ff4bb5c58f4359a57beb4e8967af852c50f492f91498dff066972254e7a4e30 · covers:the default being units-mode
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```
- 2026-09-16 · 426571f* · mutant killed · exit 1 · `internal/store/migrations/00003_raw_mode.sql` · the jobs half of the migration never lands, so a job can carry no mode at all · acceptance-sha256:1ff4bb5c58f4359a57beb4e8967af852c50f492f91498dff066972254e7a4e30 · covers:the migration applying both columns
- 2026-09-16 · 426571f* · mutant killed · exit 1 · `internal/store/migrations/00003_raw_mode.sql` · every service that existed before the migration silently becomes raw, repricing already-deployed paid work to a flat 1 credit · acceptance-sha256:e2f2b9f89ac86b234c9d437dcfdd8943f8fd1afd93aa7bdf1a435077943e6638 · covers:the default being units-mode
- 2026-09-16 · 426571f* · mutant killed · exit 1 · `internal/store/repo_write.go` · SetRate silently discards the admin raw flag, so no service can ever be marked raw · acceptance-sha256:e2f2b9f89ac86b234c9d437dcfdd8943f8fd1afd93aa7bdf1a435077943e6638 · covers:SetRate round-tripping raw
- 2026-09-16 · 426571f* · mutant killed · exit 1 · `internal/store/migrations/00003_raw_mode.sql` · the jobs half of the migration never lands, so a job can carry no mode at all · acceptance-sha256:e2f2b9f89ac86b234c9d437dcfdd8943f8fd1afd93aa7bdf1a435077943e6638 · covers:the migration applying both columns

## Invariants

- A `service_rates` row written before this migration reads as units-mode, never raw. Silent
  promotion of an existing paid service to flat-1 pricing is the failure this whole ADR is shaped
  around.
- `jobs.raw` is written once, at insert, and never updated. A job's price must not change because
  an administrator edited a service while it was running.
- `SetRate` remains the only writer of `service_rates`.

## Risks

⚠ **A mutant SURVIVED here first, and the surviving row is left in the Mutation Log on purpose.**
Flipping 00003's `service_rates.raw DEFAULT 0` to `DEFAULT 1` did not fail the fence. The two tests
that look like they cover it do not: the unconfigured case returns `false` from the `ErrNoRows`
branch without reading the column at all, and the configured case has `SetRate` writing `0`
explicitly. A column default is consulted in exactly one situation — a row that ALREADY EXISTS when
the migration runs — and that shape can only be built by migrating down, seeding, and migrating up.
`TestExistingServiceRowIsNotPromotedToRaw` does that and kills the mutant. The lesson generalises:
**a DEFAULT is untested until a test produces a row that predates it.**

- The `SetRate` signature break reaches a caller outside `internal/web`. Mitigated by the compiler
  and by the regression half of the Acceptance fence, which builds `./internal/router/...` too.
- SQLite `ALTER TABLE ADD COLUMN` cannot add a column to the middle of a row; both are appended,
  which is fine and is why `NOT NULL DEFAULT 0` is required rather than optional.

## Stop Condition

Stop if the project's goose setup cannot express a reversible `ADD COLUMN` (older SQLite drops
require a table rebuild). Rollback step 1 in the ADR promises reversibility; if it cannot be
delivered, the ADR's Rollback section is wrong and must be amended before proceeding rather than
after.

## Out of Scope

- Any admin control for the new flag — that is T8's job.
- Any behaviour change to pricing — T6's job. This task only stores the bit.

## Verification Log
- 2026-09-16 · 426571f* · exit 1 · `set -o pipefail …` · acceptance-sha256:8476b6d179dfc7e9199c7a31ed6165e71dae63e3e72f774a7db1cf00adb56a89 · ms:272
  ```
  --- last 8 line(s) of stdout
  # github.com/atvirokodosprendimai/ocr-router/internal/store [github.com/atvirokodosprendimai/ocr-router/internal/store.test]
  internal/store/migrate_internal_test.go:31:22: db.write undefined (type *DB has no field or method write, but does have field Write)
  internal/store/migrate_internal_test.go:34:22: db.write undefined (type *DB has no field or method write, but does have field Write)
  internal/store/migrate_internal_test.go:43:26: db.write undefined (type *DB has no field or method write, but does have field Write)
  internal/store/migrate_internal_test.go:47:21: db.write undefined (type *DB has no field or method write, but does have field Write)
  internal/store/migrate_internal_test.go:50:21: db.write undefined (type *DB has no field or method write, but does have field Write)
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/store [build failed]
  FAIL
  ```
- 2026-09-16 · 426571f* · exit 1 · `set -o pipefail …` · acceptance-sha256:8476b6d179dfc7e9199c7a31ed6165e71dae63e3e72f774a7db1cf00adb56a89 · ms:8015
  ```
  --- last 10 line(s) of stdout (of 11 after folding 11 raw)
  --- PASS: TestSetRateCarriesRawMode (0.01s)
  === RUN   TestServiceModeDefaultsToUnits
  --- PASS: TestServiceModeDefaultsToUnits (0.01s)
  PASS
  ok  	github.com/atvirokodosprendimai/ocr-router/internal/store	0.412s
  ok  	github.com/atvirokodosprendimai/ocr-router/internal/store	0.625s
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/router [build failed]
  ok  	github.com/atvirokodosprendimai/ocr-router/internal/web	5.411s
  ok  	github.com/atvirokodosprendimai/ocr-router/internal/web/views	0.457s
  FAIL
  --- last 7 line(s) of stderr
  # github.com/atvirokodosprendimai/ocr-router/internal/router_test [github.com/atvirokodosprendimai/ocr-router/internal/router.test]
  internal/router/pipeline_test.go:184:44: not enough arguments in call to h.repo.SetRate
  	have (context.Context, string, number, "time".Time)
  	want (context.Context, string, int, bool, "time".Time)
  internal/router/pipeline_test.go:187:42: not enough arguments in call to h.repo.SetRate
  	have (context.Context, string, number, "time".Time)
  	want (context.Context, string, int, bool, "time".Time)
  ```
- 2026-09-16 · 426571f* · exit 0 · `set -o pipefail …` · acceptance-sha256:8476b6d179dfc7e9199c7a31ed6165e71dae63e3e72f774a7db1cf00adb56a89 · ms:7843
- 2026-09-16 · 426571f* · exit 0 · `set -o pipefail …` · acceptance-sha256:1ff4bb5c58f4359a57beb4e8967af852c50f492f91498dff066972254e7a4e30 · ms:7075
- 2026-09-16 · 426571f* · exit 0 · `set -o pipefail …` · acceptance-sha256:1ff4bb5c58f4359a57beb4e8967af852c50f492f91498dff066972254e7a4e30 · ms:7280
- 2026-09-16 · 426571f* · exit 0 · `set -o pipefail …` · acceptance-sha256:1ff4bb5c58f4359a57beb4e8967af852c50f492f91498dff066972254e7a4e30 · ms:6414
- 2026-09-16 · 426571f* · exit 0 · `set -o pipefail …` · acceptance-sha256:e2f2b9f89ac86b234c9d437dcfdd8943f8fd1afd93aa7bdf1a435077943e6638 · ms:7822
- 2026-09-16 · 426571f* · exit 0 · `set -o pipefail …` · acceptance-sha256:e2f2b9f89ac86b234c9d437dcfdd8943f8fd1afd93aa7bdf1a435077943e6638 · ms:5977
- 2026-09-16 · 426571f* · exit 0 · `set -o pipefail …` · acceptance-sha256:e2f2b9f89ac86b234c9d437dcfdd8943f8fd1afd93aa7bdf1a435077943e6638 · ms:6386
