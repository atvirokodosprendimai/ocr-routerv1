# Task ADR-0010-T1: Count every customer's four windows in one query

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `core.Usage`, `Repo.UsageByUser(ctx, now)`
**Consumes:** none
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the window floors`, `the per-owner grouping`, `the previous-month boundary`, `the single statement`

## Goal

`Repo.UsageByUser(ctx, now)` returns every customer's pushed / delivered / failed / expired counts for
24h, 7d, 31d and the previous calendar month, from one aggregate query bounded by an index.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/core/usage.go` | add | `Usage` and `Window`: the four windows × four counts, with the cohort rule in the doc comment |
| `internal/store/migrations/00007_usage_index.sql` | add | `idx_jobs_created_at` — this is what bounds the scan to recent rows, and without it the query reads all history on every SSE push |
| `internal/store/usage.go` | add | `UsageByUser`, and `previousMonth` beside it because the boundary arithmetic is the part worth reading |
| `internal/store/usage_internal_test.go` | add | the boundaries, the grouping, and the reconciliation. ⚠ `package store`, not `store_test`: seeding a delivered / dead / expired job at a CHOSEN timestamp needs raw SQL, because `CreateJob` hardcodes `state = queued`. The alternative — exporting a SetJobState for a test to call — would widen production surface for a test's benefit, which is the reasoning `migrate_internal_test.go` already records |
| `internal/store/migrate_internal_test.go` | edit | a Down test for the index, mirroring the two already there |

## Ordered Steps

1. [S1] Write the failing tests first: two customers with jobs inside and outside each window, a job
   exactly ON each floor, and a job one second older. Red — nothing compiles yet.
2. [S2] Add `core.Usage`: four named windows, each with `Pushed`, `Delivered`, `Failed`, `Expired`.
   ⚠ The doc comment states the COHORT RULE, because it is the thing a later reader will assume
   otherwise: every count is keyed on `created_at`, so a window's numbers describe the jobs PUSHED then
   and `Pushed − Delivered − Failed − Expired` is what is still in flight.
3. [S3] Add `00007_usage_index.sql`: `CREATE INDEX idx_jobs_created_at ON jobs(created_at);` with the
   Down that drops it, and a comment saying the query this exists for runs on the SSE push path.
4. [S4] Write `previousMonth(now)` returning the first instant of the previous calendar month and the
   first instant of this one, in UTC. ⚠ Use `time.Date(y, m-1, 1, …)` arithmetic rather than
   subtracting 31 days: January minus one month is December of the previous YEAR, and subtracting days
   lands in the wrong month for any month that is not 31 days long.
5. [S5] Write `UsageByUser` as ONE statement: `WHERE created_at >= :oldestFloor GROUP BY user_id` with
   conditional sums per window and bucket. ⚠ The floor is the MINIMUM of the four window floors, which
   is the previous month's start and not `now-31d` — getting that wrong silently truncates the
   previous-month column.
6. [S6] Add the migration Down test beside `TestMigrationDownDropsUnmetered`. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/store/ -run 'Usage' -count=1 2>&1 | tee /tmp/adr10t1.out && \
  ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/adr10t1.out && \
  go build ./... && go test ./internal/store/ ./internal/core/ -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestUsageCountsOnlyTheWindowAndOnlyTheOwner` | `internal/store/usage_internal_test.go` | the record's central claim, with TWO customers whose jobs overlap in time — a single-customer fixture cannot see a grouping defect | — | S1, S5 |
| `TestUsageWindowFloorsAreInclusive` | `internal/store/usage_internal_test.go` | a job exactly ON each floor counts and one second older does not; the off-by-one this shape invites | — | S1, S5 |
| `TestUsagePreviousMonthIsTheCompletedMonth` | `internal/store/usage_internal_test.go` | with `now` in September, August's jobs are in the month column and September's are not | — | S4 |
| `TestUsagePreviousMonthCrossesTheYear` | `internal/store/usage_internal_test.go` | with `now` in January, December of the PREVIOUS YEAR is the month column — red for any implementation that subtracts days or forgets the year | — | S4 |
| `TestUsageBucketsReconcile` | `internal/store/usage_internal_test.go` | `Delivered + Failed + Expired <= Pushed` in every window, with a job left in flight so the inequality is strict rather than vacuous | — | S2, S5 |
| `TestUsageSeparatesFailedFromExpired` | `internal/store/usage_internal_test.go` | a `dead` job and an `expired` job land in different buckets — the distinction an operator acts on | — | S5 |
| `TestUsageIsOneStatement` | `internal/store/usage_internal_test.go` | ⚠ asserts the SOURCE holds exactly one query call, NOT a runtime statement count — this driver exposes no per-connection statement counter, which is the fallback T1's Risks pre-registered. It proves the shape cannot silently become 16N; it does not prove what SQLite executed | — | S5 |
| `TestMigrationDownDropsUsageIndex` | `internal/store/migrate_internal_test.go` | the index EXISTS after `goose up` and is gone after the Down — both halves, so the Rollback section is real and the thing that bounds the scan is present | — | S3, S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestUsageCountsOnlyTheWindowAndOnlyTheOwner` |
| 2 — something selects it | nothing yet — T2 is the caller, and until then this method is unreachable by design. ⚠ That is the defect class this codebase has shipped four times, so it is named here as a DEBT T2 discharges rather than left to look finished |
| 3 — the caller can discover it | the index exists in the schema after `goose up`, asserted by the migration test |
| 4 — it is used | nothing measures whether an operator reads the numbers |

## Mutation Log

- 2026-09-28 · cef5521* · mutant killed · exit 1 · `internal/store/usage.go` · the scan floor becomes now-31d instead of the oldest window, so the previous-month column is silently truncated in the first weeks of any month — plausible numbers, wrong ones · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · covers:the window floors
- 2026-09-28 · cef5521* · mutant survived · exit 0 · `internal/store/usage.go` · a day-subtracting previous-month boundary: wrong for every month that is not 31 days long, and it drags December of the wrong year into a January window · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · covers:the previous-month boundary
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```
- 2026-09-28 · cef5521* · mutant killed · exit 1 · `internal/store/usage.go` · a day-subtracting previous-month boundary. It SURVIVED the first attempt because every month test used a 31-day month; TestUsagePreviousMonthHandlesAShortMonth is the fixture that can produce the failure · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · covers:the previous-month boundary
- 2026-09-28 · cef5521* · mutant killed · exit 1 · `internal/store/usage.go` · the grouping collapses, so every customer is charged with every other customer's jobs — the defect a single-customer fixture cannot see · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · covers:the per-owner grouping
- 2026-09-28 · cef5521* · mutant killed · exit 1 · `internal/store/usage.go` · a second database call appears, which is what a per-window or per-customer loop looks like as it creeps in · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · covers:the single statement

## Invariants

- One SQL statement per call, whatever the customer count.
- The scan floor is the oldest of the four windows, never `now-31d`.
- No count includes a job belonging to another customer.
- `Delivered + Failed + Expired` never exceeds `Pushed` for the same window.
- Nothing is written: this is a read, on the read handle.

## Risks

- `time.Date` with `m-1` is correct across a year boundary and day arithmetic is not;
  `TestUsagePreviousMonthCrossesTheYear` is the guard, and it is why that test exists rather than being
  left to review.
- ★ **A MUTANT SURVIVED HERE AND THE FIXTURE WAS THE REASON.** The day-subtracting boundary
  (`firstOfThisMonth.AddDate(0, 0, -31)`) passed both month tests on the first attempt, because August
  and December both have 31 days — so subtracting 31 days from the 1st lands exactly on the 1st of the
  previous month and the two implementations are indistinguishable. Neither assertion was weak; the
  DATA could not exhibit the defect. `TestUsagePreviousMonthHandlesAShortMonth` uses February, where 1
  March minus 31 days is 29 January, and the mutant is killed. The survived row is left in the log
  above. ⚠ The general form is worth carrying: before trusting a kill, ask whether the fixture could
  have produced the failure at all.
- Counting the query statements reads the SOURCE, not a runtime counter — this driver exposes no
  per-connection statement count, which is the fallback this section pre-registered. It proves the
  shape cannot silently become 16N and proves nothing about what SQLite executed; the task's Tests
  table says so on that row rather than leaving the stronger reading available.

## Stop Condition

Stop if `jobs` turns out to be pruned anywhere — the whole record rests on rows being kept, and a
retention policy would make every window past the retention horizon silently wrong rather than empty.

## Out of Scope

- Bytes and credits per window. (deferred: `docs/adr/BACKLOG.md`)
- Any rendering. T2 owns the view.
- A rollup table. (deferred: `docs/adr/BACKLOG.md`)

## Verification Log
- 2026-09-28 · cef5521* · exit 1 · `set -o pipefail …` · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · ms:188
  ```
  --- last 9 line(s) of stdout
  # github.com/atvirokodosprendimai/ocr-router/internal/store [github.com/atvirokodosprendimai/ocr-router/internal/store.test]
  internal/store/usage_internal_test.go:86:21: f.repo.UsageByUser undefined (type *Repo has no field or method UsageByUser)
  internal/store/usage_internal_test.go:130:21: f.repo.UsageByUser undefined (type *Repo has no field or method UsageByUser)
  internal/store/usage_internal_test.go:159:21: f.repo.UsageByUser undefined (type *Repo has no field or method UsageByUser)
  internal/store/usage_internal_test.go:186:21: f.repo.UsageByUser undefined (type *Repo has no field or method UsageByUser)
  internal/store/usage_internal_test.go:213:21: f.repo.UsageByUser undefined (type *Repo has no field or method UsageByUser)
  internal/store/usage_internal_test.go:241:21: f.repo.UsageByUser undefined (type *Repo has no field or method UsageByUser)
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/store [build failed]
  FAIL
  ```
- 2026-09-28 · cef5521* · exit 0 · `set -o pipefail …` · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · ms:3643
- 2026-09-28 · cef5521* · exit 0 · `set -o pipefail …` · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · ms:2047
- 2026-09-28 · cef5521* · exit 0 · `set -o pipefail …` · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · ms:2769
- 2026-09-28 · cef5521* · exit 0 · `set -o pipefail …` · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · ms:2406
- 2026-09-28 · cef5521* · exit 0 · `set -o pipefail …` · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · ms:2108
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:03b2aa193640a7a2aa75d3227ae12dd7e556c3f7b1d50d102b16fd79e154c06d · ms:2201
