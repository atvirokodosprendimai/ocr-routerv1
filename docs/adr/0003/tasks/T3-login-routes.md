# Task ADR-0003-T3: The login page, the cookie, and a CSRF check that fails closed

**Depends-on:** T1, T2
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary: httpapi, web, views)
**Owner:** unassigned
**Produces:** `GET/POST /admin/login`, `POST /admin/logout`, the session cookie, `httpapi` cookie fallback, the `Origin` guard, the rewritten unauthenticated-route invariant
**Consumes:** `identity.Login` and `identity.ResolveSession` (T2), `ratelimit.Limiter` (ADR-0002 T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the closed-by-default origin check`, `the cookie attributes`, `the header-first precedence`, `the admin-only cookie scope`, `the login rate limit`

## Goal

Give a browser a way in: a login page, a session cookie with the attributes that make it safe, and a
CSRF guard that refuses a request carrying no `Origin` rather than waving it through.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/web/views/login.templ` | add | the login page — form, error state, and the TLS-required message. A file of its own rather than an addition to `views.templ`: it is the one view that renders a whole document and shares no layout with the dashboard |
| `internal/web/login.go` | add | the three handlers, the cookie, the `Origin` guard, and the login rate limit |
| `internal/web/web.go` | edit | **`r.Get("/login")` and `r.Post("/login")` mounted OUTSIDE the authenticated group**, `r.Post("/logout")` inside it, the `Origin` guard on the rest, and `Deps.Limiter` |
| `internal/httpapi/middleware.go` | edit | `resolveCaller` — the cookie fallback inside `authenticate`, and the `/admin`-only scope |
| `internal/web/login_test.go` | add | the login, cookie and CSRF tests, over a **TLS** test server |
| `internal/httpapi/session_test.go` | add | cookie-fallback precedence and scope tests. A new file rather than an edit to `middleware_test.go`, because it needs a chi router with an `/admin` subtree that `middleware_test.go`'s fixture does not build |
| `cmd/router/monitoring_test.go` | edit | **rewrite** `TestHealthzIsTheOnlyUnauthenticatedRoute` → `TestOnlyLoginAndHealthzAreUnauthenticated` |

`web.go`'s three route lines are the selecting lines: a login handler that exists and is mounted
inside the authenticated group is a page you must already be logged in to see.

## Ordered Steps

1. [S1] Write the failing test first: `GET /admin/login` with no credential returns 200 and an HTML
   form, before the route exists (TDD red). [proof: acceptance]
2. [S2] `GET /admin/login` renders the form — email, password, submit — in the existing templ
   layout. It is **unauthenticated**, and it is the second such route in the process.
3. [S3] `POST /admin/login` calls `identity.Login`, and on success sets
   `Set-Cookie: ocrr_session=<secret>; Path=/admin; HttpOnly; Secure; SameSite=Strict; Max-Age=43200`
   and redirects to `/admin`. On failure it re-renders the form with ONE generic message — never
   "no such user" or "wrong password", which would undo T2's collapsed error.
4. [S4] ⚠ **Refuse login over plain HTTP with an explicit message.** A `Secure` cookie set over
   `http://` is silently dropped by the browser, so the operator would see a login that appears to
   succeed and lands back on the form forever. Detect it (`r.TLS == nil` and no
   `X-Forwarded-Proto: https`) and say so, naming TLS.
5. [S5] `POST /admin/logout` revokes the session server-side, clears the cookie with `Max-Age=-1`,
   and redirects to the login page. Both halves: clearing the cookie without revoking leaves a live
   credential in anyone's hands who copied it.
6. [S6] `authenticate` gains the fallback: `Authorization` header **first**, then — only when the
   header is absent AND the path is under `/admin` — the `ocrr_session` cookie. A request that
   presents a token is authenticated as that token, whatever cookie it also carries.
7. [S7] ⚠ **The `Origin` guard, written CLOSED.** On every state-changing request under `/admin`
   (POST, PUT, PATCH, DELETE) authenticated **by cookie**, require `Origin` to match the request's
   own host, falling back to `Referer`. A request with NEITHER is refused with `403`. The obvious
   spelling — `if origin != "" && origin != want { reject }` — permits every request that omits the
   header, which is every request an attacker writes by hand.
8. [S8] The login route is rate limited via ADR-0002's limiter, keyed on the **normalised email**,
   with a tighter limit than the API's. Not the IP: ADR-0001 puts callers behind NATs. It is a rate
   limit and not a lockout — the real administrator must always get through eventually.
9. [S9] Rewrite `TestHealthzIsTheOnlyUnauthenticatedRoute` as
   `TestOnlyLoginAndHealthzAreUnauthenticated`: walk the real chi route table, assert every route
   401s except an explicit allow-list of `/healthz`, `GET /admin/login`, `POST /admin/login`.
   ⚠ **Rewritten, not deleted** — it is the only guard on that count and it is being changed at the
   moment the count changes.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && templ generate \
  && go test ./internal/web/... -count=1 -race 2>&1 | tee /tmp/adr3-t3.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr3-t3.out \
  && go test ./internal/httpapi/... ./cmd/router/... -count=1 2>&1 | tee /tmp/adr3-t3r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr3-t3r.out
```

`internal/web` carries the verdict; `httpapi` and `cmd/router` run second, covering the fallback and
the rewritten invariant. Red at authoring: the route does not exist, so the new test 404s.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestLoginPageIsReachableWithoutCredentials` | `internal/web/login_test.go` | `GET /admin/login` with no header and no cookie returns 200 and HTML containing a password input — **the whole point of this record**, and red today | — | S2 |
| `TestLoginSetsASessionCookie` | `internal/web/login_test.go` | correct credentials over TLS produce a `Set-Cookie: ocrr_session=` and a redirect to `/admin` | — | S3 |
| `TestSessionCookieAttributes` | `internal/web/login_test.go` | the cookie is `HttpOnly`, `Secure`, `SameSite=Strict` and `Path=/admin` — four assertions, each naming what its absence costs | — | S3 |
| `TestCookieValueIsNotTheUsersToken` | `internal/web/login_test.go` | the cookie value matches no token in the `tokens` table — red if someone "simplifies" this into putting the bearer token in the cookie, the alternative this record rejected | — | S3 |
| `TestFailedLoginSaysNothingSpecific` | `internal/web/login_test.go` | wrong password and unknown email render the SAME message, and neither contains the email — T2's collapsed error survives the trip through the UI | — | S3 |
| `TestLoginOverPlainHTTPExplainsItself` | `internal/web/login_test.go` | a login POST without TLS returns a body naming TLS and sets no cookie — red if it silently sets a cookie the browser will drop | — | S4 |
| `TestLogoutRevokesServerSide` | `internal/web/login_test.go` | after logout the OLD cookie value is refused even when replayed directly — asserts revocation, not merely that a clearing header was sent | — | S5 |
| `TestLogoutClearsTheCookie` | `internal/web/login_test.go` | the response carries `ocrr_session=` with `Max-Age=-1` | — | S5 |
| `TestCookieAuthenticatesTheDashboard` | `internal/httpapi/session_test.go` | a request to `/admin` with only the session cookie resolves to the admin principal | — | S6 |
| `TestHeaderWinsOverCookie` | `internal/httpapi/session_test.go` | a request carrying BOTH a client bearer token and an admin session cookie is authenticated as the **client** — red if the cookie is consulted first, which would silently escalate every API call from a browser | — | S6 |
| `TestCookieIsRefusedOutsideAdmin` | `internal/httpapi/session_test.go` | the same cookie on `POST /upload` and `POST /claim` is rejected with 401 — **the property that keeps CSRF out of the API**, and no test in `internal/web` can see it | — | S6 |
| `TestStateChangingRequestWithNoOriginIsRefused` | `internal/web/login_test.go` | `POST /admin/users` authenticated by cookie with NO `Origin` and NO `Referer` returns 403 — **the single most likely bug in this record**, and the check that distinguishes a closed guard from an open one | — | S7 |
| `TestStateChangingRequestWithForeignOriginIsRefused` | `internal/web/login_test.go` | `Origin: https://evil.example` returns 403 | — | S7 |
| `TestStateChangingRequestWithMatchingOriginSucceeds` | `internal/web/login_test.go` | the dashboard's own `Origin` is accepted — without this the two tests above are satisfied by a guard that refuses everything | — | S7 |
| `TestOriginGuardDoesNotApplyToTokenAuth` | `internal/web/login_test.go` | an API client using a bearer token on a POST is unaffected by the guard — a token is not attached automatically, so it has no CSRF exposure and must not pay for one | — | S7 |
| `TestGetRequestsAreNotOriginChecked` | `internal/web/login_test.go` | `GET /admin` with no `Origin` still renders — the guard covers state-changing methods only, or every navigation breaks | — | S7 |
| `TestLoginIsRateLimitedPerEmail` | `internal/web/login_test.go` | repeated failures for one email are throttled while a DIFFERENT email still gets through — both halves, since a global limit would pass the first alone and lock out every admin | — | S8 |
| `TestLoginRateLimitIsNotALockout` | `internal/web/login_test.go` | after the limiter refills, the correct password still works — an attacker must not be able to lock out the real administrator by guessing | — | S8 |
| `TestOnlyLoginAndHealthzAreUnauthenticated` | `cmd/router/monitoring_test.go` | walks the binary's real route table; every route 401s except `/healthz` and the two login routes — the standing guard named in the record's `Enforced-by:` | — | S9 |
| `TestRequestLogNeverContainsTheSessionCookie` | `internal/httpapi/session_test.go` | a request carrying a session cookie produces a log line containing neither the cookie value nor a `Cookie` field — "already true" is not "will stay true" | — | S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the twenty tests above |
| 2 — something selects it | `web.go`'s three `r.Get`/`r.Post` lines outside the auth group. `TestLoginPageIsReachableWithoutCredentials` goes red if the login route is moved inside it — which is the mistake that produces a login page you must be logged in to reach. |
| 3 — the caller can discover it | a browser hitting `/admin` unauthenticated is redirected to `/admin/login`; the README documents the flow |
| 4 — it is used | human sign-off in T4: a real browser, a real login, a real logout |

## Mutation Log

- 2026-09-15 · 7fe24bf* · mutant killed · exit 1 · `internal/web/login.go` · the origin check fails OPEN, permitting every request that omits both headers — which is every cross-site request an attacker writes by hand · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · covers:the closed-by-default origin check
- 2026-09-15 · 7fe24bf* · mutant killed · exit 1 · `internal/httpapi/middleware.go` · the cookie is consulted before the header, so every API call made from a logged-in administrator browser silently runs with admin rights whatever token it presented · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · covers:the header-first precedence
- 2026-09-15 · 7fe24bf* · mutant killed · exit 1 · `internal/httpapi/middleware.go` · the session cookie is honoured on every API route, so POST /upload and POST /claim become reachable by CSRF from a logged-in administrator browser · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · covers:the admin-only cookie scope
- 2026-09-15 · 7fe24bf* · mutant killed · exit 1 · `internal/web/login.go` · the session cookie loses SameSite=Strict, so a cross-site POST carries it and the primary CSRF defence is gone · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · covers:the cookie attributes
- 2026-09-15 · 7fe24bf* · mutant killed · exit 1 · `internal/web/login.go` · login is unthrottled, so an attacker can spend 64MB and tens of milliseconds of the router per guess with no credential at all · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · covers:the login rate limit

## Invariants

- The `Authorization` header always wins over the cookie.
- The session cookie is never accepted outside `/admin`.
- A cookie-authenticated state-changing request with no `Origin` and no `Referer` is refused.
- The login response never distinguishes its failure causes.
- Exactly three routes answer without a credential.

## Risks

- **The `Origin` check is the single most likely thing to get wrong**, because the natural phrasing
  fails open and reads as correct. `TestStateChangingRequestWithNoOriginIsRefused` exists for
  exactly that spelling, and a mutant binds to it.
- **`TestCookieIsRefusedOutsideAdmin` cannot be written in `internal/web`** — that package does not
  know `/upload` exists. It belongs in `httpapi`, where the whole route table is visible, and
  putting it in the convenient place would leave the API's CSRF exposure untested.
- **A login test that only checks for a `Set-Cookie` header proves very little.** The attribute test
  is separate and asserts each of the four properties by name, because losing `HttpOnly` or
  `Secure` costs something different from losing `SameSite`.
- **`TestLogoutRevokesServerSide` must replay the OLD cookie value**, not merely observe the
  clearing header. A logout that only clears the cookie leaves a live credential with anyone who
  copied it, and the clearing-header assertion passes regardless.
- **The rate-limit test needs a tight limit in the fixture.** At the production default it would
  pass without ever throttling — the same trap ADR-0002's worker-429 test documented.
- **Rewriting the unauthenticated-route test could widen it silently.** The rewrite asserts an
  explicit allow-list of three and fails on a fourth, rather than counting.
- **`templ generate` must run before the web tests**, and it is in the Acceptance fence for that
  reason: a stale `views_templ.go` would test the previous markup.

## Stop Condition

Stop and ask if the operator terminates TLS somewhere that does not set `X-Forwarded-Proto`. The
plain-HTTP refusal in S4 depends on detecting TLS correctly, and a proxy that strips or omits that
header would make login refuse itself in production — which is a deployment fact this repository
cannot check.

## Out of Scope

- The CLI that sets passwords, and the reaper sweep — T4.
- A synchroniser CSRF token (permanent: boundary: `SameSite=Strict` plus a closed origin check
  already cover this threat; a third mechanism threaded through every datastar POST fails silently
  when wrong).
- Remembering the requested URL across a login redirect (deferred: `docs/adr/BACKLOG.md`).
- Self-service password change in the UI (deferred: `docs/adr/BACKLOG.md`).

## Verification Log
- 2026-09-15 · 7fe24bf* · exit 0 · `set -o pipefail …` · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · ms:46263
- 2026-09-15 · 7fe24bf* · exit 0 · `set -o pipefail …` · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · ms:46215
- 2026-09-15 · 7fe24bf* · exit 0 · `set -o pipefail …` · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · ms:41578
- 2026-09-15 · 7fe24bf* · exit 0 · `set -o pipefail …` · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · ms:41346
- 2026-09-15 · 7fe24bf* · exit 0 · `set -o pipefail …` · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · ms:40507
- 2026-09-15 · 7fe24bf* · exit 0 · `set -o pipefail …` · acceptance-sha256:610e687ed4eade47637b1d8a5d6285489334db18dc6d85d78fea76591cb7c960 · ms:41813
