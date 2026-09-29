# Task ADR-0012-T2: `Upload` and `Collect` on the Session, and `ErrNotReady`

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `(*Session).Upload()`, `(*Session).Collect()`, `client.ErrNotReady`
**Consumes:** `client.Session` + `client.Open()` + `client.ErrNotOpen` (T1)
**Data dependency:** hermetic — an `httptest.Server` shaped like `/sse`, `/upload` and `/files/<id>`
**Proof map:** v1
**Rests-on:** `Upload returning the router's job id`, `Upload carrying label and raw into the query`, `a buffer-full 429 arriving as RetryableError`, `Collect branching on the response content type`, `a 404 from /files being named ErrNotReady`, `concurrent Upload from many goroutines being safe`, `the guard from T1 refusing an unopened Session`

## Goal

Put the two halves of a job on the handle: post a document and get its id back, and fetch a finished
result by that id. Give the ambiguous 404 a name so a caller can tell "not collectable" from a
transport failure, without letting it read as "wait longer". Prove the concurrency this package now
promises.

`Submit` is still untouched — it keeps its own `upload`/`collect` helpers until T3.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `client/session.go` | edit | Add `Upload` and `Collect` as methods and `ErrNotReady`. Both are relocations of the logic in `client/client.go`'s unexported `upload` and `collect`, taking `s.base`/`s.cfg.Token`/`s.http` from the handle instead of parameters, and both check T1's `established` marker first. |
| `client/session_test.go` | edit | This task's six tests, alongside T1's twelve. |

⚠ **`client/client.go` is deliberately NOT in this table.** Its `upload` and `collect` stay where they
are and keep serving `Submit` until T3 deletes them. Two implementations coexist for exactly one task,
which is the price of making T3's behaviour-preservation provable in isolation — see Risks.

## Ordered Steps

1. [S1] Write all six tests FIRST and confirm the package does not compile — `Upload`, `Collect` and
   `ErrNotReady` do not exist. ⚠ **Do not attempt per-test red here:** Go compiles a test package as a
   unit, so a missing method makes every test in `client_test` unbuildable, T1's included. The red to
   record is the build failure naming the three missing identifiers. Per-test verdicts arrive in S5.
2. [S2] Add `ErrNotReady` with the doc comment stating WHY it is ambiguous by construction: the
   router answers 404 for a job that is queued, one that failed, one whose result expired, and one
   belonging to another customer. Only the stream distinguishes them, which is why `Collect` is called
   in response to an event rather than on a timer. [proof: acceptance]
3. [S3] Add `(*Session).Upload(ctx, Input) (string, error)`: check `established` and return
   `ErrNotOpen` if unset; build the query from `Input.Pipeline` / `Input.Label` / `Input.Params`; always
   set `raw` explicitly via the existing `rawParam`; build the multipart body; POST; decode `job_id` and
   reject an empty one. A nil `Input.Body` sends no body at all — ADR-0001's crawler shape, where the
   job is its parameters. Reuse T1's header-aware `classify` so a 429 arrives as `*RetryableError` with
   `RetryAfter` populated. ⚠ Hold no session-level mutable state across the call: `*Session` must stay
   safe for concurrent use (T1's invariant), which S6 is what actually tests. [proof: acceptance]
4. [S4] Add `(*Session).Collect(ctx, jobID) (Result, error)`: check `established`; GET `/files/<id>`
   with the id PATH-ESCAPED; map 404 to `ErrNotReady`; and branch on the response `Content-Type` —
   `application/octet-stream` reads bytes into `Result.Raw`, anything else decodes `{"units":[…]}`.
   ⚠ Branch on what the SERVER said, never on what was requested: the two disagree exactly when
   something is wrong, and that is the case worth surfacing rather than misreading.
   ★ **`url.PathEscape` is a DECLARED behaviour difference, not an oversight.** The `collect` this
   replaces does not escape (`client/client.go:350`), so after T3 a job id containing `/` or `%` is
   handled differently by `Submit`. No existing test distinguishes it — ids in the suite are `job-1` —
   so it is declared here, listed in T3's Stop Condition as expected, and in ADR-0012 §Risks. Escaping
   a value that reaches a URL path is correct; shipping it silently inside a
   "behaviour-preserving" refactor would not be. [proof: acceptance]
5. [S5] [proof: acceptance] With the package compiling, run the filtered leg and confirm each test
   passes for its own reason; then run the whole package under `-race` including T1's tests and the 776
   pre-existing lines, and confirm `Submit` still behaves identically while two upload paths coexist.
6. [S6] [proof: acceptance] Prove the concurrency this package promises: `TestConcurrentUploadIsSafe`
   fires N `Upload` calls from N goroutines on ONE session under `-race`, asserting every call gets a
   distinct job id and the detector stays quiet. ⚠ This needs its own test because `-race` over
   SEQUENTIAL tests proves nothing about concurrent use, and ADR-0012 §Served-path change promises it
   explicitly.
7. [S7] [proof: mutation] Break `Collect`'s content-type branch — decode the JSON envelope
   unconditionally — and confirm `TestSessionCollectBranchesOnContentType` goes red. This is the
   mechanism that decides whether a caller receives a converted file or a document full of
   `{"job_id":…}`, and ADR-0006 is the record it would violate.

## Acceptance

```bash
set -o pipefail
go test ./client/... -count=1 -race -v \
  -run 'TestSessionUploadReturnsJobID|TestSessionUploadCarriesLabelAndRaw|TestSessionUploadBufferFullIsRetryable|TestSessionUploadOnUnopenedSessionIsRefused|TestSessionCollectBranchesOnContentType|TestCollectNotReadyOn404|TestConcurrentUploadIsSafe' \
  2>&1 | tee /tmp/acc-0012-T2a.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/acc-0012-T2a.out \
  && templ generate && go build ./... && go vet ./... && test -z "$(gofmt -l .)" \
  && go test ./client/... ./cmd/client/... -count=1 -race 2>&1 | tee /tmp/acc-0012-T2b.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0012-T2b.out
```

<The seven new checks run first and alone so T1's twelve and the 776 pre-existing lines cannot carry
them. `-race` is on the filtered leg too, because `TestConcurrentUploadIsSafe` is worthless without it.
The `! grep -qE "no tests to run"` guard is what makes this fence red at authoring time: with none of
the seven written, `-run` matches nothing and `go test` exits 0 with a summary.>

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestSessionUploadReturnsJobID` | `client/session_test.go` | A 201 carrying `{"job_id":"…"}` is decoded and returned; a 201 with an EMPTY job id is an error rather than a silent empty string, because an id that was never written down is a job that gets paid for twice. | — | S1, S3 |
| `TestSessionUploadCarriesLabelAndRaw` | `client/session_test.go` | `Input.Label` reaches `?label=`, `Input.Pipeline` wins over `Label` when both are set (matching the router's own precedence), `Input.Params` reach the query, and `raw` is sent EXPLICITLY in both modes so a mismatch is a disagreement between two stated positions rather than between a statement and a default. | — | S1, S3 |
| `TestSessionUploadBufferFullIsRetryable` | `client/session_test.go` | A 429 with `{"error":"buffer full"}` and no `Retry-After` yields `*RetryableError` with `RetryAfter == 0`, found by `errors.As`. This is the response a bulk caller actually has to act on, and T1's field is what it reads. | — | S1, S3 |
| `TestSessionUploadOnUnopenedSessionIsRefused` | `client/session_test.go` | `(&client.Session{}).Upload(...)` returns `ErrNotOpen` rather than panicking or posting into a stream nobody holds — T1's guard, exercised through the method that would actually cause the damage. | — | S1, S3 |
| `TestSessionCollectBranchesOnContentType` | `client/session_test.go` | `application/octet-stream` populates `Result.Raw` byte-for-byte including invalid UTF-8, leaving `Units` nil; a JSON reply populates `Units`, leaving `Raw` nil. Asserted in BOTH directions, since a one-directional test passes with the branch inverted. | — | S1, S4, S7 |
| `TestCollectNotReadyOn404` | `client/session_test.go` | A 404 from `/files/<id>` returns exactly `ErrNotReady` under `errors.Is`, and NOT a `*RetryableError` — the distinction that stops a caller retrying a job that failed, expired, or was never theirs. | — | S1, S2, S4 |
| `TestConcurrentUploadIsSafe` | `client/session_test.go` | N goroutines calling `Upload` on one `*Session` produce N distinct job ids with the race detector quiet. The promise in ADR-0012 §Served-path change, which no sequential test under `-race` can establish. | — | S1, S6 |

⚠ **T1's twelve tests and the 776 pre-existing lines are not listed here.** They run in the fence's
unfiltered leg, which is how the coexistence of two upload paths is proved harmless, but a Tests row
promises a test the FILTERED leg selects.

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | The seven tests above, none of which compiles before this task. |
| 2 — something selects it | ⚠ **Still nothing in this repository, and still deliberate.** `Upload` and `Collect` get their production caller in T3, when `Submit` starts using them. S7's mutation stands in for a call site at this rung; after T2 the methods exist and only their own tests reach them. |
| 3 — the caller can discover it | Not closed here — doc comments only. The `Example` and README are T4. |
| 4 — it is used | Nothing measures this; the consumer is in another repository (ADR-0012 §Consequences). |

## Mutation Log

<Tool-written by `adr-verify --mutant` at execution time. Empty at authoring.>

## Invariants

- **`client/client.go` is not edited.** `Submit`, `upload` and `collect` keep working exactly as they
  do today; this task only adds.
- **`client/client_test.go` and `client/raw_test.go` are not edited.**
- **No existing exported symbol changes shape.** `Upload` takes the EXISTING `Input` and `Collect`
  returns the EXISTING `Result`; no parallel parameter type is introduced.
- **Both methods check T1's `established` marker first** and return `ErrNotOpen` rather than acting on
  a session nobody opened.
- **`*Session` stays safe for concurrent use.** Neither method may introduce session-level mutable
  state without a lock, and `TestConcurrentUploadIsSafe` under `-race` is what holds this.
- **`Collect` branches on the response's content type, never on `Input.Raw`.** ADR-0006's whole point
  is that raw bytes never meet `encoding/json`.
- **`Collect` path-escapes the job id**, and that difference from the code it replaces is DECLARED
  (S4), not silent.
- **`Upload` sends `raw` explicitly in both modes.**
- ⚠ **Neither method takes a `Progress`.** The `collect` being replaced reports `StageCollecting` and
  `StageDone` internally (`client/client.go:348`, `:379`, `:390`); those reports become T3's
  responsibility at both of its call sites. Stated here because it is this task's decision that creates
  that obligation.
- `client/` still has no flags, no terminal output, no exit codes (ADR-0005's seam).

## Risks

- **Two upload implementations coexist for the length of one task**, and a fix applied to one would
  silently miss the other. Bounded deliberately: T3 deletes the old pair in the very next task, and
  T2's Invariants forbid editing `client.go` so the duplication cannot be "tidied" into a
  half-migration here. The alternative — rewriting `Submit` in this task — is what would make a
  behaviour change unattributable, which is what ADR-0012's falsifiability argument depends on avoiding.
- **Moving the progress reports out of `Collect` is where `Submit`'s stage sequence can silently
  diverge**, because `TestProgressCallbackSequence` (`client/client_test.go:553`) covers only the event
  path and not the backlog path. Named here and carried into T3's steps; neither task can close it with
  an existing test.
- `Collect` charges the customer and destroys the router's only copy of the result. A caller that
  collects twice loses a paid-for result. This task cannot enforce once-only — that is the caller's
  discipline — so it names the hazard in the doc comment and asserts the SECOND call returns
  `ErrNotReady` rather than something untyped.
- A truncated read after the charge has landed is a paid-for result lost, and retrying gets a 404.
  `Collect` returns a plain error naming what happened rather than a `*RetryableError`, because
  retrying is the wrong action.
- `Upload` buffers the whole document in memory, reproducing the defect in the code it relocates
  (`client/client.go:289-306`). Deliberate: streaming it is Out of Scope so this task stays a
  relocation, and it is recorded as a Negative consequence in ADR-0012 rather than hidden.
- `TestConcurrentUploadIsSafe` can pass by accident if the goroutines do not actually overlap. Use a
  barrier so every goroutine is released at once, and enough of them that serialisation would be
  visible.

## Stop Condition

Stop and ask if relocating `upload`/`collect` onto the handle requires changing any observable
behaviour BEYOND the declared `PathEscape` difference — a status code's meaning, an error's type, a
query parameter's spelling. T2 is otherwise additive by construction, and an undeclared behaviour
difference here would land inside T3's supposedly behaviour-preserving refactor where nothing would
attribute it.

Stop and ask if `*Session` cannot be made concurrency-safe without a lock that serialises uploads —
that would mean the handle's shape from T1 is wrong for the served path, and the fix belongs there
rather than in a mutex added quietly here.

Stop and ask if the router turns out to answer 404 for a job that IS collectable, or a non-404 for one
that is not — that would make `ErrNotReady` name the wrong condition, and it is a finding about
`internal/httpapi` rather than a client fix.

## Out of Scope

- Re-implementing `Submit` over these methods, and re-emitting the progress stages `Collect` no longer
  reports — T3's job.
- Deleting `client.go`'s `upload` and `collect` — T3's job, in the same commit that replaces their
  caller.
- `Services` and the documentation — T4.
- Exposing an HTTP status code on the returned errors, which is what a consumer would need to re-attach
  its own 409 diagnosis (deferred: `docs/adr/BACKLOG.md`).
- Enforcing exactly-once collection (permanent: boundary: the router already deletes the result on
  collection, so at-most-once is enforced where the state lives; a client-side guard would be a second
  source of truth about whether a job was taken, and the caller that persists job ids is the only
  party that knows).
- Streaming the multipart body (deferred: `docs/adr/BACKLOG.md`).

## Verification Log

<Tool-written by `adr-verify` at execution time. Empty at authoring.>
