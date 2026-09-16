# Task ADR-0006-T5: Store a raw result as a blob and stream it to the client unchanged

**Depends-on:** T3, T4
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** raw result blob keyed by job id, `GET /files/{id}` octet-stream response, `ocrr_result_blobs` gauge
**Consumes:** `jobs.raw` stamped at admission (T3), worker raw result wire shape (T4)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the job's stamped mode overriding the request header`, `the bytes never entering the results store`, `deletion on delivery`, `the reaper sweeping raw results`

## Goal

A raw worker's bytes land in the blob store, stream back to the client byte-for-byte, and are
deleted on delivery.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/httpapi/upload.go` | edit | Inside the worker arm, an octet-stream body streams to `CompleteRaw`; the role branch is untouched |
| `internal/router/service.go` | edit | `CompleteRaw`, `DeliverRaw`, `DropRawResult`, the symmetric mode guard in `Complete`, and `Config.ResultTTL` |
| `internal/blob/store.go` | edit | `PutResult`/`OpenResult`/`DeleteResult` on a `.out` key, with `putAt` factored out so the source and result blobs cannot drift apart on the staged-write guarantee |
| `internal/httpapi/files.go` | edit | `resultToClient` streams octet-stream for a raw job, JSON for a units job |
| `internal/router/reaper.go` | edit | Both blob keys deleted on expiry, AND a new sweep for raw results — see the amendment on S7 |
| `internal/store/repo.go` | edit | `RawJobsDoneBefore`, the query that sweep needs |
| `cmd/router/wire.go` | edit | `ResultTTL` into `router.Config`, so the two result kinds share one window |
| `internal/httpapi/rawresult_test.go` | add | End-to-end tests |
| `internal/router/rawresult_test.go` | add | The two blob-leak tests |
| `internal/httpapi/helpers_test.go` | edit | `mintWorker`, so a test can act as a worker holding no lease |
| `internal/router/service_test.go`, `internal/httpapi/api_test.go`, `internal/web/handlers_test.go` | edit | `ResultTTL` in each harness, or the new sweep is disabled in every test |

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
7. [S7] ⚠ **AMENDED DURING EXECUTION — the reaper needed a NEW SWEEP, not just an extra delete.**
   The step said "extend the reaper and the dead-letter path to delete result blobs", which assumed
   an existing sweep would find raw jobs. It cannot: the result sweep iterates the IN-MEMORY result
   store, and a raw result never enters it — so a completed raw job nobody collects was not merely
   leaking a file, it was STRANDED in `done` forever. Added `Repo.RawJobsDoneBefore`,
   `Config.ResultTTL`, and a second sweep that requeues such a job exactly as the units sweep does.
   Found by `TestExpiredRawJobDropsItsResultBlob`, which was written for the leak and exposed the
   stranding. [proof: acceptance]
8. [S8] Add `ocrr_raw_results_pending`, a gauge of raw results awaiting collection. ⚠ Renamed from
   the ADR's `ocrr_result_blobs` and counted from the JOB TABLE rather than by walking the blob
   directory: the number an operator needs is how many results are waiting, and a directory walk on
   every scrape would cost more the worse the problem got. Nil-hook means the sample is OMITTED
   rather than reported as zero — "not measured" and "none" are different claims. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/httpapi ./internal/router -run 'TestRawResultRoundTripsByteForByte|TestRawResultDeletedOnDelivery|TestRawShapeMismatchIsRefused|TestLeaseStillGuardsRawCompletion|TestRawResultNeverEntersResultsStore|TestExpiredRawJobDropsItsResultBlob|TestDeadRawJobDropsItsResultBlob' -count=1 -v 2>&1 | tee /tmp/acc-0006-T5.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T5.out \
  && go test ./internal/httpapi/... ./internal/router/... ./internal/blob/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestRawResultRoundTripsByteForByte` | `internal/httpapi/rawresult_test.go` | `\x89PNG\r\n\x1a\n` posted by a worker is served to the client identically, with `Content-Type: application/octet-stream` — the exact bytes the JSON path corrupts | — | S2, S3, S5, S6 |
| `TestRawResultDeletedOnDelivery` | `internal/httpapi/rawresult_test.go` | After a successful collect the second is 404 AND no `.out` file remains — the file check is separate because the 404 comes from the done→delivered transition and holds with the blob still on disk | — | S6 |
| `TestRawShapeMismatchIsRefused` | `internal/httpapi/rawresult_test.go` | A worker posting octet-stream for a units job, and JSON units for a raw job, are both refused — the header cannot override the stamped mode | — | S4 |
| `TestRawResultNeverEntersResultsStore` | `internal/httpapi/rawresult_test.go` | The in-memory results store is empty after a raw completion | — | S3 |
| `TestExpiredRawJobDropsItsResultBlob` | `internal/router/rawresult_test.go` | An undelivered raw job past the result TTL leaves no `.out` file, counted by WALKING THE DIRECTORY rather than asking the component that should have deleted it | — | S7 |
| `TestDeadRawJobDropsItsResultBlob` | `internal/router/rawresult_test.go` | The dead-letter path drops it too — two terminal paths, two tests, because the whole risk is that one is forgotten | — | S7 |
| `TestLeaseStillGuardsRawCompletion` | `internal/httpapi/rawresult_test.go` | A SECOND worker, holding no lease, cannot complete the job — and the rightful holder still can, so the guard does not simply refuse everything | — | S3 |

<!-- TestUploadBranchesOnRoleNotBody (internal/httpapi/api_test.go) is deliberately NOT a row here.
It is pre-existing, it runs in this fence's REGRESSION segment, and it is not a test T5 adds —
listing it would claim ownership of coverage that already existed. That it stays green IS the
evidence that the new Content-Type branch did not disturb the role branch; what T5 owns is the
stamped-mode pair, proved by TestRawShapeMismatchIsRefused and its killed mutant. -->

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestRawResultRoundTripsByteForByte` |
| 2 — something selects it | `jobs.raw` selects the octet-stream response in `resultToClient`; deleting that branch serves JSON for a raw job and turns `TestRawResultRoundTripsByteForByte` red — the mutation to record |
| 3 — the caller can discover it | The `Content-Type` response header is the declared interface; T7's client reads it rather than assuming |
| 4 — it is used | `ocrr_result_blobs` (S8) — the first rung-4 answer in this ADR that is not "nothing measures this yet" |

## Mutation Log

- 2026-09-16 · 567bec1* · mutant killed · exit 1 · `internal/router/service.go` · a worker may post raw bytes for a UNITS job, so the Content-Type header chooses the shape instead of the stamped mode — the role-confusion failure one level down · acceptance-sha256:67ac00cb9c7f909888fdf3cefd9fc12da3d12e15832a6c47ee0979ad68aacda5 · covers:the job's stamped mode overriding the request header
- 2026-09-16 · 567bec1* · mutant killed · exit 1 · `internal/router/service.go` · the raw result goes into the in-memory store as a string — the +RAM risk ADR-0001 already meters, and the string is one step from the JSON that corrupts it · acceptance-sha256:67ac00cb9c7f909888fdf3cefd9fc12da3d12e15832a6c47ee0979ad68aacda5 · covers:the bytes never entering the results store
- 2026-09-16 · 567bec1* · mutant survived · exit 0 · `internal/httpapi/files.go` · a delivered raw result stays on disk, so a second collect succeeds and the customer is charged twice for one job · acceptance-sha256:67ac00cb9c7f909888fdf3cefd9fc12da3d12e15832a6c47ee0979ad68aacda5 · covers:deletion on delivery
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```
- 2026-09-16 · 567bec1* · mutant killed · exit 1 · `internal/router/reaper.go` · a completed raw job nobody collects is never swept — it sits in done forever with its output blob on disk, because a raw result never enters the in-memory store the other sweep iterates · acceptance-sha256:67ac00cb9c7f909888fdf3cefd9fc12da3d12e15832a6c47ee0979ad68aacda5 · covers:the reaper sweeping raw results
- 2026-09-16 · 567bec1* · mutant killed · exit 1 · `internal/httpapi/files.go` · a delivered raw result stays on disk forever: the 404 on a second collect comes from the done→delivered transition, so nothing else notices, and delivery is the success path no reaper sweeps · acceptance-sha256:67ac00cb9c7f909888fdf3cefd9fc12da3d12e15832a6c47ee0979ad68aacda5 · covers:deletion on delivery

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
- 2026-09-16 · 567bec1* · exit 1 · `set -o pipefail …` · acceptance-sha256:f40c74813a4309c182ea64680ce9817bfa02dd4ab201080bcecbed36d0fbf802 · ms:1010
  ```
  --- last 10 line(s) of stdout (of 16 after folding 16 raw)
  === RUN   TestRawShapeMismatchIsRefused
  === RUN   TestRawShapeMismatchIsRefused/octet-stream_for_a_units_job
  === RUN   TestRawShapeMismatchIsRefused/units_JSON_for_a_raw_job
      rawresult_test.go:121: a worker posted units for a RAW job and was accepted
  --- FAIL: TestRawShapeMismatchIsRefused (0.03s)
      --- PASS: TestRawShapeMismatchIsRefused/octet-stream_for_a_units_job (0.01s)
      --- FAIL: TestRawShapeMismatchIsRefused/units_JSON_for_a_raw_job (0.01s)
  FAIL
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/httpapi	0.513s
  FAIL
  ```
- 2026-09-16 · 567bec1* · exit 0 · `set -o pipefail …` · acceptance-sha256:67ac00cb9c7f909888fdf3cefd9fc12da3d12e15832a6c47ee0979ad68aacda5 · ms:5893
- 2026-09-16 · 567bec1* · exit 0 · `set -o pipefail …` · acceptance-sha256:67ac00cb9c7f909888fdf3cefd9fc12da3d12e15832a6c47ee0979ad68aacda5 · ms:4976
- 2026-09-16 · 567bec1* · exit 0 · `set -o pipefail …` · acceptance-sha256:67ac00cb9c7f909888fdf3cefd9fc12da3d12e15832a6c47ee0979ad68aacda5 · ms:4884
- 2026-09-16 · 567bec1* · exit 0 · `set -o pipefail …` · acceptance-sha256:67ac00cb9c7f909888fdf3cefd9fc12da3d12e15832a6c47ee0979ad68aacda5 · ms:5404
- 2026-09-16 · 567bec1* · exit 0 · `set -o pipefail …` · acceptance-sha256:67ac00cb9c7f909888fdf3cefd9fc12da3d12e15832a6c47ee0979ad68aacda5 · ms:4933
