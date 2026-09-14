# Task ADR-0001-T4: Keep source files on disk and OCR results in memory behind a TTL

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `blob.Store` (`Put`/`Open`/`Delete`/`Size`), `results.Store` (`Put`/`Take`/`Peek`/`Sweep`/`Len`)
**Consumes:** `core.Result`, `core.Err*` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the TTL clock`, `the fsync-then-rename`

## Goal

Store the uploaded source file durably on disk until delivery, and hold the OCR result only
in memory with a TTL, so an abandoned result is swept and its job re-queued rather than
occupying the process forever.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/blob/store.go` | add | content-addressed-by-job-id file storage |
| `internal/blob/store_test.go` | add | round-trip, delete, traversal-refusal tests |
| `internal/results/store.go` | add | in-memory map with expiry and a sweeper |
| `internal/results/store_test.go` | add | TTL, take-once and concurrency tests |

Both are selected by `router.Service` in T6 and by the `/files/{id}` handler in T7; those
tasks carry the call sites and the mutations that prove them reached.

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write
   `results/store_test.go` asserting that `Take` returns a result once and that an expired
   result is gone, before any implementation (TDD red). [proof: acceptance]
2. [S2] `blob.Store.Put(id, r io.Reader)` writes to `<dir>/<id[:2]>/<id>` — a two-character
   shard so one directory never holds a million entries — by writing a temp file in the
   same directory, `fsync`ing it, then `rename`ing into place, so a crash mid-write cannot
   leave a truncated blob that reads as a valid source file.
3. [S3] `blob.Store` rejects any id that is not a plain uuidv7: no separator, no `.`, no
   `..`. The id reaches it from a URL path, so a traversal here is a read of any file the
   process can see. Validate rather than sanitise — a rejected id is `core.ErrNotFound`.
4. [S4] `blob.Store.Delete` is idempotent: deleting an absent blob is not an error, because
   delivery and the dead-letter path can both reach it.
5. [S5] `results.Store` is a `map[string]entry` behind a `sync.Mutex`, each entry holding
   `core.Result` and `expiresAt`. `Put` stamps `now + ttl`.
6. [S6] `Take(id)` returns the result **and removes it in the same locked critical
   section**, so two concurrent clients cannot both receive it and both be charged.
   `Peek(id)` reads without removing, for the backlog listing.
7. [S7] `Sweep(now)` removes every expired entry and **returns their ids**, so the caller
   (T6) can re-queue those jobs. A sweeper that silently dropped them would strand the job
   in `done` with no result — the exact stuck state the TTL exists to prevent.
8. [S8] The clock is an injected `func() time.Time` field defaulting to `time.Now`, so the
   TTL tests assert expiry deterministically instead of sleeping.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/blob/... ./internal/results/... -count=1 -race 2>&1 | tee /tmp/adr1-t4.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t4.out
```

Red at authoring: neither package exists, so `go build ./...` fails.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestBlobPutOpenRoundTrip` | `internal/blob/store_test.go` | bytes written are the bytes read back | — | S2 |
| `TestBlobPutIsAtomic` | `internal/blob/store_test.go` | no partial file is visible under the final name until the rename completes | — | S2 |
| `TestBlobRejectsTraversalID` | `internal/blob/store_test.go` | `../../etc/passwd`, `a/b`, `.` and `..` are each refused, and no file outside the dir is opened | — | S3 |
| `TestBlobDeleteIsIdempotent` | `internal/blob/store_test.go` | deleting twice, and deleting an absent id, both succeed | — | S4 |
| `TestResultTakeReturnsOnce` | `internal/results/store_test.go` | the second `Take` of the same id returns `core.ErrNotFound` | — | S6 |
| `TestResultTakeIsAtomicUnderRace` | `internal/results/store_test.go` | N goroutines taking one id yield exactly one success | — | S6 |
| `TestResultPeekDoesNotRemove` | `internal/results/store_test.go` | `Peek` twice both succeed, and a later `Take` still succeeds | — | S6 |
| `TestResultExpires` | `internal/results/store_test.go` | with the clock advanced past the TTL, `Take` returns `core.ErrNotFound` | — | S5, S8 |
| `TestSweepReturnsExpiredIDs` | `internal/results/store_test.go` | `Sweep` returns exactly the expired ids and leaves unexpired entries — the ids are what T6 re-queues, so a sweep returning nothing would strand the job | — | S7 |
| `TestSweepIsExclusiveOfBoundary` | `internal/results/store_test.go` | an entry expiring exactly at `now` is swept, and one expiring one second later is not | — | S7, S8 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the ten tests above |
| 2 — something selects it | `router.Service.CompleteJob` calls `results.Put` and `router.Service.Deliver` calls `results.Take` (T6); `blob.Put` is called by the upload path (T6) and `blob.Open` by `/files/{id}` (T7). Each mutation is recorded on the task that owns the call site. |
| 3 — the caller can discover it | n/a: no declared interface — internal Go packages |
| 4 — it is used | T8's end-to-end test writes a blob, produces a result and takes it |

## Mutation Log

## Invariants

- A result exists **only** in memory; nothing in `internal/results` touches the filesystem.
- `Take` removes and returns in one critical section.
- A blob survives until its job is `delivered`, `dead` or `expired` — never deleted earlier.
- `Sweep` returns every id it removed.
- A blob id is validated, never sanitised.

## Risks

- **`Take` implemented as read-then-delete outside one lock** double-delivers under
  concurrency and double-charges. `TestResultTakeIsAtomicUnderRace` is the guard.
- **A sweeper that drops ids** leaves a job stuck in `done` forever with no result to serve
  — worse than the leak it was fixing, and invisible. `TestSweepReturnsExpiredIDs` binds it.
- **Unbounded memory** if results are large and clients are slow. Bounded by the TTL and by
  `buffer_limit`; the dashboard shows `results.Len()` so the operator can see it.

## Stop Condition

Stop and ask if the operator wants a size cap on the result store in addition to the TTL —
the ADR bounds it by time and by buffer limit only, and a cap is a different policy with a
different failure (rejecting a finished result) that nobody has asked for.

## Out of Scope

- Deciding *when* to re-queue a swept job — T6 owns the state machine; this task only
  reports which ids expired.
- Serving blobs over HTTP — T7's.
- Encryption at rest (deferred: docs/adr/BACKLOG.md).

## Verification Log
