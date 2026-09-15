# Task ADR-0005-T1: The protocol — stream first, then upload, then wait

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (one new package plus its test)
**Owner:** unassigned
**Produces:** `client.Submit()`, `client.Config`, `client.Input`, `client.Result`, `client.Progress`, `client.FailedError`
**Consumes:** none from this record's siblings — it drives `POST /upload`, `GET /sse` and `GET /files/{id}` exactly as ADR-0001 shipped them
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the stream-before-upload ordering`, `the job-id match`, `the typed failure`, `the frame reassembly`

## Goal

One function that submits a job and returns its result or a typed failure, with the SSE stream open
before the upload so a fast job cannot finish into a stream nobody is holding.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/client/client.go` | add | `Submit` — the whole protocol |
| `internal/client/sse.go` | add | frame reassembly, separated because it is the part with a parsing bug worth isolating |
| `internal/client/client_test.go` | add | the failing tests, against `httptest` servers |

Nothing selects this yet — T2's binary does. Rung 2 is discharged there, and this record says so
rather than claiming it: three methods in this codebase have already shipped tested and uncalled.

## Ordered Steps

1. [S1] Write the failing test first: `Submit` against a server that records the ORDER of its
   requests and asserts `/sse` arrives before `/upload` (TDD red). [proof: acceptance]
2. [S2] ⚠ **Open `GET /sse` and read the `hello` frame BEFORE posting the upload.** Waiting for
   `hello` is what makes "open" mean established rather than dialled — a connection that has not
   yet been accepted by the handler is not subscribed to the bus, so the race is still open. This
   is the record's central claim.
3. [S3] `POST /upload`: multipart `file` when `Input.Body` is set, query parameters for `label`,
   `pipeline` and every `Param`. A params-only upload sends no body at all — ADR-0001's crawler
   shape, where the service fetches its own input.
4. [S4] Map the upload's status to a typed outcome before anything else happens: `401` and `403` →
   fatal, `402` (no credits) → fatal, `429` → retryable, `400` → fatal, `5xx` → retryable. T2 turns
   these into exit codes; the classification lives here because it is about the protocol.
5. [S5] Wait for an event naming THIS job id. ⚠ A `ready` or `failed` for a different job of the
   same user is ignored — a customer with two jobs in flight would otherwise collect the wrong one,
   or fail on someone else's failure.
6. [S6] On `ready`, `GET /files/{id}` and return the units. On `failed`, return a `FailedError`
   carrying the reason — a typed error, not a string, so T2 can exit 3 without parsing prose.
7. [S7] The `backlog` frame on connect is treated exactly like a `ready` for each job it names. It
   is what makes a reconnect correct: a client that missed the live event is told on reconnection
   which of its results are waiting.
8. [S8] ⚠ **Frames are reassembled, not read line by line.** An SSE frame is `event:` then `data:`
   then a blank line, and a reader that assumes one frame per read drops events when two arrive in
   one TCP segment or one frame spans two.
9. [S9] `Progress` is an optional callback — `ProgressUploading`, `ProgressWaiting`,
   `ProgressCollecting`, `ProgressDone`. The package renders nothing and knows nothing about
   terminals; that is T2's job.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/client/... -count=1 -race 2>&1 | tee /tmp/adr5-t1.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr5-t1.out
```

The package runs alone because nothing consumes it yet — there is no regression surface to add. Red
at authoring: `internal/client` does not exist, so the run reports `no test files` and the grep
makes the fence non-zero.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestStreamOpensBeforeUpload` | `internal/client/client_test.go` | a server recording request order sees `/sse` before `/upload` — **the record's `Enforced-by:` check**, and the one ordering that cannot be recovered from: a job finishing in the gap fires `ready` into a stream nobody holds, and the client waits forever | — | S2 |
| `TestSubmitReturnsUnits` | `internal/client/client_test.go` | the happy path returns the units the server sent — the positive half every failure test below needs | — | S3, S6 |
| `TestUploadSendsTheFileAsMultipart` | `internal/client/client_test.go` | the server receives the bytes under the `file` part | — | S3 |
| `TestParamsOnlyUploadSendsNoBody` | `internal/client/client_test.go` | with no `Body` and a `url` param, the request carries the param and no multipart body — ADR-0001's crawler shape | — | S3 |
| `TestLabelAndPipelineReachTheQuery` | `internal/client/client_test.go` | `--label` and `--pipeline` arrive as query parameters in the form the router parses | — | S3 |
| `TestFailedEventReturnsTypedError` | `internal/client/client_test.go` | a `failed` event yields a `FailedError` whose `Reason` is the server's — a typed error, so T2 exits 3 without parsing prose | — | S6 |
| `TestIgnoresEventsForOtherJobs` | `internal/client/client_test.go` | a `ready` and a `failed` for a DIFFERENT job id are ignored, and the client still returns on its own — red if the id is not matched, which would collect someone else's result or fail on their failure | — | S5 |
| `TestBacklogOnConnectIsHonoured` | `internal/client/client_test.go` | a `backlog` naming this job is treated as ready, with no live event ever sent — the reconnection path, and the only thing that makes a dropped stream survivable | — | S7 |
| `TestFrameSplitAcrossWrites` | `internal/client/client_test.go` | an `event:`/`data:` frame delivered in two writes is still parsed — a line-at-a-time reader drops these under load | — | S8 |
| `TestTwoFramesInOneWrite` | `internal/client/client_test.go` | two frames in one write both arrive — the other half of the same reassembly bug | — | S8 |
| `TestRetryableUploadStatuses` | `internal/client/client_test.go` | `429` and `503` classify retryable; `401`, `402` and `400` classify fatal — table-driven, because a single case proves one branch of a five-way decision | — | S4 |
| `TestProgressCallbackSequence` | `internal/client/client_test.go` | the callback fires uploading → waiting → collecting → done, in order | — | S9 |
| `TestSubmitHonoursContextCancellation` | `internal/client/client_test.go` | a cancelled context returns promptly rather than blocking — with a deadline on the test, so a regression fails instead of hanging the suite | — | S5 |
| `TestCollectFailureIsNotSilent` | `internal/client/client_test.go` | a `500` from `GET /files/{id}` returns an error rather than empty units — an empty result that reads as success is the shape that loses data downstream | — | S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the fourteen tests above |
| 2 — something selects it | **nothing selects it yet** — T2's binary is the first caller, and its mutants discharge this rung. Recorded rather than claimed: `ListTokens`, `RevokeToken` and `SetActive` all shipped in this repository tested and uncalled, and the habit that produced them is claiming rung 2 from a unit test. |
| 3 — the caller can discover it | doc comments on every exported identifier; `FailedError` is a typed error a caller matches with `errors.As` |
| 4 — it is used | T2's human sign-off — a real submission against a real router |

## Mutation Log

## Invariants

- The SSE stream is established, `hello` received, before the upload is posted.
- Only events naming this job id are acted on.
- A failed job returns a typed error, never empty units and a nil error.
- The package writes nothing to a terminal and reads no flags.

## Risks

- **`TestStreamOpensBeforeUpload` is the record's central check and the easiest to write
  vacuously.** Asserting that `/sse` was *called* proves nothing about order; the fixture must
  record a sequence and compare indices.
- **Every waiting test can hang the suite forever if the fixture never sends its event.** All of
  them take a context with a deadline, so a regression fails rather than hanging — and a hung suite
  is the failure that gets a test deleted rather than fixed.
- **`TestIgnoresEventsForOtherJobs` must send the foreign event FIRST**, then the real one. Sending
  it after proves nothing: the client would already have returned.
- **A test server that writes an SSE frame with one `Fprintf` never exercises S8.** The split tests
  must write in two parts with a flush between, or the reassembly code is untested and the bug
  ships.
- **Classifying `402` as retryable would make a caller retry forever against an empty balance.** The
  table test covers all five statuses for that reason.

## Stop Condition

Stop and ask if the operator's deployment has a reverse proxy that buffers responses. This whole
design rests on SSE arriving promptly; behind a buffering proxy `Submit` hangs rather than failing,
and the deferred `--poll` mode becomes required rather than optional.

## Out of Scope

- Flags, progress rendering and exit codes — T2.
- A polling fallback (deferred: `docs/adr/BACKLOG.md`).
- Batch submission (deferred: `docs/adr/BACKLOG.md`).
- Reconnecting a dropped stream automatically (permanent: boundary: the backlog frame makes a fresh
  `Submit` correct, and an internal retry loop would hide a router outage from the caller that has
  to decide about it).

## Verification Log
