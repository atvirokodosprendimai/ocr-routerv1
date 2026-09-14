# Task ADR-0001-T6: Make router.Service the one writer of job state and of the credit ledger

**Depends-on:** T2, T4, T5
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `router.Service` — `Upload`, `Claim`, `Complete`, `Fail`, `Deliver`, `Backlog`, `RecoverOnBoot`, `Reap`
**Consumes:** `store.Repo` (T2), `blob.Store` and `results.Store` (T4), `bus.Bus` (T5), `core.*` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the delivery transaction`, `the buffer-limit count`, `the lease clock`

## Goal

Implement the whole job lifecycle and the credit ledger behind one type that is the only
writer, so admission, metering, expiry and requeue cannot disagree with each other.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/router/service.go` | add | the write API — the single writer |
| `internal/router/reaper.go` | add | lease expiry, queue-deadline expiry, result sweep |
| `internal/router/service_test.go` | add | lifecycle, admission and metering tests |
| `internal/router/reaper_test.go` | add | expiry and recovery tests |

`router.Service` is constructed in `cmd/router` (T8) and every handler in T7 holds a
pointer to it; T8's composition-root mutation proves it is reached from the real binary,
which no test in this package can show.

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write
   `service_test.go` asserting that `Deliver` charges exactly once and that `Upload`
   refuses a customer at their buffer limit, before any implementation (TDD red).
   [proof: acceptance]
2. [S2] `Upload(ctx, userID, in UploadInput)` — `in` carries `Filename`, an optional
   `io.Reader`, `Pipeline []string` and `Params map[string]string`. In order: load the user;
   refuse with `core.ErrNoCredits` if `credits <= 0`; refuse with `core.ErrBufferFull` if
   `CountInFlight(user) >= buffer_limit`; validate every param key with `core.ValidParamKey`;
   validate `Pipeline[0]` against the **live label registry** and refuse an unknown one with
   `core.ErrNotFound` carrying the available labels; mint a uuidv7; when a reader is present
   `blob.Put` **before** the row, so a row can never name a blob that does not exist, and set
   `has_blob` accordingly — a params-only crawler job has no blob and that is normal, not an
   error; insert the job with `queued_at = now`, `expires_at = now + job_ttl_secs` (NULL when
   `job_ttl_secs = 0`), `label = Pipeline[0]`, `stage = 0`; then publish `work` to
   `workers:<label>`. **Persist before publish** — a publish on a failed write shows workers
   a job that is not there.
3. [S3] `Claim(ctx, workerID, label)` calls `Repo.ClaimOneQueued(now, agingStep, label,
   workerID, now+lease)` and returns the job or `core.ErrNotFound`. The service adds no
   read-then-write of its own: the ordering, the label filter and the deadline all live in
   T2's single statement.
4. [S4] `Complete(ctx, workerID, jobID, out []string)` — verify the caller **holds the
   lease** (`worker_id` matches and the lease has not expired), else `core.ErrConflict`.
   Then branch on the pipeline:
   - **Not the last stage** — write `out` as the next stage's input blob (one element
     verbatim, several joined by `\n`), call `Repo.AdvanceStage(jobID, pipeline[stage+1],
     len(out) * RateForLabel(label))`, delete the previous stage's blob, and publish `work`
     to `workers:<next label>`. **No `ready`, and the client is told nothing** — a pipeline
     is one job from the client's side.
   - **Last stage** — `results.Put(jobID, out)`; set `state='done'`,
     `pages=len(out)`, accrue this stage's cost; publish `ready` to `user:<owner>`.

   A worker whose lease expired mid-run is refused here, because the job has already been
   requeued and may be running elsewhere.
5. [S5] `Fail(ctx, workerID, jobID, reason)` — lease-checked as in S4; increment
   `attempts`; if `attempts < maxAttempts` return the job to `queued` (leaving
   `queued_at` **unchanged**, so a retried job keeps the age it has already accrued and
   does not go to the back of the aged queue); else set `dead`, delete the blob, publish
   `failed` to the owner.
6. [S6] `Deliver(ctx, userID, jobID)` — the metering step, and the only one that charges.
   Verify ownership; `results.Take(jobID)`; then in **one immediate transaction**: insert a
   `credit_entries` row of `-accrued_credits`, decrement `users.credits` by the same, and set
   `state='delivered'`. The charge is the **sum accrued across every stage**, so a pipeline
   is billed once for all of its stages at their own rates. Only after it commits, delete the
   blob. A second `Deliver` finds no result and returns `core.ErrNotFound`, so it cannot
   charge twice.
7. [S7] `Backlog(ctx, userID)` reads every `done` job for the user and returns
   `{job_id, pages}` for each — what a reconnecting client is sent so it can fetch what it
   missed.
8. [S8] `RecoverOnBoot(ctx)` resets every `processing` and `done` job to `queued`, because
   leases and in-memory results died with the process. It charges nothing and deletes no
   blob; the work is simply redone.
9. [S9] `Reap(ctx, now)` — one pass doing three things: requeue jobs whose lease expired;
   `ExpireOverdue` for queued jobs past their deadline, publishing `failed` with
   `reason:"expired"` to each owner; and `results.Sweep`, returning each swept job to
   `queued`. Driven by a ticker in `cmd/router` (T8), and called directly by the tests so
   nothing sleeps.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/router/... -count=1 -race 2>&1 | tee /tmp/adr1-t6.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t6.out \
  && go test ./internal/store/... ./internal/results/... ./internal/bus/... -count=1 2>&1 | tee /tmp/adr1-t6r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t6r.out
```

The new package runs first and alone so it can carry the verdict; its three dependencies
run second as regression. Red at authoring: `internal/router` does not exist.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestUploadRefusesWithoutCredits` | `internal/router/service_test.go` | `credits == 0` is `core.ErrNoCredits`, and no blob and no row are created | — | S2 |
| `TestUploadRefusesAtBufferLimit` | `internal/router/service_test.go` | the `(limit+1)`-th in-flight upload is `core.ErrBufferFull`, and the limit counts `queued`+`processing`+`done` | — | S2 |
| `TestUploadBufferLimitFreesOnDelivery` | `internal/router/service_test.go` | after delivering one job the customer can upload again — the limit is in-flight, not lifetime | — | S2, S6 |
| `TestUploadPublishesWorkAfterPersisting` | `internal/router/service_test.go` | a work event is published only once the row is readable — persist before publish | — | S2 |
| `TestUploadSetsDeadlineFromUserTTL` | `internal/router/service_test.go` | `expires_at = queued_at + job_ttl_secs`, and is NULL when the TTL is 0 | — | S2 |
| `TestClaimLeasesOneQueuedJob` | `internal/router/service_test.go` | `Claim` returns a job now in `processing` with the lease set to `now+lease` and `worker_id` set | — | S3 |
| `TestClaimOnEmptyQueue` | `internal/router/service_test.go` | with nothing claimable, `Claim` is `core.ErrNotFound` and writes nothing | — | S3 |
| `TestCompleteRequiresLease` | `internal/router/service_test.go` | a worker that does not hold the lease gets `core.ErrConflict` and the result is not stored | — | S4 |
| `TestCompleteRejectsExpiredLease` | `internal/router/service_test.go` | the original worker is refused after its lease expired and the job was requeued | — | S4, S9 |
| `TestDeliverChargesOncePerUnit` | `internal/router/service_test.go` | a 7-unit result debits exactly 7 | — | S6 |
| `TestDeliverTwiceChargesOnce` | `internal/router/service_test.go` | the second `Deliver` is `core.ErrNotFound` and the balance is unchanged — the double-charge guard | — | S6 |
| `TestDeliverIsAtomicUnderRace` | `internal/router/service_test.go` | N concurrent `Deliver` calls for one job produce exactly one success and one ledger row | — | S6 |
| `TestDeliverRefusesOtherUsersJob` | `internal/router/service_test.go` | customer B cannot deliver customer A's job, and A is not charged | — | S6 |
| `TestDeliverDeletesBlobAfterCommit` | `internal/router/service_test.go` | the blob is gone after delivery, and still present if the transaction failed | — | S6 |
| `TestDeliverMayOverdraw` | `internal/router/service_test.go` | a result longer than the balance still delivers and the balance goes negative by exactly the shortfall — the accepted overshoot, asserted rather than left to chance | — | S6 |
| `TestUploadRejectsUnknownLabel` | `internal/router/service_test.go` | a label no live worker advertises is refused at upload with the available labels named, rather than queued to sit until its deadline | — | S2 |
| `TestUploadRejectsBadParamKey` | `internal/router/service_test.go` | a param key failing `core.ValidParamKey` refuses the whole upload and writes no blob and no row | — | S2 |
| `TestUploadWithoutBlobIsValid` | `internal/router/service_test.go` | a params-only crawler job is created with `has_blob=false`, no blob written, and is claimable | — | S2 |
| `TestPipelineAdvancesWithoutNotifyingClient` | `internal/router/pipeline_test.go` | completing stage 0 of a two-stage job requeues it under the next label and publishes **zero** client events, and the whole pipeline publishes exactly **one** `ready` — counted, because "a ready was published" passes in both arrangements | — | S4 |
| `TestPipelineIntermediateBecomesNextInput` | `internal/router/pipeline_test.go` | stage 0's output is the blob stage 1 is handed, verbatim, and `has_blob` flips for a job that started without one | — | S4 |
| `TestPipelineMultiElementIntermediateIsJoined` | `internal/router/pipeline_test.go` | a multi-element intermediate is newline-joined — the documented, lossy encoding choice | — | S4 |
| `TestPipelinePreservesQueuedAtAcrossStages` | `internal/router/pipeline_test.go` | a job at stage 1 keeps its original `queued_at`, so it is preferred over freshly uploaded work | — | S4 |
| `TestPipelineAccruesPerStageRate` | `internal/router/pipeline_test.go` | crawl at rate 0 then OCR at rate 2 over 3 pages charges 6, in ONE ledger row equal to `accrued_credits` rather than to `len(result)` | — | S4, S6 |
| `TestSingleStageUsesDefaultRateOfOne` | `internal/router/pipeline_test.go` | an unconfigured service costs 1 per unit — the operator's original "1 page = 1 credit" | — | S6 |
| `TestFailRequeuesUntilMaxAttempts` | `internal/router/service_test.go` | attempts 1..N-1 requeue and attempt N is `dead` with the blob deleted | — | S5 |
| `TestFailPreservesQueuedAt` | `internal/router/service_test.go` | a requeued job keeps its original `queued_at`, so a retry does not lose its accrued age | — | S5 |
| `TestBacklogListsCollectableResults` | `internal/router/service_test.go` | only this user's `done` jobs are listed, and a job whose result was swept is omitted rather than promised | — | S7 |
| `TestRecoverOnBootRequeues` | `internal/router/reaper_test.go` | `processing` and `done` become `queued`, nothing is charged, no blob is deleted | — | S8 |
| `TestReapRequeuesExpiredLease` | `internal/router/reaper_test.go` | a job whose lease passed is `queued` again with `attempts` incremented | — | S9 |
| `TestReapExpiresOverdueQueued` | `internal/router/reaper_test.go` | an overdue `queued` job becomes `expired`, is not charged, its blob is deleted, and its owner receives `failed` with `reason:"expired"` | — | S9 |
| `TestReapLeavesProcessingPastDeadline` | `internal/router/reaper_test.go` | a `processing` job past its deadline is **not** expired — work already started runs to completion | — | S9 |
| `TestReapSweepsResultsAndRequeues` | `internal/router/reaper_test.go` | an expired result returns its job to `queued` rather than stranding it in `done` | — | S9 |
| `TestReapAbandonsAfterMaxAttempts` | `internal/router/reaper_test.go` | three lapsed leases exhaust the attempt budget and the job becomes `dead` | — | S9 |
| `TestRecoverOnBootWakesWorkers` | `internal/router/reaper_test.go` | boot recovery publishes work for every label that now has a queue, so recovered jobs do not sit until the next upload | — | S8 |
| `TestAvailableLabelsUsesGraceWindow` | `internal/router/service_test.go` | a label stays valid inside `LabelGrace` after its last worker disconnects and is gone after it — the rolling-restart guard | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the thirty-two tests above |
| 2 — something selects it | every T7 handler calls a `Service` method, and `cmd/router` constructs it and starts the `Reap` ticker (T8). T8's mutation deletes the ticker and `TestReaperRunsInBinary` goes red — the composition root is the one thing no unit test here watches. |
| 3 — the caller can discover it | n/a: no declared interface — an internal Go package |
| 4 — it is used | T8's end-to-end test drives upload → claim → complete → deliver and asserts the balance moved |

## Mutation Log

- 2026-09-15 · e9329c4* · mutant killed · exit 1 · `internal/router/service.go` · A pipeline is one job to the client: only the LAST stage may publish ready, or the client collects a half-processed intermediate as the finished result. · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · covers:the delivery transaction
- 2026-09-15 · e9329c4* · mutant killed · exit 1 · `internal/router/service.go` · A multi-stage job must be billed for the sum accrued across stages at each stage's rate, not for the size of the final output. · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00
- 2026-09-15 · e9329c4* · mutant killed · exit 1 · `internal/router/service.go` · The buffer limit is the back-pressure that stops one customer monopolising the worker pool. · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · covers:the buffer-limit count
- 2026-09-15 · e9329c4* · mutant killed · exit 1 · `internal/router/service.go` · A worker whose lease lapsed may have had its job requeued and run elsewhere; accepting its late result delivers one worker's output for a job another is still doing. · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · covers:the lease clock
- 2026-09-15 · e9329c4* · mutant killed · exit 1 · `internal/router/reaper.go` · An expiry nobody is told about is indistinguishable from a job still waiting, which is the worst of both states. · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00
- 2026-09-15 · e9329c4* · mutant killed · exit 1 · `internal/router/reaper.go` · A swept result whose job is not requeued strands that job in done forever with nothing to serve. · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00

## Invariants

- `router.Service` is the **only** writer of `jobs`, `credit_entries` and `users.credits`.
- Persist before publish, on every path.
- A charge happens exactly once per job, in the same transaction as `done → delivered`.
- Nothing is charged for `queued`, `processing`, `dead` or `expired`.
- A blob outlives its job until `delivered`, `dead` or `expired`.
- `queued_at` is set once at upload and never rewritten, including on requeue.
- Expiry applies only to `queued`.

## Risks

- **`TestDeliverTwiceChargesOnce` is the test most likely to pass for the wrong reason** —
  if `Deliver` happens to fail early for an unrelated reason the balance is also unchanged.
  It must assert the specific `core.ErrNotFound` **and** the ledger row count, not the
  balance alone.
- **The delivery transaction spanning the blob delete.** A filesystem operation inside a
  database transaction can leave the two disagreeing. S6 deletes the blob strictly after
  the commit; the worst case is an orphan blob, which is recoverable, rather than a
  delivered-and-unreadable job, which is not.
- **`Reap` doing three things** is a cohesion risk. Kept as one pass because all three are
  clock-driven sweeps over the same aggregate and splitting them would mean three tickers
  racing to write the same rows — which is the two-writer bug.

## Stop Condition

Stop and ask if the operator wants a *dead* job's credits handled differently — the ADR
charges nothing for work that never reached the customer, which means a document that fails
OCR N times costs the customer nothing and the operator N attempts of worker time.

## Out of Scope

- HTTP shapes and status codes — T7's.
- The ticker that drives `Reap` — T8 owns the composition root; this task exposes `Reap`.
- Notifying the admin dashboard of state changes — T10 subscribes to the same bus.

## Verification Log
- 2026-09-15 · e9329c4* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · ms:6069
- 2026-09-15 · e9329c4* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · ms:4318
- 2026-09-15 · e9329c4* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · ms:4129
- 2026-09-15 · e9329c4* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · ms:3953
- 2026-09-15 · e9329c4* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · ms:4522
- 2026-09-15 · e9329c4* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · ms:4298
- 2026-09-15 · e9329c4* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · ms:4189
- 2026-09-15 · e54cc8f* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5d29feaefe3875a6935efbb17846d060f4ad142571ebd7534f865eb2b5abc00 · ms:5762
