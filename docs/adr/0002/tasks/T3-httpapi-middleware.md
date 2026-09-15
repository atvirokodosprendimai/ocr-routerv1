# Task ADR-0002-T3: Enforce the rate limit and log every request, inside the authenticated group

**Depends-on:** T1, T2
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `core.ErrRateLimited`, `httpapi.Deps.Limiter`, `httpapi.Deps.Logger`, `httpapi.Deps.Limits`, the two middlewares, `monitor.MetricRequestsThrottled`
**Consumes:** `ratelimit.Limiter` (T1), `logging.Logger` (T2), `monitor.Registry` (ADR-0001-T11)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the middleware order`, `the per-token key`, `the Retry-After header`, `the widened label allow-list`

## Goal

Apply the limiter to every authenticated request keyed on the caller's token, answer `429` with a
truthful `Retry-After`, and emit one structured line per request — with the middleware order that
makes both correct rather than merely present.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/core/errors.go` | edit | `ErrRateLimited` |
| `internal/httpapi/errors.go` | edit | map `ErrRateLimited` → `429` |
| `internal/httpapi/middleware.go` | edit | `rateLimit` and `logRequests` middlewares |
| `internal/httpapi/api.go` | edit | **the lines that APPLY them, and their order** — `Deps.Limiter`, `Deps.Logger`, `Deps.Limits` |
| `internal/monitor/registry.go` | edit | `MetricRequestsThrottled`, and `role` added to `allowedLabelNames` |
| `internal/monitor/monitor_test.go` | edit | T11's allow-list guard test gains `role`, in the same commit as the constant |
| `internal/httpapi/middleware_test.go` | add | the failing tests |

`api.go` is the selecting file: a middleware that exists and is not applied there limits nothing
and logs nothing, and every test written against the middleware function directly would still pass.
That is why the mutants bind to the `r.Use` lines rather than to the middleware bodies.

## Ordered Steps

1. [S1] Write the failing test first: a request past the burst returns `429` through the real route
   table, before the middleware is applied (TDD red). [proof: acceptance]
2. [S2] `core.ErrRateLimited`, mapped by `writeError` to `429`. It joins the existing sentinel
   errors rather than being a special case in one handler, so every route gets it uniformly.
3. [S3] `rateLimit` middleware: key on `principal(r).TokenID`, choose the `Limit` by
   `principal(r).Role`, and on refusal set `Retry-After` from `Limiter.RetryAfter` (rounded UP to
   the next whole second, since rounding down tells a client to retry before a token exists) and
   write `ErrRateLimited`.
4. [S4] ⚠ **`rateLimit` is applied INSIDE the `authenticate` group.** It needs `TokenID`, which
   only exists after authentication. Applied outside it would key on the zero Principal — one
   shared bucket for every caller in the system, which looks like a working rate limiter and is a
   global outage waiting for the second customer.
5. [S5] `logRequests` middleware is applied **OUTERMOST**, before `authenticate`, so it records the
   `401`s and `429`s too. A request log that only covers requests which succeeded in authenticating
   cannot answer "is someone hammering us with a revoked token", which is the question it is most
   needed for.
6. [S6] The log line carries method, **route pattern** from `chi.RouteContext(r.Context())`, status,
   duration, and — when present — `user_id`, `token_id`, `role`. The raw path is never logged: a
   `/files/{id}` path embeds a job id in a field meant to identify a route, and makes every line
   unique for the same reason T11 bounds its metric labels.
7. [S7] `ocrr_requests_throttled_total{role}` increments on every refusal, via the existing
   `monitor.Registry`. `role` is added to `allowedLabelNames` — a widening of a set T11 froze
   deliberately, justified on the same ground the original three were: a role is one of exactly
   three values fixed at compile time. T11's guard test is updated in the same commit so the
   constant and its test can never disagree.
8. [S8] A nil `Deps.Limiter` means no limiting and a nil `Deps.Logger` means `logging.Nop()`, so
   every existing `httpapi` test compiles and passes unchanged. The defaults are set in `New`, once,
   rather than checked at each use.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/httpapi/... -count=1 -race 2>&1 | tee /tmp/adr2-t3.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr2-t3.out \
  && go test ./internal/monitor/... ./internal/core/... -count=1 2>&1 | tee /tmp/adr2-t3r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr2-t3r.out
```

`httpapi` runs alone first so it carries the verdict; `monitor` and `core` run second as the
regression for the allow-list widening and the new sentinel error. Red at authoring: the new tests
name middlewares that do not exist, so the package does not compile and `^FAIL` matches.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestRequestPastBurstIs429` | `internal/httpapi/middleware_test.go` | driving `POST /upload` past the burst through the real route table returns `429` — red if the `r.Use` line is deleted from `api.go` | — | S3, S4 |
| `TestRetryAfterHeaderIsPresentAndPositive` | `internal/httpapi/middleware_test.go` | the `429` carries `Retry-After` parsing as an integer ≥ 1 — never 0, which tells a client to retry immediately into another refusal | — | S3 |
| `TestTwoTokensDoNotShareABucket` | `internal/httpapi/middleware_test.go` | exhausting one token's budget leaves a second token's requests succeeding — **red if the middleware is applied outside the authenticator**, where every caller keys on the zero Principal | — | S4 |
| `TestTwoTokensOfTheSameUserDoNotShareABucket` | `internal/httpapi/middleware_test.go` | two tokens minted for one user limit independently — the backlog asked for per-TOKEN, and a per-user implementation passes the test above while failing this one | — | S3 |
| `TestUnauthenticatedRequestConsumesNoBudget` | `internal/httpapi/middleware_test.go` | a flood of tokenless requests, then a valid one, which succeeds — proves the limiter runs after auth rather than before | — | S4 |
| `TestWorkerAndClientLimitsDiffer` | `internal/httpapi/middleware_test.go` | with a client burst of 2 and a worker burst of 10, the client is refused where the worker is not — red if the role lookup is dropped and one limit is applied to all | — | S3 |
| `TestUnauthorizedRequestIsLogged` | `internal/httpapi/middleware_test.go` | a `401` produces a log line with status 401 — **red if the logger is applied inside the auth group**, which is the mistake that reads as working | — | S5 |
| `TestThrottledRequestIsLogged` | `internal/httpapi/middleware_test.go` | a `429` produces a log line with status 429 | — | S5 |
| `TestLogUsesRoutePatternNotRawPath` | `internal/httpapi/middleware_test.go` | `GET /files/0192f…` logs `route=/files/{id}` and the raw uuid appears nowhere in the line — asserts the pattern IS present, so an empty route field fails too | — | S6 |
| `TestLogCarriesPrincipalFields` | `internal/httpapi/middleware_test.go` | an authenticated request logs `user_id`, `token_id` and `role`, and the bearer token's secret appears nowhere | — | S6 |
| `TestThrottleCounterIncrements` | `internal/httpapi/middleware_test.go` | a refusal moves `ocrr_requests_throttled_total{role="client"}` on a real registry | — | S7 |
| `TestRoleIsAnAllowedMetricLabel` | `internal/monitor/monitor_test.go` | `role` is accepted by the registry and the four unbounded names still panic — the widening is exactly one entry wide | — | S7 |
| `TestNilLimiterMeansNoLimiting` | `internal/httpapi/middleware_test.go` | an API built with no limiter serves 1000 requests without a 429 — what keeps every pre-existing test in this package honest | — | S8 |
| `TestNilLoggerDoesNotPanic` | `internal/httpapi/middleware_test.go` | an API built with no logger serves a request | — | S8 |
| `TestRateLimitedErrorMapsTo429` | `internal/httpapi/middleware_test.go` | `writeError(w, core.ErrRateLimited)` writes 429 with the standard error body shape | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the fifteen tests above |
| 2 — something selects it | `api.go`'s two `r.Use` lines. `TestRequestPastBurstIs429` goes red when the limiter line is deleted; `TestUnauthorizedRequestIsLogged` goes red when the logger line is moved inside the auth group — which is the subtler of the two and the one a reviewer would not see. |
| 3 — the caller can discover it | the `429` status and `Retry-After` are standard and self-describing; the flags that configure the limits arrive in T4, and the README documents them there |
| 4 — it is used | `ocrr_requests_throttled_total{role}` makes throttling visible to the operator before a customer complains |

## Mutation Log

- 2026-09-15 · 0dc1656* · mutant killed · exit 1 · `internal/httpapi/api.go` · the limiter is built and configured but never applied to any route, so every limit reads as generous rather than as absent · acceptance-sha256:71e7fc057e0f19127fb59fbf258939c020247f90e64bf40ac521c4aad1adefd6 · covers:the middleware order
- 2026-09-15 · 0dc1656* · mutant killed · exit 1 · `internal/httpapi/middleware.go` · the limit becomes per user rather than per token, so a leaked credential consumes the legitimate one allowance — the exact threat the backlog named · acceptance-sha256:71e7fc057e0f19127fb59fbf258939c020247f90e64bf40ac521c4aad1adefd6 · covers:the per-token key
- 2026-09-15 · 0dc1656* · mutant killed · exit 1 · `internal/httpapi/middleware.go` · a 429 carries no Retry-After, so a client has to guess when to come back and typically retries immediately into another refusal · acceptance-sha256:71e7fc057e0f19127fb59fbf258939c020247f90e64bf40ac521c4aad1adefd6 · covers:the Retry-After header
- 2026-09-15 · 0dc1656* · mutant killed · exit 1 · `internal/monitor/registry.go` · the throttle counter panics at its first refusal, taking down the request it was meant to observe · acceptance-sha256:71e7fc057e0f19127fb59fbf258939c020247f90e64bf40ac521c4aad1adefd6 · covers:the widened label allow-list

## Invariants

- The limiter keys on the token id, never the user id and never the remote address.
- The limiter runs after authentication; the request logger runs before it.
- Every `429` carries a `Retry-After` of at least 1.
- No bearer token secret and no param value ever reaches a log line.
- `allowedLabelNames` gains exactly one entry, and its guard test changes in the same commit.

## Risks

- **Middleware order is invisible in the diff and untested by default.** Both order mistakes
  produce a system that works in the common case: the limiter outside auth limits everyone
  together, and the logger inside auth loses exactly the lines an operator needs. Two tests exist
  solely to pin the order, and they are the ones to keep if anything is cut.
- **`TestTwoTokensDoNotShareABucket` passes against a per-USER implementation** when the two tokens
  belong to different users. `TestTwoTokensOfTheSameUserDoNotShareABucket` is the one that
  distinguishes them, and the fixture must mint both tokens for one user id or it proves nothing.
- **Rounding `Retry-After` down yields 0 for sub-second waits**, telling a client to retry straight
  into another refusal. Round up; the test asserts ≥ 1.
- **Widening the metric allow-list is precedent** for the next field. Argued from the bounded-enum
  property rather than from convenience, and T11's guard test is updated alongside so the two
  cannot drift.
- **The log tests must read real emitted bytes**, not a fake logger. A mock asserts the call was
  made; only the bytes show whether a secret was in them.

## Stop Condition

Stop and ask if the operator wants `/claim` exempt from limiting. Workers poll it by design, and a
limit tuned for uploads could throttle the pollers into a stall — the default worker limit is set
well above the poll rate for that reason, but the exemption is a real alternative and it is the
operator's call, not this task's.

## Out of Scope

- The flags, the eviction tick, and transition logging — T4.
- Limiting unauthenticated requests (permanent: boundary: a tokenless caller is rejected before any
  database work, and defending the socket is a reverse proxy's job).
- Per-route limits as opposed to per-role (deferred: `docs/adr/BACKLOG.md`).

## Verification Log
- 2026-09-15 · 0dc1656* · exit 0 · `set -o pipefail …` · acceptance-sha256:71e7fc057e0f19127fb59fbf258939c020247f90e64bf40ac521c4aad1adefd6 · ms:8018
- 2026-09-15 · 0dc1656* · exit 0 · `set -o pipefail …` · acceptance-sha256:71e7fc057e0f19127fb59fbf258939c020247f90e64bf40ac521c4aad1adefd6 · ms:5881
- 2026-09-15 · 0dc1656* · exit 0 · `set -o pipefail …` · acceptance-sha256:71e7fc057e0f19127fb59fbf258939c020247f90e64bf40ac521c4aad1adefd6 · ms:5637
- 2026-09-15 · 0dc1656* · exit 0 · `set -o pipefail …` · acceptance-sha256:71e7fc057e0f19127fb59fbf258939c020247f90e64bf40ac521c4aad1adefd6 · ms:5583
- 2026-09-15 · 0dc1656* · exit 0 · `set -o pipefail …` · acceptance-sha256:71e7fc057e0f19127fb59fbf258939c020247f90e64bf40ac521c4aad1adefd6 · ms:5538
