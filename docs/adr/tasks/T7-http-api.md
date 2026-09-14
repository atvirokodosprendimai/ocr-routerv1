# Task ADR-0001-T7: Serve upload, claim, files and the SSE command stream behind bearer auth

**Depends-on:** T3, T6
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `httpapi.New(deps) http.Handler` mounting `POST /upload`, `GET /sse`, `GET /files/{id}`, `POST /claim`, `GET /services`
**Consumes:** `identity.Service.Authenticate()` (T3), `router.Service` (T6), `blob.Store` and `results.Store` (T4), `bus.Bus` (T5), `core.*` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the role gate`, `the cleared write deadline`, `the status mapping`

## Goal

Put the five endpoints on the wire, route each by the authenticated principal's role, and
hold SSE streams open indefinitely with a backlog on connect and a keepalive.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/httpapi/api.go` | add | `New()` — **the route table; this is what makes every handler reachable** |
| `internal/httpapi/middleware.go` | add | bearer auth, role gate, principal in context |
| `internal/httpapi/upload.go` | add | the two-role `POST /upload` |
| `internal/httpapi/files.go` | add | the two-role `GET /files/{id}` |
| `internal/httpapi/claim.go` | add | `POST /claim` |
| `internal/httpapi/sse.go` | add | the stream: hello, backlog, events, ping |
| `internal/httpapi/errors.go` | add | sentinel → status mapping, in one place |
| `internal/httpapi/*_test.go` | add | the failing tests |

`api.go`'s route table is what *selects* every handler: a handler that exists and is not
mounted is this pipeline's most common shipped defect, which is why the mutation below
deletes a route rather than a handler body.

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write
   `middleware_test.go` asserting that an unauthenticated request is 401 and that a client
   token cannot reach `POST /claim`, before any implementation (TDD red).
   [proof: acceptance]
2. [S2] `New(deps)` builds a `chi` router, wraps every route in the auth middleware, and
   mounts the five paths — the four in the ADR's table plus `GET /services`, which is how a
   client discovers the labels currently available instead of guessing. `deps` is an explicit
   struct, so a missing dependency is a compile error rather than a nil panic at the first
   request.
3. [S3] The middleware calls `identity.Authenticate`, puts the `core.Principal` in the
   request context, and returns `401` with `WWW-Authenticate: Bearer` on failure. A
   `RequireRole(...)` wrapper gates `POST /claim` to workers.
4. [S4] `POST /upload` branches **on the principal's role, never on the body or a header** —
   a client sends multipart and a worker sends JSON, and letting the payload choose the
   branch is the role-confusion bug in ADR §Risks. A client posting a worker-shaped body is
   `400`, not a completed job.
5. [S5] The client arm reads `?label=` (default `ocr`) or `?pipeline=a,b,c`, and every other
   query parameter becomes a job param. The multipart part, **when present**, streams
   straight into `router.Upload` under a size cap (`http.MaxBytesReader`); a request with no
   body at all is the valid crawler shape, not an error. Answers `201 {"job_id":…}`.
6. [S6] The worker arm decodes `{"job_id","pages":[…]}` or `{"job_id","error":"…"}` and
   calls `router.Complete` or `router.Fail`. Answers `204`.
7. [S7] `GET /files/{id}` branches on role: a **client** gets `router.Deliver` — the result
   as JSON, charged, removed; a **worker** gets the source blob streamed, and only if it
   holds the lease (else `409`). The id is validated as a uuidv7 before it reaches
   `blob.Store`.
8. [S8] `POST /claim` reads the worker's `?label=`, calls `router.Claim(workerID, label)`,
   and answers `200 {"job_id":…, "label":…, "params":{…}, "has_blob":…}` or `204` when
   nothing is claimable — `204`, not `404`, because an empty queue is a normal answer to a
   polling worker and not an error to be logged.
9. [S9] `GET /sse`: **clear the write deadline first** —
   `http.NewResponseController(w).SetWriteDeadline(time.Time{})` — or `WriteTimeout` kills
   the stream mid-session with nothing in the logs. Then subscribe to the role's topic,
   send `hello`, send the backlog, and enter one `for { select { ctx.Done, event, ticker } }`
   loop. The ticker sends `ping` every 15s so an idle stream survives proxies and the
   client can tell a live stream from a dead one.
10. [S10] All error responses go through one mapper: `ErrUnauthorized`→401,
    `ErrForbidden`→403, `ErrNotFound`→404, `ErrConflict`→409, `ErrNoCredits`→402,
    `ErrBufferFull`→429. One place, so a new sentinel cannot silently become a 500.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/httpapi/... -count=1 -race 2>&1 | tee /tmp/adr1-t7.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t7.out \
  && go test ./internal/router/... ./internal/identity/... -count=1 2>&1 | tee /tmp/adr1-t7r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t7r.out
```

Red at authoring: `internal/httpapi` does not exist.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestUnauthenticatedIsRejected` | `internal/httpapi/middleware_test.go` | all five routes are 401 without a token, **driven from the table returned by `New`** so a newly added unguarded route fails this test | — | S2, S3 |
| `TestEveryRouteIsMounted` | `internal/httpapi/api_test.go` | each of the five paths resolves to a handler rather than chi's 404 — the test that goes red when a route is deleted | — | S2 |
| `TestClaimRequiresWorkerRole` | `internal/httpapi/claim_test.go` | client and admin tokens get 403 on `POST /claim` | — | S3, S8 |
| `TestUploadBranchesOnRoleNotBody` | `internal/httpapi/upload_test.go` | a **client** token posting a worker-shaped JSON body gets 400 and completes no job — the role-confusion guard | — | S4 |
| `TestUploadClientCreatesJob` | `internal/httpapi/upload_test.go` | multipart from a client yields 201 and a `queued` job | — | S5 |
| `TestUploadRejectsOversizeBody` | `internal/httpapi/upload_test.go` | a body past the cap is refused without buffering it all | — | S5 |
| `TestUploadWorkerCompletesJob` | `internal/httpapi/upload_test.go` | worker JSON with pages yields 204 and a `done` job | — | S6 |
| `TestUploadWorkerReportsFailure` | `internal/httpapi/upload_test.go` | worker JSON with `error` requeues the job | — | S6 |
| `TestFilesClientGetsResultAndIsCharged` | `internal/httpapi/files_test.go` | a client gets the pages JSON and the balance moves | — | S7 |
| `TestFilesClientSecondFetchIs404` | `internal/httpapi/files_test.go` | the result is gone after delivery | — | S7 |
| `TestFilesWorkerNeedsLease` | `internal/httpapi/files_test.go` | a worker without the lease gets 409; the lease holder gets the bytes | — | S7 |
| `TestFilesRejectsNonUUIDPath` | `internal/httpapi/files_test.go` | `../../etc/passwd` and a plain word are 404 before any filesystem call | — | S7 |
| `TestUploadReadsLabelAndParams` | `internal/httpapi/upload_test.go` | `?label=crawl&url=x` yields a job with that label and `{"url":"x"}` as params, and the reserved `label`/`pipeline` keys do not leak into params | — | S5 |
| `TestUploadAcceptsPipeline` | `internal/httpapi/upload_test.go` | `?pipeline=crawl,strip-html` yields a two-stage job whose first label is `crawl` | — | S5 |
| `TestUploadWithNoBodyIsValid` | `internal/httpapi/upload_test.go` | a params-only POST with no multipart body yields 201, not 400 — the crawler shape | — | S5 |
| `TestUploadRejectsUnknownLabelWithAvailable` | `internal/httpapi/upload_test.go` | an unknown label is 400 and the body names the labels that are available | — | S5 |
| `TestClaimIsLabelScoped` | `internal/httpapi/claim_test.go` | a worker claiming `?label=strip-html` never receives an `ocr` job | — | S8 |
| `TestClaimResponseCarriesParamsAndHasBlob` | `internal/httpapi/claim_test.go` | the claim response gives the worker everything it needs to build argv without a second request | — | S8 |
| `TestServicesListsLiveLabels` | `internal/httpapi/api_test.go` | `GET /services` lists a label while a worker advertising it is connected, and keeps listing it inside the grace window after that worker disconnects | — | S2 |
| `TestClaimEmptyQueueIs204` | `internal/httpapi/claim_test.go` | nothing claimable is 204 with an empty body, not 404 | — | S8 |
| `TestSSEClearsWriteDeadline` | `internal/httpapi/sse_test.go` | against a server with a 100ms `WriteTimeout`, a stream still delivers an event after 300ms — **fails if the deadline clear is removed**, which no markup or handler assertion would catch | — | S9 |
| `TestSSESendsBacklogOnConnect` | `internal/httpapi/sse_test.go` | a client with two `done` jobs receives both in `backlog` immediately on connect | — | S9 |
| `TestSSEDeliversReadyEvent` | `internal/httpapi/sse_test.go` | a `ready` published during the stream arrives on it | — | S9 |
| `TestSSEIsUserScoped` | `internal/httpapi/sse_test.go` | customer A's stream never receives customer B's job id | — | S9 |
| `TestSSESendsPing` | `internal/httpapi/sse_test.go` | with the interval shortened, a `ping` arrives on an idle stream | — | S9 |
| `TestSSEUnsubscribesOnDisconnect` | `internal/httpapi/sse_test.go` | after the client cancels, `bus.Subscribers(topic)` returns to 0 | — | S9 |
| `TestErrorStatusMapping` | `internal/httpapi/errors_test.go` | each sentinel maps to its status, table-driven over every sentinel in `core` so a new one cannot default to 500 | — | S10 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the twenty tests above |
| 2 — something selects it | `TestEveryRouteIsMounted` is the in-package check; `cmd/router` mounts `httpapi.New` (T8), and T8's mutation removes that mount so its end-to-end test goes red. Mounting and constructing are two selections and this rung covers only the first. |
| 3 — the caller can discover it | the wire contract in ADR §Wiring; the dashboard shows a client its token and the three URLs (T10) |
| 4 — it is used | T8's end-to-end test drives all four endpoints over a real `httptest` server |

## Mutation Log

## Invariants

- Every route is authenticated; there is no unauthenticated path except the dashboard's
  own login surface, which T10 owns.
- The role comes from the principal, never from the body, a header or the token's prefix.
- `GET /files/{id}` never mutates anything for a worker; for a client it delivers, which is
  the one deliberate exception and it is a POST-shaped action behind a GET **only** because
  delivery is idempotent-to-the-holder: the second call is a 404, not a second charge.
- Every SSE stream clears its write deadline and unsubscribes on disconnect.
- Every error response goes through the one mapper.

## Risks

- **`TestSSEClearsWriteDeadline` is the only test here that can catch a whole class of
  silent production failure**, and it is easy to write so that it cannot fail — if the test
  server has no `WriteTimeout`, it passes with the clear removed. The server in that test
  must set a short `WriteTimeout` explicitly.
- **A new route added later without auth** is invisible to a per-handler test. Driving
  `TestUnauthenticatedIsRejected` from the router's own route table rather than a written
  list is what keeps it honest.
- **Streaming the multipart part directly into `blob.Put`** means a client that aborts
  mid-upload leaves a partial blob; T4's atomic rename means the partial never appears
  under the final name.

## Stop Condition

Stop and ask if the operator wants `GET /files/{id}` to be non-charging with an explicit
acknowledgement endpoint instead — the ADR charges on a successful GET, and a client whose
connection drops mid-download has been charged for bytes it did not finish reading.

## Out of Scope

- The dashboard's routes — T10 mounts its own subtree.
- TLS (permanent: boundary: the router serves plain HTTP behind the operator's proxy).
- The binary and its flags — T8's.

## Verification Log
