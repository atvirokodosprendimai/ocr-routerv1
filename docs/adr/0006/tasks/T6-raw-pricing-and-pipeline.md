# Task ADR-0006-T6: Charge a raw job a flat credit on delivery and bridge a raw stage without joinUnits

**Depends-on:** T5
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** flat-1 raw pricing, raw stage bridge, `ocrr_raw_jobs` counter
**Consumes:** `jobs.raw` (T3), raw result blob (T5)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the flat price replacing len(units) × rate`, `the stage bridge moving the blob rather than joining units`, `the next stage's mode being validated`

## Goal

A raw job costs exactly 1 credit, debited on delivery, and a raw stage's output blob becomes the
next stage's input without passing through `joinUnits`.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/router/service.go` | edit | `DeliverRaw` charges a flat credit; `bridgeRawStage` moves the result blob onto the next stage's input key and validates that stage's mode |
| `internal/router/metrics.go` | edit | `ocrr_raw_jobs_total`, labelled by service only |
| `internal/router/rawpricing_test.go` | add | Pricing tests |
| `internal/router/rawpipeline_test.go` | add | Stage-bridge, mismatch and counter tests |

## Ordered Steps

1. [S1] Write the failing tests: `TestRawJobCostsOneCreditRegardlessOfSize` and `TestRawStageBridgesWithoutJoinUnits`. Confirm red. [proof: acceptance]
2. [S2] In `Deliver`, branch on `job.Raw`: cost is 1, not `len(units) * rate`. Leave the debit at the
   same point in the same transaction — this task changes the AMOUNT, never the moment.
3. [S3] In the stage-advance path, when the completed stage was raw, rename/move the result blob to
   the next stage's input key instead of calling `joinUnits`. `joinUnits` itself is untouched and
   still serves units stages.
4. [S4] Validate a later raw stage when the job advances into it: the next label's
   `ServiceMode` must match what the next stage will produce, and a mismatch fails the job with a
   readable reason rather than queueing it forever.
5. [S5] Add `ocrr_raw_jobs`, incremented on raw delivery, with the label as its only dimension —
   inside T11's existing cardinality allow-list, not beside it. [proof: acceptance]
6. [S6] Confirm a failed or expired raw job debits nothing, which is the ledger rule this task must
   not bend. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/router -run 'TestRawJobCostsOneCreditRegardlessOfSize|TestFailedRawJobCostsNothing|TestUnitsPricingUnchanged|TestRawStageBridgesWithoutJoinUnits|TestRawStageModeMismatchFailsTheJob|TestRawJobsCounterIncrementsOnDelivery' -count=1 -v 2>&1 | tee /tmp/acc-0006-T6.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T6.out \
  && go test ./internal/router/... ./internal/httpapi/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestRawJobCostsOneCreditRegardlessOfSize` | `internal/router/rawpricing_test.go` | A 1-byte and a 1 MiB raw result both debit EXACTLY 1, against a service whose per-unit rate is 5 — so both the 0 of an empty unit list and the 5 of a single unit are caught | — | S2 |
| `TestFailedRawJobCostsNothing` | `internal/router/rawpricing_test.go` | A raw job that never delivers debits nothing — the charge-on-delivery rule | — | S2, S6 |
| `TestUnitsPricingUnchanged` | `internal/router/rawpricing_test.go` | A 4-unit job at rate 3 still costs 12 | — | S2 |
| `TestRawStageBridgesWithoutJoinUnits` | `internal/router/rawpipeline_test.go` | Stage 1's bytes — containing an embedded NEWLINE and a non-UTF-8 lead — are stage 2's input byte for byte; joinUnits would mangle the first and JSON the second | — | S3 |
| `TestRawStageModeMismatchFailsTheJob` | `internal/router/rawpipeline_test.go` | Advancing into a units stage fails the job with a reason NAMING that stage, instead of queueing it under a label whose every worker is refused | — | S4 |
| `TestRawJobsCounterIncrementsOnDelivery` | `internal/router/rawpipeline_test.go` | `ocrr_raw_jobs_total{label}` moves once on a raw delivery | — | S5 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestRawJobCostsOneCreditRegardlessOfSize` |
| 2 — something selects it | `job.Raw` in `Deliver` selects the flat price; deleting that branch charges `len(units) * rate` for a raw job — where `units` is empty, so it charges ZERO, which is the mutation worth recording because it fails silently in the customer's favour and would never be reported |
| 3 — the caller can discover it | The flat price is documented in the client README and on the admin service row (T8); a price a customer cannot look up is not a price |
| 4 — it is used | `ocrr_raw_jobs` (S5) |

## Mutation Log

- 2026-09-16 · 2d6794c* · mutant killed · exit 1 · `internal/router/service.go` · a raw job falls back to the accrued per-unit total, which for a job with no units is ZERO — a silent failure in the customer favour that nothing reports · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · covers:the flat price replacing len(units) × rate
- 2026-09-16 · 2d6794c* · mutant inconclusive · exit 1 · `internal/router/service.go` · a raw stage may advance into a units stage, queueing the job under a label whose every worker the mode check refuses — stalled with nothing saying why · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · covers:the stage bridge moving the blob rather than joining units
  ```
  the fence failed on a build/parse error, not an assertion
  ```
- 2026-09-16 · 2d6794c* · mutant killed · exit 1 · `internal/router/service.go` · the bridge delivers a TRUNCATED payload to the next stage — the silent shortening a raw stream has no syntax to reveal · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · covers:the stage bridge moving the blob rather than joining units
- 2026-09-16 · 2d6794c* · mutant killed · exit 1 · `internal/router/service.go` · a raw stage advances into a UNITS stage, so the job queues under a label whose every worker the mode check refuses — stalled forever with nothing saying why · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · covers:the charge landing on delivery
- 2026-09-16 · 2d6794c* · mutant killed · exit 1 · `internal/router/service.go` · a raw stage advances into a UNITS stage, so the job queues under a label whose every worker the mode check refuses — stalled forever with nothing saying why · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · covers:the next stage's mode being validated
- 2026-09-16 · 2d6794c* · mutant killed · exit 1 · `internal/router/service.go` · a raw job falls back to the accrued per-unit total, which with no units is ZERO — a silent failure in the customer favour · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · covers:the flat price replacing len(units) × rate
- 2026-09-16 · 2d6794c* · mutant killed · exit 1 · `internal/router/service.go` · the bridge delivers a TRUNCATED payload to the next stage — the silent shortening a raw stream has no syntax to reveal · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · covers:the stage bridge moving the blob rather than joining units

## Invariants

- The charge moment does not change: on delivery, in the same transaction, for both modes.
- A raw job debits exactly 1 credit, never 0 and never size-dependent.
- `joinUnits` is unchanged and still bridges units stages.
- Metric cardinality stays inside T11's allow-list.

## Risks

- The obvious implementation — letting a raw job fall through to `len(units) * rate` with an empty
  units slice — charges 0 and looks like it works. It is the survived-mutant case this task must
  kill, and `TestRawJobCostsOneCreditRegardlessOfSize` asserts the 1 rather than merely asserting
  "a charge happened".
- A raw stage feeding a units stage produces a blob the next tool may reject. Accepted in the ADR's
  risk table; S4 makes a MODE mismatch fail fast, but the tool's own content expectations are the
  operator's composition to get right.

## Stop Condition

Stop if the multi-stage accrual column (`jobs`, the running cost across completed stages) cannot
express a mixed pipeline — some stages priced per unit, one priced flat. That is a pricing model
question for the owner, not an implementation detail to settle here.

## Out of Scope

- Per-unit pricing for raw services (permanent: boundary: raw means the router does not split the output, and a per-unit price requires units).
- The admin control that marks a service raw — T8.

## Verification Log
- 2026-09-16 · 2d6794c* · exit 1 · `set -o pipefail …` · acceptance-sha256:9c3ac8994bc2541fb9630f5b4144ce20a3d3e1dcc22bc6156e7ccdf58bc2b813 · ms:942
  ```
  --- last 9 line(s) of stdout
  === RUN   TestRawJobCostsOneCreditRegardlessOfSize
      rawpricing_test.go:59: a 1-byte raw job cost 0 credit(s), want exactly 1 — the service's rate is 5 per unit and a raw job has no units, so both 0 and 5 are wrong
      rawpricing_test.go:59: a 1048576-byte raw job cost 0 credit(s), want exactly 1 — the service's rate is 5 per unit and a raw job has no units, so both 0 and 5 are wrong
  --- FAIL: TestRawJobCostsOneCreditRegardlessOfSize (0.05s)
  === RUN   TestFailedRawJobCostsNothing
  --- PASS: TestFailedRawJobCostsNothing (0.01s)
  FAIL
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/router	0.432s
  FAIL
  ```
- 2026-09-16 · 2d6794c* · exit 0 · `set -o pipefail …` · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · ms:4913
- 2026-09-16 · 2d6794c* · exit 0 · `set -o pipefail …` · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · ms:4876
- 2026-09-16 · 2d6794c* · exit 0 · `set -o pipefail …` · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · ms:5576
- 2026-09-16 · 2d6794c* · exit 0 · `set -o pipefail …` · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · ms:4647
- 2026-09-16 · 2d6794c* · exit 0 · `set -o pipefail …` · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · ms:5854
- 2026-09-16 · 2d6794c* · exit 0 · `set -o pipefail …` · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · ms:4853
- 2026-09-16 · 2d6794c* · exit 0 · `set -o pipefail …` · acceptance-sha256:94be1b3ff21c899380dfd72734a030793482397e393435226dad622b1bfb2574 · ms:4622
