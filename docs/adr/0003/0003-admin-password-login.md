# ADR-0003: Let an administrator reach the dashboard from a browser with an email and password

**Status:** Accepted
**Date:** 2026-09-15
**Owner:** M (operator) — authored by claude-code-aks
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/tasks/T10-admin-dashboard.md`, `docs/adr/0002/0002-rate-limiting-and-structured-logging.md`
**Governs:** `internal/identity/**`, `internal/session/**`, `internal/web/**`
**Enforced-by:** `cmd/router/monitoring_test.go::TestOnlyLoginAndHealthzAreUnauthenticated`
**Invalidates:** none. This record CHANGES no accepted decision — see Context; what it contradicts
is a source comment, not a record. Checked with `adr-state.mjs` and
`grep -rn "second authentication" docs/adr/` (2026-09-15, no match).
**Served-path change:** An administrator can open `https://…/admin` in a browser, be shown a login
form, sign in with an email and password, and use the dashboard. Today that navigation returns
`401` and there is no way to proceed from a browser at all.

## Context

T10 built the admin dashboard — templ views, datastar live updates, user creation, token minting,
per-label rate editing — and mounted it at `/admin` behind the API's bearer-token authenticator.
That authenticator reads exactly one thing (`internal/httpapi/middleware.go:43`):

```go
p, err := a.deps.Identity.Authenticate(r.Context(), r.Header.Get("Authorization"), a.deps.Now())
```

**A browser navigating to `/admin` sends no `Authorization` header.** Measured against a running
binary, 2026-09-15:

```
curl -i …/admin                                 → 401, WWW-Authenticate: Bearer realm="ocr-router"
curl -H "Authorization: Bearer ocr_a_…" …/admin → 200
```

The `WWW-Authenticate` header does not rescue it: browsers render a native credential prompt for
`Basic` and `Digest` and never for `Bearer`. So the dashboard is reachable by `curl` and by a
header-injecting extension, and by no ordinary human. T10's own Reachability rung 4 asked for a
two-tab **browser** sign-off, which could only have been satisfied by such an extension — the gap
was visible in the task and nobody read it that way.

**What this record contradicts is a comment, not a decision.** `internal/web/web.go:62` says *"A
dashboard with its own login is a second authentication system to keep correct, and the second one
is always the one that rots."* That sentence appears in no ADR (`grep -rn` over `docs/adr/`,
2026-09-15: no match) — it was written during T10's implementation and reasoned about
**credentials**, which is correct, but it was applied to **forms**, which is what left the UI
unreachable. The concern it names is real and this record keeps it: there is exactly one credential
store, one `identity` package, and one `core.Principal`.

ADR-0001's nearest actual clause points the same way. It rejected OAuth/JWT with the disposition
*"a revocable hashed row in the same SQLite file is simpler and nothing here needs offline
validation"* (`docs/adr/0001-ocr-router-architecture.md:473`). A server-side session is precisely a
revocable hashed row in that file, so this design sits inside that boundary rather than against it.

The operator's decision, taken 2026-09-15, is **email and password for administrators**, over the
cheaper alternative of pasting a bearer token into a form.

## Existing Primitives Audit

Checked before proposing anything new (`grep`, `go list -m`, and `adr-context` over
`internal/identity`, `internal/web`, `internal/httpapi`, `internal/store`, 2026-09-15):

| Need | Existing primitive | Reused? |
|---|---|---|
| Turning a credential into a `core.Principal` | `identity.Service.Authenticate` | **Yes** — a session resolves to the same `core.Principal` shape, through the same package. There is no second notion of "who you are" |
| A revocable, hashed, expiring credential row | `tokens` table + `identity.HashToken` (SHA-256) | **Yes** — a session row is the same design with an expiry: random secret in the cookie, SHA-256 in the database |
| Constant-time secret comparison | `crypto/subtle` in `Authenticate` | **Yes** — same discipline for the session lookup |
| Collapsing failure causes to one error | `core.ErrUnauthorized`, deliberately one error for unknown/revoked/deactivated | **Yes, and extended** — "no such email" and "wrong password" must also collapse, or login becomes an account-enumeration oracle |
| Per-caller request throttling | `ratelimit.Limiter` + `httpapi` middleware (ADR-0002) | **Yes** — but keyed differently; see Decision 4, since a login has no token to key on |
| Sweeping expired rows on a schedule | `App.StartReaper` ticker, already sweeping leases, deadlines, results and limiter entries | **Yes** — expired sessions ride the same tick. No second goroutine |
| Password hashing | none in-tree | **No** — `golang.org/x/crypto/argon2`, already an indirect dependency at v0.55.0, pure Go, no cgo |
| A session/cookie library | none in-tree | **No, and none is added** — `net/http` has `http.Cookie`; a session is one table and two queries |
| Rendering a form | `internal/web/views` (templ) | **Yes** — the login page is one more templ view in the existing layout |

## Decision

**We will give administrators a password, authenticate them at `GET/POST /admin/login` into a
server-side session carried by a `Secure; HttpOnly; SameSite=Strict` cookie, and teach the existing
authenticator to fall back to that cookie when no `Authorization` header is present.** Bearer tokens
keep working unchanged and remain the only credential the API accepts. The five sections below state
each part and what it costs.

### 1. Passwords are argon2id, on the user row, and only administrators have one

`users.password_hash TEXT NOT NULL DEFAULT ''`. An empty string means **this account cannot log in
with a form**, which is the state of every client and worker account and of every user that exists
today. Migration `00002` adds the column with that default, so it is backward compatible by
construction rather than by a backfill.

**argon2id**, via `golang.org/x/crypto/argon2` — pure Go, no cgo, and already in the module graph as
an indirect dependency. Parameters are stored **with** the hash in the standard PHC string
(`$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>`), so raising the cost later does not invalidate
existing hashes: verification reads the parameters out of the stored string, and only a new password
uses the new cost.

⚠ **Verification runs the KDF even when the account does not exist or has no password**, against a
fixed dummy hash, and discards the result. Without it the response time says whether an email is
registered — argon2 at these parameters takes tens of milliseconds and a missing row takes
microseconds, which is not a subtle difference. The failure is `core.ErrUnauthorized` in every case,
extending the rule `Authenticate` already follows for unknown/revoked/deactivated tokens.

**Policy: minimum 12 characters, no composition rules.** Length is the only requirement that
survives contact with how people actually choose passwords; mandated symbol classes produce
`Password1!` and a sticky note.

### 2. A session is a revocable hashed row, like a token with an expiry

New table `sessions`: `id`, `user_id`, `token_hash`, `created_at`, `expires_at`, `revoked`. The
cookie carries 32 bytes of `crypto/rand`, base64url; the database stores its SHA-256. **The
plaintext is never stored**, so a database read cannot impersonate anyone — the same property the
`tokens` table already has, reusing `identity.HashToken`.

**Absolute expiry of 12 hours, not a sliding window.** A sliding session that is refreshed on every
request never ends for anyone who keeps a tab open, which is how a browser left on a laptop becomes
a permanent credential. Twelve hours is one working day.

`POST /admin/logout` revokes the row — server-side, so it is a real logout and not merely a cleared
cookie. Expired and revoked rows are swept on the existing reaper tick.

### 3. The authenticator gains a cookie fallback; the API surface does not

`httpapi.authenticate` tries the `Authorization` header **first**, and only if it is absent consults
the session cookie. Header-first matters: an API client that somehow also carried a cookie must be
authenticated as the token it presented.

⚠ **The cookie is accepted only under `/admin`.** A session must not be usable against `POST
/upload` or `POST /claim`, because a cookie is attached by the browser automatically and a bearer
token is not — that difference is the entire CSRF threat model, and confining the cookie to the
subtree that has CSRF defences is what keeps the API out of it. The API's credential remains the
bearer token, exactly as ADR-0001 specified.

Both paths produce the same `core.Principal`, so `requireAdmin` and every dashboard handler are
unchanged.

### 4. CSRF, and a login limit keyed on the email

Cookie authentication introduces a threat bearer tokens do not have. Three layers, in order:

- **`SameSite=Strict`** on the session cookie. A cross-site POST does not carry it, which defeats
  the classic attack outright.
- **An `Origin` check** on every state-changing request under `/admin`. `SameSite` is the mechanism;
  this is the backstop for a browser that does not honour it, and it fails **closed** — a request
  with no `Origin` header and no `Referer` is refused rather than allowed.
- **`Secure` and `HttpOnly`**, so the cookie is TLS-only and invisible to scripts.

A synchroniser token is deliberately **not** added: with `SameSite=Strict` plus a closed-by-default
origin check it is a third mechanism against the same threat, and it would have to be threaded
through every datastar POST in the dashboard, where the failure mode of getting it wrong is a form
that silently stops working.

**The login route is rate limited, keyed on the normalised email**, using ADR-0002's limiter. It
cannot key on a token — there isn't one yet — and it deliberately does not key on the IP address:
ADR-0001 puts callers anywhere on the internet, behind NATs and proxies. Keying on the email throttles
credential stuffing against one account. **It is a rate limit, never a lockout**: an attacker must
not be able to lock a real administrator out by guessing at their email.

### 5. Passwords are set from the CLI, on the machine that owns the database

`router admin bootstrap --email … --password …` sets the first administrator's password alongside
minting its token, and `router admin set-password --email …` changes one. Both read the password
from a **prompt with echo disabled** when the flag is absent, because a password on a command line
is in the shell history and in `ps`.

**The dashboard does not offer password reset or self-service change in this record.** A reset flow
needs an email channel this system does not have; an operator with shell access has `set-password`,
which is the honest recovery path for a single-tenant admin dashboard.

## Alternatives Considered

- **Paste the bearer token into a form, store it in the cookie:** the cheapest option — no password
  column, no KDF, no session table, and one credential store by construction. Rejected by the
  operator on 2026-09-15 in favour of a real login. It is worth recording why it was tempting: it
  makes the cookie a transport for an existing credential rather than a new one. It is also worth
  recording why it is worse than it looks — it puts a long-lived API token, which mints other
  tokens, into a browser cookie jar, where a token that leaks cannot be rotated without breaking
  whatever else uses it.

- **HTTP Basic authentication:** the browser renders the prompt itself, so there is no form to build
  and no cookie to protect. Rejected because the credential is replayed on every request, there is
  no logout that works reliably across browsers, and the native dialog cannot be styled or explained
  — a dashboard whose only sign-in affordance is a grey OS box is a worse product than the one T10
  built.

- **OAuth / OIDC against an external identity provider:** the right answer for a multi-tenant
  product with existing corporate accounts. Rejected because ADR-0001 already took this decision for
  the API (`:473`), the reasoning is unchanged, and it makes a single-process SQLite service depend
  on an external service being reachable in order for anyone to log in.

- **A signed stateless cookie (HMAC or JWT) instead of a session table:** no table, no sweep, no
  read on each request. Rejected because it **cannot be revoked** — logout would clear the cookie
  while the credential inside it stays valid until expiry, and an admin credential is exactly the
  one worth being able to kill. The table costs one indexed lookup per request against a local file.

- **bcrypt instead of argon2id:** perfectly respectable, and `x/crypto/bcrypt` is equally available.
  Rejected because bcrypt silently truncates at 72 bytes, which quietly weakens passphrases — the
  kind of password the length-only policy above is meant to encourage — and argon2id is the current
  recommendation with memory-hardness bcrypt does not have.

- **A sliding session refreshed on each request:** friendlier, and what most applications do.
  Rejected for an administrative credential: a tab left open on an unlocked laptop becomes a
  permanent session, and the convenience is small when the alternative is logging in once a day.

- **A synchroniser CSRF token:** see Decision 4 — a third mechanism against a threat two already
  cover, threaded through every datastar POST, whose failure mode is a silently broken form.

- **Sessions in memory rather than in SQLite:** faster, and no migration. Rejected because every
  restart would log out every administrator, and the router is restarted for every deploy.

- **Keying the login rate limit on the client IP:** the conventional choice. Rejected for this
  system specifically — ADR-0001 puts callers anywhere on the internet, so one NAT is many admins
  and one attacker is many IPs.

## Component / Boundary Impact

No new bounded context. One new leaf package and one widened existing one:

- **`internal/session`** (new) — the session table's read and write API. Depends on
  `internal/store` and `internal/core` only. Consumed by `internal/identity` and `internal/httpapi`.
- **`internal/identity`** (widened) — gains password hashing/verification and session issue/resolve.
  It remains **the only package that decides who a caller is**, which is the property that keeps
  this from becoming the second authentication system the T10 comment feared.
- **`internal/web`** (widened) — one new templ view and three routes.
- **`internal/httpapi`** (widened) — a cookie fallback inside the existing authenticator, not a
  second middleware.

The C4 container diagram is unchanged: no new process, no new listener, no external dependency. No
repository architecture document exists (`docs/architecture.md` absent, checked 2026-09-15), so
ADR-0001 plus this record are the structural sources.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| Database schema | `users.password_hash TEXT NOT NULL DEFAULT ''` (migration `00002`) | T1 | `internal/identity` |
| Database schema | new `sessions` table + index on `token_hash` (migration `00002`) | T2 | `internal/session` |
| HTTP | `GET /admin/login`, `POST /admin/login`, `POST /admin/logout` — the **second and third unauthenticated routes** in the process | T3 | browser |
| HTTP | `Set-Cookie: ocrr_session=…; Secure; HttpOnly; SameSite=Strict; Path=/admin` | T3 | browser |
| HTTP | `403` on a state-changing `/admin` request with a bad or absent `Origin` | T3 | browser |
| `core` errors | reuses `core.ErrUnauthorized`; no new sentinel | T1 | `internal/httpapi` |
| CLI | `admin bootstrap --password`, new `admin set-password` | T4 | operator |
| `go.mod` | `golang.org/x/crypto` promoted from indirect to direct | T1 | — |
| SSE protocol | **None** | — | — |
| Metrics | **None** — login failures are visible in the request log; a counter would need an unbounded label to be actionable | — | — |

⚠ **The invariant this record changes:** ADR-0001 made `/healthz` the only unauthenticated route,
and `cmd/router/monitoring_test.go::TestHealthzIsTheOnlyUnauthenticatedRoute` asserts it. That test
must be **rewritten, not deleted**, into one that enumerates the real route table and permits exactly
`/healthz` and the two login routes. Deleting it would remove the only guard on the count, at the
moment the count stops being one.

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `users.password_hash` column (migration `00002`) | T1 | T2, T3, T4 | No — `DEFAULT ''`, so every existing row is valid |
| `identity.HashPassword()` / `VerifyPassword()` | T1 | T3, T4 | No — new functions |
| `sessions` table + `session.Store` | T2 | T3, T4 | No — new table |
| `identity.Login()` returning a session secret | T2 | T3, T4 | No — new method |
| `identity.ResolveSession()` returning a `core.Principal` | T2 | T3 | No — new method |
| Cookie fallback in `httpapi.authenticate` | T3 | T4 | No — header still wins; API behaviour unchanged |
| `session.Store.SweepExpired()` | T2 | T4 | No — called from the existing reaper tick |
| Rewritten unauthenticated-route invariant test | T3 | T4 | **Yes, deliberately** — the count it asserts goes from one to three, and T3 owns the rewrite |

T4 is last: it is the composition root plus the CLI, and consumes all three.

## Implementation

Four tasks in `docs/adr/0003/tasks/`, in order: T1 (passwords) and T2 (sessions) are the primitives,
T3 is the HTTP surface and the UI, T4 is the CLI and the wiring. See
`docs/adr/0003/tasks/README.md`.

## Consequences

**Good.** The dashboard built in T10 becomes usable by the person it was built for. Logout actually
revokes. An admin credential in a browser is now a 12-hour session rather than a permanent API token
in a cookie jar. Nothing about the API changes, so no client is affected.

**Bad.** There is now a password to store, hash, rotate and recover — the cost the T10 comment named,
accepted deliberately rather than by oversight. Two more unauthenticated routes, which is a 200%
increase on a surface whose whole virtue was being exactly one. A CSRF threat that did not exist
while every credential was a header. And argon2id at these parameters costs ~50ms and 64MB per
login attempt, which is the point of it and is also a resource an unauthenticated caller can consume
— hence the rate limit in Decision 4.

**Neutral but load-bearing.** Sessions live in the same SQLite file as everything else, so they
inherit the single-writer serialisation. Login is a write (inserting a session row) on a
connection with `MaxOpenConns(1)`; at administrative login rates this is irrelevant, and it would
not be if this were a customer-facing login for thousands of users.

## Out of Scope

- Password reset by email (permanent: boundary: this system has no email channel, and an operator
  with shell access has `admin set-password`, which is the honest recovery path for a single-tenant
  admin dashboard).
- Self-service password change in the dashboard UI (deferred: `docs/adr/BACKLOG.md`).
- Two-factor authentication (deferred: `docs/adr/BACKLOG.md`).
- Passwords for client and worker accounts (permanent: boundary: they have no UI to log into — the
  API's credential is the bearer token, and `password_hash` stays empty for them).
- A customer-facing portal (permanent: fact: T10 recorded the operator's scope as an admin dashboard
  only; citation: file `docs/adr/tasks/T10-admin-dashboard.md:186`).
- OAuth / OIDC (permanent: fact: ADR-0001 took this decision for the API and nothing here changes
  its reasoning; citation: file `docs/adr/0001-ocr-router-architecture.md:473`).
- Account lockout after N failures (permanent: boundary: a lockout keyed on something an attacker
  supplies is a denial-of-service against the real administrator; the rate limit is the chosen
  mitigation).
- Audit logging of logins as a tamper-evident stream (deferred: `docs/adr/BACKLOG.md`).

## Risks

| # | Risk | Mitigation |
|---|---|---|
| 1 | **Login becomes an account-enumeration oracle** via timing: a missing row returns in microseconds where a real verification takes ~50ms. | Verify against a fixed dummy hash when the user is absent or has no password, and discard the result. T1 asserts the two paths are within an order of magnitude of each other, and binds a mutant to the dummy-verify line. |
| 2 | **A cookie is sent automatically and a bearer token is not**, which is the whole of CSRF. | `SameSite=Strict`, an `Origin` check that fails closed, `Path=/admin`, and the cookie refused outside `/admin`. Four tests, one per property. |
| 3 | **The `Origin` check fails OPEN if written the obvious way** — `if origin != "" && origin != expected { reject }` permits every request that omits the header. | The check is written closed and `TestStateChangingRequestWithNoOriginIsRefused` asserts exactly that case. This is the single most likely bug in the record. |
| 4 | **Rewriting the unauthenticated-route invariant could quietly widen it.** The test is the only guard on that count, and it is being changed at the moment the count changes. | The rewrite enumerates the real chi route table and permits an explicit allow-list of three; a fourth unauthenticated route fails it. It is named in `Enforced-by:`. |
| 5 | **Argon2 is a denial-of-service amplifier**: 64MB and ~50ms per attempt, reachable without credentials. | Rate limited per email, and the router is behind a proxy per ADR-0002's boundary. Recorded rather than fully solved: an attacker spreading attempts across many emails is not throttled by this, and the mitigation for that is the proxy. |
| 6 | **A session outliving a role change or deactivation.** An admin demoted or deactivated keeps a valid session row. | `ResolveSession` re-reads the user row on every request and refuses if `!Active`, exactly as `Authenticate` already does for tokens. Asserted, because the caching version is the tempting one. |
| 7 | **`Secure` on the cookie makes login impossible over plain HTTP**, including on `localhost` for an operator's first run. | Deliberate, and the failure must be legible rather than mysterious: the login handler refuses over plain HTTP with an explicit message naming TLS, instead of setting a cookie the browser silently drops. |
| 8 | **The password column could be read back by a handler that lists users.** The dashboard already renders a user table. | `core.User` carries no password field at all; the hash is loaded only by the verification path. A mutant binds to this. |
| 9 | **A test that asserts "wrong password is refused" passes against an implementation that refuses everything.** | Every negative password test is paired with a positive one in the same run, the same discipline ADR-0002's level-parsing tests used. |
| 10 | **Cookie value logged.** ADR-0002's request logger emits headers it is given; a `Cookie` header carries the session secret. | The logger emits a fixed field set and never headers, which is already true — and `TestRequestLogNeverContainsTheSessionCookie` pins it, because "already true" is not "will stay true". |
| 11 | **Migration `00002` runs against an existing database with rows.** | `ADD COLUMN … NOT NULL DEFAULT ''` is backward compatible by construction; T1 tests the migration against a database seeded with the prior schema, not only against a fresh one. |
| 12 | **Two browser tabs, one logout.** Revoking server-side affects every tab, which is correct but can read as a bug. | Correct behaviour, stated in the README so it is not later "fixed" into a per-tab session. |

## Rollback

No wire contract that an existing client depends on changes, and the API is untouched.

- **The feature** is disabled by not setting a password: an account with `password_hash = ''`
  cannot log in, and `/admin` remains reachable by bearer token exactly as today. That is the
  operational rollback, per-account and needing no redeploy.
- **The schema** is additive — one column with a default and one new table. A previous binary runs
  unchanged against the migrated database: it neither reads the column nor knows the table exists.
  This is the property that makes rolling the binary back safe, and it is why the column is
  `DEFAULT ''` rather than `NOT NULL` with a backfill.
- **Full revert** is `git revert` of this record's commits. The `sessions` table and the column are
  then inert; dropping them is optional and is not required for correctness. No data migration is
  needed in either direction.

## Follow-ups

- Operator to confirm TLS terminates in front of the router before anyone relies on the login: the
  session cookie is `Secure`, so login over plain HTTP is refused by design (Risk 7).
- Operator to decide whether self-service password change in the UI is wanted, now deferred to the
  backlog.
- Revisit the `Origin`-only CSRF defence if the dashboard ever gains a cross-origin caller; the
  synchroniser token rejected above becomes the right answer at that point.
