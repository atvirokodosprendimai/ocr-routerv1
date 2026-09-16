# Task ADR-0006-T5: Store a raw result as a blob and stream it to the client unchanged

**Depends-on:** T3, T4
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** raw result blob keyed by job id, `GET /files/{id}` octet-stream response, `ocrr_result_blobs` gauge
**Consumes:** `jobs.raw` stamped at admission (T3), worker raw result wire shape (T4)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the job's stamped mode overriding the request header`, `the bytes never entering the results store`, `deletion on delivery`, `the reaper covering result blobs`

## Goal

A raw worker's bytes land in the blob store, stream back to the client byte-for-byte, and are
deleted on delivery.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/httpapi/upload.go` | edit | Inside the worker arm, an octet-stream body is streamed to `Router.CompleteRaw`; the role branch is untouched |
| `internal/router/service.go` | edit | `CompleteRaw` — lease check, blob write, state transition; `Deliver` returns a raw handle |
| `internal/blob/store.go` | edit | A result key distinct from the source key, so a raw job's input and output can coexist during a pipeline stage |
| `internal/httpapi/files.go` | edit | `resultToClient` streams octet-stream for a raw job, JSON for a units job |
| `internal/router/reaper.go` | edit | Expiry deletes result blobs; without this they leak for undelivered jobs |
| `internal/router/metrics.go` | edit | `ocrr_result_blobs` gauge beside `ocrr_results_in_memory` |
| `internal/httpapi/raw_test.go` | edit | The failing tests below |

## Ordered Steps

1. [S1] Write the failing tests: `TestRawResultRoundTripsByteForByte` (the PNG magic number survives worker → router → client) and `TestRawResultDeletedOnDelivery`. Confirm red. [proof: acceptance]
2. [S2] Add a result key to `blob.Store` (`<id>.out` or an equivalent that `validID` still guards),
   keeping the same staged-write-and-rename atomicity the source path has.
3. [S3] Add `Service.CompleteRaw(ctx, workerID, jobID, io.Reader, now)`: the SAME lease check as
   `Complete` — `job.WorkerID != workerID` is what stops one customer's worker writing another's
   result — then stream into the result blob, then transition.
4. [S4] In the worker arm of `/upload`, branch on `Content-Type` and refuse a shape that disagrees
   with `jobs.raw`. The stamped mode is the authority; the header selects a representation only.
5. [S5] Stream the body into `CompleteRaw` with `MaxBytesReader`, reusing the pattern commit
   `b84f45a` established rather than reading it into memory.
6. [S6] `resultToClient` serves `application/octet-stream` for a raw job and deletes the blob after
   a successful write, preserving the existing "delivery is idempotent to the holder" property —
   the second call finds no blob and is a 404, not a second charge.
7. [S7] Extend the reaper and the dead-letter path to delete result blobs. [proof: acceptance]
8. [S8] Add `ocrr_result_blobs`, a gauge of result blobs on disk, so the risk this task introduces
   is measured rather than asserted. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/httpapi/... -run 'TestRawResultRoundTripsByteForByte|TestRawResultDeletedOnDelivery|TestRawShapeMismatchIsRefused' -count=1 -v 2>&1 | tee /tmp/acc-0006-T5.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T5.out \
  && go test ./internal/httpapi/... ./internal/router/... ./internal/blob/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestRawResultRoundTripsByteForByte` | `internal/httpapi/raw_test.go` | `\x89PNG\r\n\x1a\n` posted by a worker is served to the client identically — the exact bytes the JSON path corrupts, and the staged-write result key that holds them | — | S2, S3, S5, S6 |
| `TestRawResultDeletedOnDelivery` | `internal/httpapi/raw_test.go` | After a successful collect the result blob is gone and a second collect is 404, not a second charge | — | S6 |
| `TestRawShapeMismatchIsRefused` | `internal/httpapi/raw_test.go` | A worker posting octet-stream for a units job, and JSON units for a raw job, are both refused — the header cannot override the stamped mode | — | S4 |
| `TestRawResultNeverEntersResultsStore` | `internal/router/service_test.go` | The in-memory results store is empty after a raw completion | — | S3 |
| `TestExpiredRawJobDropsItsResultBlob` | `internal/router/reaper_test.go` | An undelivered raw job past its TTL leaves no blob — the leak named in the ADR's risk table | — | S7 |
| `TestLeaseStillGuardsRawCompletion` | `internal/httpapi/raw_test.go` | A worker that does not hold the lease cannot post a raw result for the job | — | S3 |
| `TestUploadBranchesOnRoleNotBody` | `internal/httpapi/api_test.go` | The role branch is unchanged by the new content-type branch inside the worker arm | — | S4 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestRawResultRoundTripsByteForByte` |
| 2 — something selects it | `jobs.raw` selects the octet-stream response in `resultToClient`; deleting that branch serves JSON for a raw job and turns `TestRawResultRoundTripsByteForByte` red — the mutation to record |
| 3 — the caller can discover it | The `Content-Type` response header is the declared interface; T7's client reads it rather than assuming |
| 4 — it is used | `ocrr_result_blobs` (S8) — the first rung-4 answer in this ADR that is not "nothing measures this yet" |

## Mutation Log

## Invariants

- The lease check on a raw completion is identical to the units one. This is the boundary that stops
  a worker writing a job it does not hold, and a second write path is exactly where it gets forgotten.
- A raw result never enters `internal/results`.
- Delivery remains idempotent to the holder: second call is 404, never a second charge.
- `blob.validID` still guards the result key — the id comes from a URL path and is a trust boundary.
- The role branch in `/upload` is unchanged and still first.

## Risks

- Two blob keys per job doubles the paths that must be deleted; a deletion added to one and not the
  other leaks silently. `TestExpiredRawJobDropsItsResultBlob` plus `ocrr_result_blobs` cover it —
  the gauge is what makes a leak visible if a path is missed later.
- The content-type branch inside the worker arm resembles the role-confusion bug the ADR explains
  it is not. Named in the ADR's risk table and guarded by `TestRawShapeMismatchIsRefused`.

## Stop Condition

Stop if the result key cannot be made to satisfy `blob.validID` without loosening it. Loosening a
path-traversal validator to fit a new key is the kind of change that reads as plumbing and is not;
bring it to the owner instead.

## Out of Scope

- Pricing the raw job — T6.
- The client's handling of the octet-stream response — T7.
- Streaming a partial result before the worker finishes (permanent: boundary: named in the ADR's Out of Scope).

## Verification Log
