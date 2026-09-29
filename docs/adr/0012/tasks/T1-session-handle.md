# Task ADR-0012-T1: The Session handle — `Open`, `Events`, `Backlog`, `Close`, `ErrNotOpen`, `RetryAfter`

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `client.Session`, `client.Open()`, `(*Session).Events()`, `(*Session).Backlog()`, `(*Session).Close()`, `client.Event`, `client.ErrNotOpen`, and the additive field `client.RetryableError.RetryAfter`
**Consumes:** none
**Data dependency:** hermetic — every test drives an `httptest.Server` shaped like the router's `/sse`
**Proof map:** v1
**Rests-on:** `Open returning only after hello AND the first backlog`, `the grace period releasing Open when no backlog follows`, `Backlog naming what was already waiting`, `Events closing when the stream dies`, `Close being idempotent and racing nothing`, `a zero-value or closed Session being refused with ErrNotOpen`, `RetryAfter being zero when the server named none`, `the existing 776 lines still passing after the classify reshape`

## Goal

Introduce the stream-lifetime handle the rest of the record hangs off: a `*Session` that `Open`
returns only once the router has both greeted it and told it what was already waiting. Add the
terminal-state event type it delivers, the sentinel that refuses a session nobody opened, and give
`RetryableError` the `Retry-After` value it currently discards.

`Submit` is not touched — it keeps its own `openStream` until T3.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `client/session.go` | add | The handle: `Session`, `Open`, `Events`, `Backlog`, `Close`, `pump`, `dispatch`, `Event`, `ErrNotOpen`, and the establishment grace constant. New file so the diff against `client.go` stays readable and T3's later edit to `client.go` is attributable. |
| `client/client.go` | edit | `RetryableError` gains `RetryAfter time.Duration` with its doc comment; a `retryAfter(http.Header)` helper is added; `classify` is reshaped to read the header. ⚠ **That reshape changes `classify`'s signature, so all THREE of its call sites change too** (`client/client.go:227`, `:328`, `:364`) — four hunks in this file, not two. It is the one edit that touches code `Submit` already runs, which is why the fence runs the 776 existing lines. |
| `client/session_test.go` | add | This task's tests, `package client_test` like every other test file here, so the handle is driven through the exported surface only. |

⚠ **`client/sse.go` is NOT edited.** An earlier draft carried a hunk making `readFrames` tolerate a
`data:` line with no space after the colon. The router emits `"event: %s\ndata: %s\n\n"`
(`internal/httpapi/sse.go:80`) and `client/sse.go:68-72` already parses exactly that, so the hunk
would have been speculative tolerance for a format nothing sends. Dropped before authoring rather
than probed at execution time.

## Ordered Steps

1. [S1] Write every test below FIRST, in one commit, and confirm the package does not compile — the
   symbols do not exist. ⚠ **Do NOT try to observe each test failing "for its own reason": Go
   compiles a test package as a unit, so the five that reference `Session` make EVERY test in
   `client_test` unbuildable, including the two `RetryAfter` ones.** An earlier draft asked for
   per-test red and also claimed the `RetryAfter` tests would fail on an assertion; both were wrong —
   `re.RetryAfter` is a compile error while the field is absent, which is the correct pre-state. The
   red to record here is the build failure and the missing identifiers it names. Per-test red is
   recovered in S6, once the package compiles.
2. [S2] Add `RetryAfter time.Duration` to `RetryableError`, a `retryAfter(http.Header) time.Duration`
   helper reading delta-seconds, and reshape `classify` to take the header so it can populate the
   field. Update all three call sites. ⚠ Every existing error's TEXT must be unchanged — the
   unfiltered regression leg is what proves it. [proof: acceptance]
3. [S3] Write `client/session.go`'s establishment: `Open` dials `GET /sse`, spawns `readFrames`, and
   drains frames until it has seen `hello` AND the first `backlog`, accumulating the backlog's job ids.
   ⚠ **The router sends `hello` THEN `backlog`** (`internal/httpapi/sse.go:87-98`), so returning on
   `hello` alone hands the `backlog` frame to `pump` and leaves `Backlog()` empty in production. After
   `hello`, wait at most `establishGrace` for the backlog, then return anyway — a role or a router that
   sends none must not hang the caller. Set the unexported `established` marker here and nowhere else.
   [proof: acceptance]
4. [S4] Write `pump`, `dispatch`, `Events`, `Backlog` and `Close`. `pump` replays the frames that
   arrived during establishment before draining the live channel, so a job that finished mid-`Open` is
   not lost, and `defer close(s.events)` so a dead stream closes the channel. `Backlog()` returns a
   COPY — the caller must not be able to mutate the session's state. `Close` cancels under a mutex and
   is safe twice. [proof: acceptance]
5. [S5] Add `ErrNotOpen` and guard `Events`, `Backlog` and `Close` plus (from T2) `Upload` and
   `Collect` on the `established` marker. ⚠ This exists because `&client.Session{}` and
   `var s client.Session` are LEGAL from any package — unexported fields stop a caller setting them,
   not constructing the zero value — so without the guard those calls panic or block on a nil channel
   instead of returning an error. `Events()` on an unopened session returns a closed channel rather
   than nil, so a caller ranging over it terminates instead of deadlocking. [proof: acceptance]
6. [S6] [proof: acceptance] With the package now compiling, run the filtered leg and confirm each test
   passes for its own reason, then run the whole package under `-race` including the 776 pre-existing
   lines to confirm `Submit` is unaffected by the `classify` reshape. This step is where per-test
   verdicts become observable, which S1 could not deliver.
7. [S7] [proof: mutation] Two mutants, both on mechanisms this task owns. First: make `Open` return as
   soon as it sees `hello`, and confirm `TestOpenReturnsOnlyAfterHelloAndBacklog` goes red — that is
   the record's `Enforced-by:` check and the blocker a cold review caught in the first draft. Second:
   delete the `established` check from one method and confirm `TestZeroValueSessionIsRefused` goes red.

## Acceptance

```bash
set -o pipefail
go test ./client/... -count=1 -race -v \
  -run 'TestOpenReturnsOnlyAfterHelloAndBacklog|TestOpenReturnsWithinGraceWhenNoBacklogFollows|TestOpenSurfacesBacklogAlreadyWaiting|TestBacklogIsACopy|TestEventsClosesWhenStreamDies|TestEventsCarriesReadyAndFailed|TestEventsReplaysFramesFromEstablishment|TestSessionCloseIsIdempotent|TestZeroValueSessionIsRefused|TestClosedSessionIsRefused|TestRetryAfterIsZeroWhenUnset|TestRetryAfterIsParsedWhenSet' \
  2>&1 | tee /tmp/acc-0012-T1a.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/acc-0012-T1a.out \
  && templ generate && go build ./... && go vet ./... && test -z "$(gofmt -l .)" \
  && go test ./client/... ./cmd/client/... -count=1 -race 2>&1 | tee /tmp/acc-0012-T1b.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0012-T1b.out
```

<The twelve new checks run FIRST and alone, so none can be carried by the 776 lines of pre-existing
test that follow — before this task those 776 lines are green while nothing is done, which is exactly
the "which of these subjects could carry the verdict by itself" trap. The `! grep` guard is
load-bearing rather than decorative: `go test -run` over a pattern that matches nothing prints a
summary and exits 0, so without it this fence passes with no code written. The regression leg is
scoped to the two packages this task can reach; `build`/`vet`/`gofmt` still sweep the whole tree
because the `classify` reshape touches three call sites and could break a caller anywhere.>

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestOpenReturnsOnlyAfterHelloAndBacklog` | `client/session_test.go` | With the stub emitting the ROUTER'S order — `hello`, then `backlog` after a delay — `Open` has not returned while only `hello` has been sent, asserted by a channel closed on return and checked before the delay elapses. The `Enforced-by:` check, and the hole `TestStreamOpensBeforeUpload` cannot cover because it records at handler entry (`client/client_test.go:60` vs `:65-68`). | — | S1, S3, S7 |
| `TestOpenReturnsWithinGraceWhenNoBacklogFollows` | `client/session_test.go` | A stub that sends `hello` and NO `backlog` still lets `Open` return, within a bound. Without this the grace period is untested and a worker-role or older-router connection would hang forever. | — | S1, S3 |
| `TestOpenSurfacesBacklogAlreadyWaiting` | `client/session_test.go` | Job ids in the `backlog` frame are reflected by `Backlog()` after `Open` returns — the resume primitive, and the blocker a cold review caught: returning on `hello` alone loses this frame to `pump` and empties the map silently. | — | S1, S3 |
| `TestBacklogIsACopy` | `client/session_test.go` | Mutating the map `Backlog()` returned does not change what a second call returns. A caller must not be able to corrupt the session's resume state. | — | S1, S4 |
| `TestEventsClosesWhenStreamDies` | `client/session_test.go` | When the stub closes the response body, the channel from `Events()` closes rather than blocking, so a caller ranging over it learns the stream died and can re-`Open`. | — | S1, S4 |
| `TestEventsCarriesReadyAndFailed` | `client/session_test.go` | A `ready` frame yields `Event{Ready:true}`; a `failed` frame yields `Event{Ready:false, Reason:…}` with the reason carried; a post-establishment `backlog` frame yields a ready `Event` per job — required because T3's `Submit` depends on it and nothing else specifies it. | — | S1, S4 |
| `TestEventsReplaysFramesFromEstablishment` | `client/session_test.go` | A `ready` frame arriving between `hello` and `backlog` is delivered on `Events()` after `Open` returns, not dropped. A fast job finishing mid-establishment is the case this exists for. | — | S1, S4 |
| `TestSessionCloseIsIdempotent` | `client/session_test.go` | Two `Close()` calls return nil and do not panic; under `-race`, so a second cancel racing `pump` is caught here rather than in a consumer. | — | S1, S4 |
| `TestZeroValueSessionIsRefused` | `client/session_test.go` | `var s client.Session` and `&client.Session{}` — both legal from another package — return `ErrNotOpen` from every method under `errors.Is`, and `Events()` returns a CLOSED channel rather than nil so a range terminates. | — | S1, S5, S7 |
| `TestClosedSessionIsRefused` | `client/session_test.go` | After `Close()`, the same methods return `ErrNotOpen` rather than appearing to work against a cancelled stream. | — | S1, S5 |
| `TestRetryAfterIsZeroWhenUnset` | `client/session_test.go` | A 429 carrying `{"error":"buffer full"}` and NO `Retry-After` yields `*RetryableError` with `RetryAfter` exactly zero — the buffer-full case, which cannot name a delay because what frees a slot is a collection. | — | S1, S2 |
| `TestRetryAfterIsParsedWhenSet` | `client/session_test.go` | A 429 carrying `Retry-After: 2` yields `RetryAfter == 2*time.Second`, so a caller can lengthen its own backoff. | — | S1, S2 |

⚠ **`TestStreamOpensBeforeUpload` and the rest of the 776 existing lines are NOT listed here, on
purpose.** They run — in the fence's unfiltered regression leg, which is how the `classify` reshape is
proved harmless — but a Tests row promises a test the FILTERED leg selects, and listing already-green
suites beside twelve red ones is how a verdict gets carried by the wrong subject.

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | The twelve tests above, none of which compiles before this task. |
| 2 — something selects it | ⚠ **Nothing in this repository calls `Open` when T1 lands, and that is stated rather than hidden.** `Session` gets its production caller in T3. S7's two mutants are what stand in for a call site at this rung — they prove the tests are not vacuous, which is the most this task can honestly claim. |
| 3 — the caller can discover it | Not closed here. Doc comments are written; the godoc `Example` and the README section are T4's job, and rung 3 is not honestly closed until T4 lands. |
| 4 — it is used | Nothing measures this. The motivating consumer is in another repository and this one cannot see it — a permanent cost named in ADR-0012 §Consequences. |

## Mutation Log

<Tool-written by `adr-verify --mutant` at execution time. Empty at authoring; the section exists
because `adr-verify` refuses to record a mutant into a task with nowhere to put it.>

## Invariants

- **No existing exported symbol is removed, renamed or re-signatured.** `RetryableError` gains a
  field; a struct field addition breaks no consumer that constructs it with field names.
- **`Submit` is not touched.** It keeps calling its own `openStream` until T3. The only code it shares
  with this task is `classify`, whose observable error text is unchanged.
- **`client/client_test.go` and `client/raw_test.go` are not edited.** They are the regression net for
  the `classify` reshape.
- **`Open` is the only thing that sets `established`.** There is no exported constructor and no
  exported field, so a `*Session` that works is one `Open` returned. ⚠ This is a RUNTIME guarantee
  enforced by the marker, not a compile-time one — the zero value is constructible from any package
  (ADR-0012 §Decision).
- **`Open` does not return before `hello` AND the first `backlog`**, except by exhausting
  `establishGrace`, and any frame arriving during establishment is replayed rather than dropped.
- **`*Session` is safe for concurrent use by multiple goroutines.** The served path is concurrent
  uploads (ADR-0012 §Served-path change), so this is a promise, not an accident. T2 adds the
  concurrent test; this task must not introduce state that makes it false.
- **`Backlog()` returns a copy.**
- `client/` still has no flags, writes nothing to a terminal and decides no exit codes (ADR-0005's
  seam). Nothing in this task reads a file, an environment variable or a global.

## Risks

- The `classify` reshape changes an error string and a consumer parses it — mitigated by the
  unfiltered regression leg, and by ADR-0005 having given these errors TYPES precisely so nobody
  parses prose. A test asserting on text is the signal to stop (Stop Condition).
- **`establishGrace` is a timing constant, so a test that leans on it is a flake waiting to happen.**
  `TestOpenReturnsOnlyAfterHelloAndBacklog` must assert on a channel closed by `Open`'s return, never
  on elapsed wall-clock time; `TestOpenReturnsWithinGraceWhenNoBacklogFollows` needs a generous upper
  bound rather than a tight one.
- `pump` leaks a goroutine when a caller forgets `Close` — real, unfixable from here, and named as a
  Negative consequence in ADR-0012. `-race` plus `TestEventsClosesWhenStreamDies` bound what this task
  can prove: the goroutine exits when the stream ends or the context is cancelled.
- The `established` guard is a runtime check, so a caller who ignores the error still proceeds. That is
  the honest ceiling of the mechanism, and ADR-0012 §Alternatives records the one shape (an interface)
  that would have made it structural, and why it was rejected.

## Stop Condition

Stop and ask if making `classify` read the header requires changing any existing error's TEXT, or if
any of the 776 pre-existing lines has to be edited to keep passing. Either means the reshape is not
additive, and ADR-0012's falsifiability argument rests on those tests being untouched.

Stop and ask if the router turns out NOT to send `backlog` after `hello` for a client role — the whole
establishment design rests on `internal/httpapi/sse.go:87-98`, and if that is conditional in a way the
code does not show, the grace period is load-bearing rather than a safety net and deserves saying so.

## Out of Scope

- `Upload` and `Collect` — T2's job, including the concurrent-upload test that proves this task's
  concurrency invariant.
- Re-implementing `Submit` over `Session` — T3, deliberately separated so a behaviour change in
  `Submit` cannot be attributed to the introduction of the type.
- `Services` and the documentation — T4.
- Making the ordering structurally unrepresentable by turning `Session` into an interface (permanent:
  boundary: ADR-0012 §Alternatives rejects it — it forces consumers to accept an interface they cannot
  usefully implement, and the typed error buys the same protection at the cost of one check).
- Reconnecting a dropped stream inside `Session` (deferred: `docs/adr/BACKLOG.md`).
- Streaming the multipart body (deferred: `docs/adr/BACKLOG.md`).

## Verification Log

<Tool-written by `adr-verify` at execution time. Empty at authoring.>
