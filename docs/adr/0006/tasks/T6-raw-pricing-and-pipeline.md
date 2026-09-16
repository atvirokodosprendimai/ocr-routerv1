# Task ADR-0006-T6: Charge a raw job a flat credit on delivery and bridge a raw stage without joinUnits

**Depends-on:** T5
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** flat-1 raw pricing, raw stage bridge, `ocrr_raw_jobs` counter
**Consumes:** `jobs.raw` (T3), raw result blob (T5)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the flat price replacing len(units) × rate`, `the charge landing on delivery`, `the stage bridge moving the blob rather than joining units`

## Goal

A raw job costs exactly 1 credit, debited on delivery, and a raw stage's output blob becomes the
next stage's input without passing through `joinUnits`.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/router/service.go` | edit | `Deliver` prices a raw job at 1; the stage-advance path moves the result blob to the next stage's input |
| `internal/router/metrics.go` | edit | `ocrr_raw_jobs` counter |
| `internal/router/service_test.go` | edit | Pricing tests |
| `internal/router/pipeline_test.go` | edit | Stage-bridge tests |

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
go test ./internal/router/... -run 'TestRawJobCostsOneCreditRegardlessOfSize|TestRawStageBridgesWithoutJoinUnits|TestFailedRawJobCostsNothing' -count=1 -v 2>&1 | tee /tmp/acc-0006-T6.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T6.out \
  && go test ./internal/router/... ./internal/httpapi/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestRawJobCostsOneCreditRegardlessOfSize` | `internal/router/service_test.go` | A 1-byte and a 10 MB raw result both debit exactly 1 credit; a units job at the same label is unaffected | — | S2 |
| `TestFailedRawJobCostsNothing` | `internal/router/service_test.go` | A raw job that fails, is abandoned, or expires debits nothing — the charge-on-delivery rule | — | S2, S6 |
| `TestRawStageBridgesWithoutJoinUnits` | `internal/router/pipeline_test.go` | A raw stage's exact bytes become the next stage's input blob; `joinUnits` is not called | — | S3 |
| `TestRawStageModeMismatchFailsTheJob` | `internal/router/pipeline_test.go` | Advancing into a stage whose label's mode disagrees fails the job with a named reason instead of queueing it forever | — | S4 |
| `TestUnitsPricingUnchanged` | `internal/router/service_test.go` | A non-raw job still costs `len(units) * rate` across multi-stage accrual | — | S2 |
| `TestRawJobsCounterIncrementsOnDelivery` | `internal/router/metrics_test.go` | `ocrr_raw_jobs` moves on raw delivery only | — | S5 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestRawJobCostsOneCreditRegardlessOfSize` |
| 2 — something selects it | `job.Raw` in `Deliver` selects the flat price; deleting that branch charges `len(units) * rate` for a raw job — where `units` is empty, so it charges ZERO, which is the mutation worth recording because it fails silently in the customer's favour and would never be reported |
| 3 — the caller can discover it | The flat price is documented in the client README and on the admin service row (T8); a price a customer cannot look up is not a price |
| 4 — it is used | `ocrr_raw_jobs` (S5) |

## Mutation Log

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
