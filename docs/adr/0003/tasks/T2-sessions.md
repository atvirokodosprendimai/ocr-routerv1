# Task ADR-0003-T2: Revocable, expiring sessions — a token row with a deadline

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (multi-file: migration, new package, identity)
**Owner:** unassigned
**Produces:** `sessions` table, `session.Store`, `Store.Create()`, `Store.Resolve()`, `Store.Revoke()`, `Store.SweepExpired()`, `identity.Login()`, `identity.ResolveSession()`
**Consumes:** `users.password_hash` (T1), `VerifyPassword` (T1) — plus `identity.HashToken` and `store.Repo`, which ADR-0001 already shipped
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the hashed session secret`, `the absolute expiry`, `the live user re-read`, `the collapsed login error`

## Goal

Turn a verified email and password into a revocable server-side session whose secret is never stored
in plaintext, which expires on an absolute deadline, and which stops working the instant its owner
is deactivated.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/store/migrations/00002_password_and_sessions.sql` | edit | the `sessions` table and its `token_hash` index (T1 wrote the `users` half of this file) |
| `internal/session/session.go` | add | the store: create, resolve, revoke, sweep |
| `internal/session/session_test.go` | add | the failing tests |
| `internal/identity/session.go` | add | `Login`, `ResolveSession` — the two calls that turn a credential into a `core.Principal` |
| `internal/identity/session_test.go` | add | the login and resolution tests |
| `internal/core/user.go` | edit | `core.Session` |

⚠ `identity` remains **the only package that decides who a caller is**. `session` owns rows; it
never produces a `core.Principal`. That split is what stops this becoming a second authentication
system.

## Ordered Steps

1. [S1] Write the failing test first: `Login` with a correct email and password returns a secret,
   and `ResolveSession` with that secret returns the admin's principal — before either exists
   (TDD red). [proof: acceptance]
2. [S2] `sessions(id TEXT PRIMARY KEY, user_id TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE,
   created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0)`
   with an index on `token_hash`, which is the only column ever queried by.
3. [S3] `Store.Create` generates 32 bytes from `crypto/rand`, base64url-encodes them as the SECRET,
   stores `identity.HashToken(secret)` — SHA-256, reusing the token path's function — and returns
   the plaintext exactly once. ⚠ **The plaintext is never written anywhere**, so a database read
   cannot impersonate anyone. Same property the `tokens` table already has.
4. [S4] `Store.Resolve(ctx, secret, now)` looks up by hash and refuses a row that is revoked or
   whose `expires_at` is at or before `now`. **Expiry is ABSOLUTE and never extended** — a sliding
   session refreshed on each request never ends for anyone who keeps a tab open, which is how a
   browser on an unlocked laptop becomes a permanent admin credential.
5. [S5] `identity.Login(ctx, email, password, now)` normalises the email, reads the hash, calls
   `VerifyPassword`, and on success creates a session with a 12-hour deadline. ⚠ **Every failure —
   unknown email, empty hash, wrong password, inactive user, non-admin role — returns the SAME
   `core.ErrUnauthorized`**, extending the rule `Authenticate` already follows. Distinguishing them
   turns login into an oracle.
6. [S6] ⚠ **Only an admin may log in.** A client or worker account with a password set (which T4's
   CLI will not do, but a direct database write could) is refused. The role check happens on the
   USER ROW, and the failure collapses into the same error as the rest.
7. [S7] `identity.ResolveSession(ctx, secret, now)` resolves the row and **re-reads the user on
   every call**, refusing if `!Active` or no longer an admin. Caching the principal on the session
   row is the tempting version and it means a deactivated admin keeps working until expiry.
8. [S8] `Store.Revoke(ctx, secret)` marks the row revoked — a real logout, server-side, not a
   cleared cookie. `Store.SweepExpired(ctx, now)` deletes expired and revoked rows and returns the
   count, for the existing reaper tick to call.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/session/... -count=1 -race 2>&1 | tee /tmp/adr3-t2.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr3-t2.out \
  && go test ./internal/identity/... ./internal/store/... -count=1 2>&1 | tee /tmp/adr3-t2r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr3-t2r.out
```

`internal/session` is new and carries the verdict alone; `identity` and `store` run second, and
`identity` is where `Login`/`ResolveSession` are tested — so both halves of this task are covered by
a command neither can satisfy alone. Red at authoring: `internal/session` does not exist.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestCreateThenResolveRoundTrips` | `internal/session/session_test.go` | a created session's secret resolves to its user id — the positive half every negative test below needs | — | S2, S3, S4 |
| `TestSessionsTableHasATokenHashIndex` | `internal/session/session_test.go` | `EXPLAIN QUERY PLAN` for the resolve query reports an index search rather than a `SCAN` — the index is the only reason a per-request session lookup is cheap, and its absence is invisible until the table is large | — | S2 |
| `TestSecretIsNotStoredInPlaintext` | `internal/session/session_test.go` | the returned secret appears **nowhere** in the `sessions` table, checked by scanning every column of every row as text — red if a future change stores it "for debugging" | — | S3 |
| `TestTwoSessionsGetDifferentSecrets` | `internal/session/session_test.go` | two creates for one user yield different secrets and both resolve — the generator is random and sessions are not singletons | — | S3 |
| `TestExpiredSessionIsRefused` | `internal/session/session_test.go` | resolving one second past `expires_at` fails, and one second before succeeds — both sides of the boundary | — | S4 |
| `TestExpiryIsNotExtendedByUse` | `internal/session/session_test.go` | resolving repeatedly up to the deadline does not move it, and the session still dies on time — **red if someone implements a sliding window**, which is the friendlier and wrong behaviour | — | S4 |
| `TestRevokedSessionIsRefused` | `internal/session/session_test.go` | a revoked session stops resolving immediately | — | S8 |
| `TestSweepRemovesExpiredAndRevoked` | `internal/session/session_test.go` | sweep deletes expired and revoked rows, **leaves live ones**, and returns the count — asserts the table SHRANK and that a live session survives, because a sweep that deletes everything passes the first half alone | — | S8 |
| `TestResolveUnknownSecret` | `internal/session/session_test.go` | a random string that was never issued is refused without error-shape difference | — | S4 |
| `TestLoginSucceedsForAdmin` | `internal/identity/session_test.go` | correct email and password return a secret that `ResolveSession` turns into an admin principal | — | S5, S7 |
| `TestLoginFailuresAreIndistinguishable` | `internal/identity/session_test.go` | unknown email, empty password hash, wrong password, inactive user and non-admin role all return exactly `core.ErrUnauthorized` — a table-driven test over all five, because a single negative case covers one path and this is an oracle risk | — | S5, S6 |
| `TestLoginRefusesNonAdmin` | `internal/identity/session_test.go` | a client account with a valid password set directly in the database cannot log in | — | S6 |
| `TestLoginNormalisesEmail` | `internal/identity/session_test.go` | `Admin@Example.COM ` with surrounding space logs in as `admin@example.com` — a form will not normalise and a human will not type carefully | — | S5 |
| `TestSessionDiesWhenUserDeactivated` | `internal/identity/session_test.go` | a live session stops resolving the moment `SetActive(false)` runs, **without waiting for expiry** — red if the principal is cached on the session row | — | S7 |
| `TestSessionDiesWhenUserDemoted` | `internal/identity/session_test.go` | an admin demoted to client stops resolving — the same caching bug, a different symptom | — | S7 |
| `TestSessionExpiryIsTwelveHours` | `internal/identity/session_test.go` | the created row's `expires_at` is `now + 12h` — the number is a decision, so it is asserted rather than left to a constant nobody checks | — | S5 |
| `TestConcurrentResolveIsRaceFree` | `internal/session/session_test.go` | parallel resolves and a concurrent sweep under `-race`, with a final assertion that live sessions survived | — | S4, S8 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the sixteen tests above |
| 2 — something selects it | **nothing selects it yet** — T3 mounts the login route that calls `Login`, and T4 wires `SweepExpired` to the reaper tick. Their mutants discharge this rung; claiming it here would be false. |
| 3 — the caller can discover it | doc comments on every exported identifier; `core.Session` describes the row |
| 4 — it is used | nothing measures sessions; an admin staying logged in is the use |

## Mutation Log

## Invariants

- The session secret exists in plaintext only in the response and the cookie, never in the database.
- `expires_at` is set once at creation and never moved.
- Every resolution re-reads the user row; nothing about the principal is cached.
- Every login failure is `core.ErrUnauthorized`, whatever the cause.
- Only an admin can hold a session.

## Risks

- **`TestSweepRemovesExpiredAndRevoked` passes against a sweep that truncates the table.** The live
  session it also asserts survives is the half that catches it; neither assertion is sufficient
  alone.
- **`TestSessionDiesWhenUserDeactivated` is the one that catches the caching bug**, and the caching
  implementation is faster, obvious, and passes every other test in this file. If anything here is
  cut, this is not it.
- **`TestExpiryIsNotExtendedByUse` needs an injected clock**, not sleeps: the session lives twelve
  hours. The store takes `now` on every call for exactly this reason, the same shape ADR-0002's
  limiter uses.
- **`TestSecretIsNotStoredInPlaintext` must scan EVERY column**, not just `token_hash`. A future
  change adding a `label` or `user_agent` column and putting the secret in it would pass a check
  that only looks where the secret is supposed to be absent.
- **The five-way indistinguishability test is only as good as its fixtures.** Each row must be a
  genuinely different cause — it is easy to write five cases that are all "wrong password" wearing
  different names, which would pass while proving one thing.

## Stop Condition

Stop and ask if the operator wants a session list and "revoke all sessions" in the dashboard. It is
a natural companion to this task and it is a UI decision, not a primitive; the store supports it
either way.

## Out of Scope

- The login route, the cookie, and CSRF — T3.
- Calling `SweepExpired` from the reaper — T4.
- Session listing or bulk revocation in the UI (deferred: `docs/adr/BACKLOG.md`).
- Remembering a device between sessions (permanent: boundary: absolute 12-hour expiry is the
  decision this record took; a remembered device is the sliding session it rejected, wearing a
  different name).

## Verification Log
