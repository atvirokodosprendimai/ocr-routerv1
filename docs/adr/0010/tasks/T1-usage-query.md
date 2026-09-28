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
| `internal/store/usage_test.go` | add | the boundaries, the grouping, and the reconciliation |
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
| `TestUsageCountsOnlyTheWindowAndOnlyTheOwner` | `internal/store/usage_test.go` | the record's central claim, with TWO customers whose jobs overlap in time — a single-customer fixture cannot see a grouping defect | — | S1, S5 |
| `TestUsageWindowFloorsAreInclusive` | `internal/store/usage_test.go` | a job exactly ON each floor counts and one second older does not; the off-by-one this shape invites | — | S1, S5 |
| `TestUsagePreviousMonthIsTheCompletedMonth` | `internal/store/usage_test.go` | with `now` in September, August's jobs are in the month column and September's are not | — | S4 |
| `TestUsagePreviousMonthCrossesTheYear` | `internal/store/usage_test.go` | with `now` in January, December of the PREVIOUS YEAR is the month column — red for any implementation that subtracts days or forgets the year | — | S4 |
| `TestUsageBucketsReconcile` | `internal/store/usage_test.go` | `Delivered + Failed + Expired <= Pushed` in every window, with a job left in flight so the inequality is strict rather than vacuous | — | S2, S5 |
| `TestUsageSeparatesFailedFromExpired` | `internal/store/usage_test.go` | a `dead` job and an `expired` job land in different buckets — the distinction an operator acts on | — | S5 |
| `TestUsageIsOneStatement` | `internal/store/usage_test.go` | the query count for one call is 1, read from SQLite's own counter, so the cost cannot silently become 16N | — | S5 |
| `TestMigrationDownDropsUsageIndex` | `internal/store/migrate_internal_test.go` | the index EXISTS after `goose up` and is gone after the Down — both halves, so the Rollback section is real and the thing that bounds the scan is present | — | S3, S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestUsageCountsOnlyTheWindowAndOnlyTheOwner` |
| 2 — something selects it | nothing yet — T2 is the caller, and until then this method is unreachable by design. ⚠ That is the defect class this codebase has shipped four times, so it is named here as a DEBT T2 discharges rather than left to look finished |
| 3 — the caller can discover it | the index exists in the schema after `goose up`, asserted by the migration test |
| 4 — it is used | nothing measures whether an operator reads the numbers |

## Mutation Log

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
- Counting the query statements depends on a SQLite counter rather than on the code's shape. If that
  turns out not to be readable through this driver, say so in the task and assert the single statement
  by reading `internal/store/usage.go` for exactly one `QueryContext` instead — and record which of the
  two the test does, because they prove different things.

## Stop Condition

Stop if `jobs` turns out to be pruned anywhere — the whole record rests on rows being kept, and a
retention policy would make every window past the retention horizon silently wrong rather than empty.

## Out of Scope

- Bytes and credits per window. (deferred: `docs/adr/BACKLOG.md`)
- Any rendering. T2 owns the view.
- A rollup table. (deferred: `docs/adr/BACKLOG.md`)

## Verification Log
