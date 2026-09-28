# Task ADR-0009-T3: Waive the charge on an unmetered delivery, and count what was waived

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `ocrr_credits_waived_total`
**Consumes:** `core.User.Unmetered` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the zero charge`, `the unchanged accrual`, `the absent ledger row`, `the waived counter`

## Goal

`Deliver` and `DeliverRaw` charge an unmetered customer nothing, leave `jobs.accrued_credits` exactly
as it is, and record what they did not charge on `ocrr_credits_waived_total`.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/router/service.go` | edit | both delivery paths select the charge from the owner's `Unmetered`; both already hold `job.UserID` and neither reads the user yet |
| `internal/router/metrics.go` | edit | `metricCreditsWaived` — the local copy the service increments |
| `internal/monitor/registry.go` | edit | `MetricCreditsWaived` — where this process's metric names are documented for an operator. ⚠ It is NOT a functional requirement: `Registry` validates LABEL names against an allow-list and does not validate metric names at all, which is why `ocrr_raw_jobs_total` has lived in `internal/router/metrics.go` alone since ADR-0006 with nothing pinning it. T3 writes the pinning test rather than inheriting one |
| `internal/router/unmetered_test.go` | edit | the delivery half of the record's central claim |

## Ordered Steps

1. [S1] Write the failing tests first: after delivering a multi-unit job for an unmetered owner, the
   balance is unchanged, `credit_entries` holds no row for that job, and `jobs.accrued_credits` is the
   cost it would have been. Red — today the balance drops.
2. [S2] Add `MetricCreditsWaived = "ocrr_credits_waived_total"` to `internal/monitor/registry.go` and
   its unexported twin to `internal/router/metrics.go`, with the comment saying why it is a SECOND
   series rather than a widening of `ocrr_credits_debited_total`: that one means credits actually
   taken from a balance, and it has to keep meaning that.
3. [S3] In `Deliver`, read the owner and select the charge: `charge := job.AccruedCredits`, and `0`
   when the owner is unmetered. Pass `charge` to `Repo.DeliverJob`. ⚠ `Repo.DeliverJob` already skips
   both the decrement AND the `credit_entries` insert when the charge is zero — do NOT add a second
   branch in the store, and do not write a zero-delta ledger row: nothing moved, so the audit has
   nothing to record.
4. [S4] Increment `ocrr_credits_debited_total` by the charge actually taken and
   `ocrr_credits_waived_total` by what was waived — never both for the same credit. The existing
   `Add(metricCreditsDebited, nil, int64(job.AccruedCredits))` currently reports the accrual as
   debited unconditionally, which for an unmetered delivery would be the conflation this record
   exists to remove.
5. [S5] Do the same in `DeliverRaw`, where the charge is the flat `rawCost` rather than the accrual.
   ⚠ This is the path that has already been wrong once in this codebase in the other direction — its
   own comment records that falling through to the accrued total charged ZERO for a raw job, "a
   failure in the customer's favour that nothing anywhere would report". Keep asserting the NUMBER.
6. [S6] Confirm the transition log and the admin event are unchanged: a waived delivery is still a
   delivery, and the dashboard must still see it. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/router/ -run 'Unmetered|Waiv' -count=1 2>&1 | tee /tmp/adr9t3a.out && \
  ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/adr9t3a.out && \
  go test ./internal/monitor/ -run 'Waived' -count=1 2>&1 | tee /tmp/adr9t3b.out && \
  ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/adr9t3b.out && \
  go test ./internal/router/ ./internal/monitor/ ./internal/store/ -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestAnUnmeteredDeliveryDebitsNothingAndStillAccrues` | `internal/router/unmetered_test.go` | the record's central claim, asserted on BOTH tables: `users.credits` unchanged, `jobs.accrued_credits` equal to the real cost | — | S1, S3 |
| `TestAnUnmeteredDeliveryWritesNoLedgerRow` | `internal/router/unmetered_test.go` | `credit_entries` has no row for that job — a zero-delta row would keep the balance right and make the audit lie | — | S3 |
| `TestUnmeteredDoesNotWaiveAMeteredDelivery` | `internal/router/unmetered_test.go` | the waiver is conditional; red if the charge becomes 0 for everyone | — | S1, S3 |
| `TestAnUnmeteredRawDeliveryWaivesTheFlatCredit` | `internal/router/unmetered_test.go` | the raw path too, asserted as the number 1 waived and 0 debited | — | S5 |
| `TestWaivedAndDebitedAreNeverBothCounted` | `internal/router/unmetered_test.go` | one delivery moves exactly one of the two counters, by the same amount the other did not get | — | S4 |
| `TestWaivedMetricNameIsTheSameOnBothSides` | `internal/monitor/monitor_test.go` | `monitor.MetricCreditsWaived` is the literal the router test reads the counter by — ⚠ THERE IS NO EXISTING PAIRING ASSERTION in this corpus (checked 2026-09-28: `ocrr_raw_jobs_total` is declared in `internal/router/metrics.go` alone and nothing pins it), so this row WRITES the check rather than leaning on one | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestAnUnmeteredDeliveryDebitsNothingAndStillAccrues` |
| 2 — something selects it | the charge selection in `Deliver` and `DeliverRaw` — the only two callers of `Repo.DeliverJob`; the mutation replaces the conditional charge with `job.AccruedCredits` and `TestAnUnmeteredDeliveryDebitsNothingAndStillAccrues` must go red |
| 3 — the caller can discover it | `ocrr_credits_waived_total` appears in `/metrics` output; `TestWaivedMetricNameIsTheSameOnBothSides` is what stops the two spellings drifting, and it exists because nothing in the registry would notice |
| 4 — it is used | the counter is the measurement: an operator can see what unmetered work costs without it being billed |

## Mutation Log

- 2026-09-28 · 5a51b7a* · mutant killed · exit 1 · `internal/router/service.go` · the units-path waiver is removed, so an unmetered customer is billed again — kills TestAnUnmeteredDeliveryDebitsNothingAndStillAccrues · acceptance-sha256:e6af3c7ad8cbb5848320d0dd466cd1dd0ad838064474c700a97227b5a4200d96 · covers:the zero charge
- 2026-09-28 · 5a51b7a* · mutant killed · exit 1 · `internal/router/service.go` · the WAIVED counter reports 0 instead of what the work cost, so an unmetered customer consumes invisibly — the conflation ADR-0009 exists to remove, in its subtle form · acceptance-sha256:e6af3c7ad8cbb5848320d0dd466cd1dd0ad838064474c700a97227b5a4200d96 · covers:the waived counter
- 2026-09-28 · 5a51b7a* · mutant killed · exit 1 · `internal/store/repo_write.go` · a zero charge writes a zero-delta ledger row, so the audit records a movement that never happened — kills TestAnUnmeteredDeliveryWritesNoLedgerRow · acceptance-sha256:e6af3c7ad8cbb5848320d0dd466cd1dd0ad838064474c700a97227b5a4200d96 · covers:the absent ledger row
- 2026-09-28 · 5a51b7a* · mutant killed · exit 1 · `internal/router/service.go` · the raw path reverts to reporting the flat credit as DEBITED even when it was waived, so ocrr_credits_debited_total stops agreeing with credit_entries — kills TestAnUnmeteredRawDeliveryWaivesTheFlatCredit · acceptance-sha256:e6af3c7ad8cbb5848320d0dd466cd1dd0ad838064474c700a97227b5a4200d96 · covers:the unchanged accrual

## Invariants

- `Repo.DeliverJob` is still the ONLY place credits move for a job, and gains no new branch.
- `jobs.accrued_credits` is computed identically for metered and unmetered customers.
- `ocrr_credits_debited_total` still agrees with the sum of delivery rows in `credit_entries`.
- A waived delivery still logs its transition and still publishes the admin event.

## Risks

- Reading the user inside `Deliver` adds a failure mode to a path that previously could not fail on a
  missing user. Treat a missing owner as the existing error rather than as "metered" — a waiver
  decided by a failed read is a waiver nobody chose.
- A future third delivery path would have to repeat the selection. Both call sites now share
  `Service.countCredits`, which is the one place that decides which series a delivery's money lands
  on; the charge selection itself is still two lines in two functions, because a helper for two
  branches is the abstraction this project's own rules reject.
- ⚠ ONE MUTATION ROW IS LABELLED LOOSELY, and it is recorded here rather than left to read stronger
  than it is. The row `covers:the unchanged accrual` actually mutated the RAW path's counter
  selection (`countCredits(u, rawCost, charge)` → a direct debited `Add`), which is a different
  mechanism. `the unchanged accrual` is a mechanism this task PRESERVES rather than adds — nothing in
  the diff touches how `jobs.accrued_credits` is computed, so no mutation of this task's code can
  break it. It is asserted by `TestAnUnmeteredDeliveryDebitsNothingAndStillAccrues`, and the honest
  statement is "asserted, not mutation-bound".

## Stop Condition

Stop if a stage completion — not a delivery — turns out to debit anywhere: this record assumes the
ONLY movement is on delivery, and `Repo.DeliverJob`'s own comment says so. If that is not true, the
waiver has more than two sites and the Decision needs amending, not the code.

## Out of Scope

- A metric dimension distinguishing waived by label or customer. (permanent: boundary: ADR-0001 task T11's cardinality allow-list exists to stop exactly that, and a per-customer label is unbounded)
- Reconciling historical deliveries made before the flag existed. (permanent: fact: no customer is unmetered before this ADR ships, so there is nothing to reconcile; citation: file `internal/store/migrations/00006_unmetered.sql:1`)

## Verification Log
- 2026-09-28 · 5a51b7a* · exit 1 · `set -o pipefail …` · acceptance-sha256:e6af3c7ad8cbb5848320d0dd466cd1dd0ad838064474c700a97227b5a4200d96 · ms:1986
  ```
  --- last 10 line(s) of stdout (of 14 after folding 14 raw)
  --- FAIL: TestAnUnmeteredRawDeliveryWaivesTheFlatCredit (0.02s)
      unmetered_test.go:247: credits = 99 after an unmetered RAW delivery, want 100
      unmetered_test.go:250: ocrr_credits_debited_total = 1, want 0 — nothing was taken from a balance
      unmetered_test.go:253: ocrr_credits_waived_total = 0, want 1 — a raw job's flat credit was waived and the waiver has to be visible
  --- FAIL: TestWaivedAndDebitedAreNeverBothCounted (0.02s)
      unmetered_test.go:280: ocrr_credits_debited_total = 8, want 3 — only the metered delivery's cost
      unmetered_test.go:283: ocrr_credits_waived_total = 0, want 5 — only the unmetered delivery's cost
  FAIL
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/router	0.584s
  FAIL
  ```
- 2026-09-28 · 5a51b7a* · exit 0 · `set -o pipefail …` · acceptance-sha256:e6af3c7ad8cbb5848320d0dd466cd1dd0ad838064474c700a97227b5a4200d96 · ms:4579
- 2026-09-28 · 5a51b7a* · exit 0 · `set -o pipefail …` · acceptance-sha256:e6af3c7ad8cbb5848320d0dd466cd1dd0ad838064474c700a97227b5a4200d96 · ms:3015
- 2026-09-28 · 5a51b7a* · exit 0 · `set -o pipefail …` · acceptance-sha256:e6af3c7ad8cbb5848320d0dd466cd1dd0ad838064474c700a97227b5a4200d96 · ms:3040
- 2026-09-28 · 5a51b7a* · exit 0 · `set -o pipefail …` · acceptance-sha256:e6af3c7ad8cbb5848320d0dd466cd1dd0ad838064474c700a97227b5a4200d96 · ms:2917
