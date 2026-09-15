# Task ADR-0003-T4: Set passwords from the CLI, sweep sessions on the existing tick

**Depends-on:** T1, T2, T3
**Covers:** none — no spec
**Estimated scope:** M (cmd, wiring, docs)
**Owner:** unassigned
**Produces:** `admin bootstrap --password`, `admin set-password`, `session.Store` in the composition root, the session sweep on the reaper tick, README login documentation
**Consumes:** `HashPassword` (T1), `session.Store` (T2), `SweepExpired` (T2), the cookie fallback (T3)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the session sweep on the tick`, `the terminal echo suppression`, `the store wired into the API`

## Goal

Give the operator a way to set the first password and change any later one, wire the session store
into the binary, and let expired sessions be swept by the tick that already exists.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `cmd/router/main.go` | edit | `--password` on `admin bootstrap`, the new `admin set-password` subcommand, the no-echo prompt |
| `cmd/router/wire.go` | edit | **build `session.Store`, pass it into `httpapi.Deps` and `web.Deps`, and call `SweepExpired` on the reaper tick** |
| `cmd/router/login_test.go` | add | the composition-root tests |
| `README.md` | edit | first-login instructions, the TLS requirement, the 12-hour expiry, and why logout affects every tab |
| `docs/adr/BACKLOG.md` | edit | file the four items this record defers |

`wire.go` is the selecting file: every test in T1–T3 passes with the store never handed to the API,
and the symptom would be a login page that authenticates and then 401s on the next request.

## Ordered Steps

1. [S1] Write the failing test first: `admin set-password` on a bootstrapped admin makes
   `identity.Login` succeed with that password, before the subcommand exists (TDD red).
   [proof: acceptance]
2. [S2] `admin bootstrap --email … --password …` sets the password alongside minting the first
   token. Without `--password` it PROMPTS.
3. [S3] `admin set-password --email …` changes an existing account's password. It refuses an email
   that does not exist, and refuses a non-admin account — the password would be unusable, since T2
   only lets admins log in, and writing one silently would be a lie.
4. [S4] ⚠ **The prompt reads with terminal echo disabled** (`golang.org/x/term`). A password given
   with `--password` is in the shell history and in `ps` output for every user on the box; the flag
   exists for scripted provisioning and the prompt is the default for a human.
5. [S5] The password is confirmed by typing it twice at the prompt, and the two must match. A
   mistyped invisible password that silently becomes the real one locks the operator out of the
   dashboard with no way to discover why.
6. [S6] `wire.go` builds `session.Store` over the same `*store.DB` and passes it to `httpapi.Deps`
   and `web.Deps`.
7. [S7] `App.StartReaper` calls `sessions.SweepExpired(ctx, time.Now())` on each tick, beside
   `Router.Reap` and `Limiter.EvictIdle`. No second goroutine — the tick already exists.
8. [S8] README: how to set the first password, that TLS is required for login, that a session lasts
   12 hours and is not extended by use, and that logout ends every tab because revocation is
   server-side.
   [proof: human: a reader follows the first-login instructions against a fresh database and reaches the dashboard in a browser]
9. [S9] File this record's deferred items into `docs/adr/BACKLOG.md`: self-service password change,
   two-factor authentication, session listing and bulk revocation, and remembering the requested URL
   across a login redirect. ⚠ A deferral is not filed by pointing at a file — the entry is written
   at the destination, naming this ADR, in the same commit.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./cmd/router/... -count=1 -race 2>&1 | tee /tmp/adr3-t4.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr3-t4.out \
  && go test ./internal/identity/... ./internal/session/... ./internal/web/... ./internal/httpapi/... -count=1 2>&1 | tee /tmp/adr3-t4r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr3-t4r.out
```

`cmd/router` carries the verdict; the four packages this task wires run second as regression.

⚠ **S9's deferrals are checked by `TestDeferredItemsReachedTheBacklog`, inside the first segment,
not by an `adr-debt` call in the fence.** The `adr-debt` version was written first and failed on its
own tooling: the fence runs in a bare shell where that script is not on `PATH`, so the segment
errored on a missing tee'd file rather than on anything about the backlog. Its `&& … || …` also had
the wrong precedence, which would have made it pass whatever it found. A Go test reading
`BACKLOG.md` directly is hermetic, needs nothing on `PATH`, and fails for the reason it claims to.

## Added during execution: `--insecure-cookies`

⚠ **The operator hit a wall this task's plan created and none of its tests could see.** ADR-0003
made the session cookie `Secure` and had T3 refuse plain HTTP outright — correct for production, and
it left **no way at all** to use the dashboard on `http://localhost`. The login page could only tell
the operator to go and get TLS. That is not a deployment nicety; it made the first run of the whole
feature impossible, and it was found by a human opening the page, not by any test here.

`--insecure-cookies` (default **false**) drops `Secure` and nothing else. `HttpOnly`,
`SameSite=Strict` and the `/admin` path scope are untouched, because the transport is a separate
question from the cookie's reach and its CSRF properties — a test asserts each of those three
survives the flag. The binary prints a warning on stdout on every boot while it is set: a dangerous
flag documented only in `--help` is one somebody turns on for an afternoon and leaves on.

The lesson worth keeping: a security default that has no development escape hatch is not a strict
default, it is a broken feature, and **every test in T1–T4 passed while the feature could not be
used at all on a laptop.** Reachability rung 4 — "a human actually uses it" — is the only rung that
would have caught it, which is why S8's sign-off is human-observed.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestInsecureCookiesAllowsPlainHTTPLogin` | `internal/web/login_test.go` | with the flag set, a plain-HTTP sign-in returns 303 and a cookie **without** `Secure` — while `HttpOnly`, `SameSite=Strict` and `Path=/admin` all survive, so the flag relaxes the transport and nothing else | — | S6 |
| `TestSecureIsTheDefault` | `internal/web/login_test.go` | with the flag unset the cookie IS `Secure` — without this, the test above is satisfied by a build that never sets it, and the dangerous behaviour would be what an operator gets by not asking | — | S6 |
| `TestBootstrapWithPasswordAllowsLogin` | `cmd/router/login_test.go` | `admin bootstrap --email … --password …` produces an account that `identity.Login` accepts | — | S2 |
| `TestBootstrapWithoutPasswordLeavesLoginDisabled` | `cmd/router/login_test.go` | omitting the flag in a non-interactive run leaves `password_hash` empty and login refused — the account still works by token, which is today's behaviour preserved | — | S2 |
| `TestSetPasswordChangesTheCredential` | `cmd/router/login_test.go` | after `set-password`, the OLD password is refused and the NEW one accepted — both halves, since "new one works" alone passes against a no-op that never rotated anything | — | S3 |
| `TestSetPasswordRefusesUnknownEmail` | `cmd/router/login_test.go` | a non-existent email exits non-zero and writes nothing | — | S3 |
| `TestSetPasswordRefusesNonAdmin` | `cmd/router/login_test.go` | a client account is refused rather than given a password it could never use | — | S3 |
| `TestSetPasswordRejectsShortPassword` | `cmd/router/login_test.go` | an 11-character password exits non-zero naming the minimum, and does not write a partial state | — | S3 |
| `TestPromptRefusesMismatchedConfirmation` | `cmd/router/login_test.go` | two different passwords typed at the prompt exit non-zero, write nothing, and do not loop — a mistyped invisible password that silently became the real one would lock the operator out with no way to discover why | — | S5 |
| `TestPromptAcceptsMatchingConfirmation` | `cmd/router/login_test.go` | the same password twice is accepted and stored — without it the test above is satisfied by a prompt that refuses everything | — | S5 |
| `TestPasswordIsNotEchoedToStdout` | `cmd/router/login_test.go` | neither the plaintext password nor its hash appears in the command's captured output — a bootstrap that prints the password puts it in the operator's scrollback and CI log | — | S4 |
| `TestBinaryAcceptsALoginEndToEnd` | `cmd/router/login_test.go` | through `buildApp`'s own handler over TLS: `POST /admin/login` → cookie → `GET /admin` returns 200 → `POST /admin/logout` → the same cookie now 401s. **Red if `wire.go` never hands the store to the API**, which every test in T1–T3 survives | — | S6 |
| `TestSessionSweepRunsOnTheReaperTick` | `cmd/router/login_test.go` | with an expired session in the table and the reaper running, the row **disappears** — asserts the table SHRANK, not that a sweep function was called, and red if the `SweepExpired` line is deleted from `StartReaper` | — | S7 |
| `TestCLIExposesThePasswordFlags` | `cmd/router/login_test.go` | `--password` on `bootstrap` and the `set-password` subcommand appear on the real command tree — a subcommand in the code but not on the CLI is unreachable by an operator | — | S2, S3 |
| `TestDashboardRequestsAreLogged` | `cmd/router/login_test.go` | a failed sign-in and a login-page load both produce `"route":"/admin/login"` request lines — **added after driving the real binary revealed the dashboard produced no log lines at all**, because its subtree is mounted on the parent mux and never passed through httpapi's router. The failed login is the security-relevant one, and it was exactly what ADR-0002 put the logger outermost to capture | — | S6 |
| `TestDeferredItemsReachedTheBacklog` | `cmd/router/login_test.go` | each of the four strings this record defers appears in `docs/adr/BACKLOG.md` — the deferral is filed, not merely pointed at | — | S9 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the eleven tests above |
| 2 — something selects it | `wire.go` hands the store to `httpapi.Deps` (`TestBinaryAcceptsALoginEndToEnd`) and calls `SweepExpired` on the tick (`TestSessionSweepRunsOnTheReaperTick`). Both go red with one line deleted, and no test in T1–T3 can see either. |
| 3 — the caller can discover it | `--help` lists `set-password` and `--password` (`TestCLIExposesThePasswordFlags`); the README carries first-login instructions |
| 4 — it is used | human sign-off: a real browser, a real login, the dashboard rendering, a real logout — the verification T10 asked for and could not perform |

## Mutation Log

- 2026-09-15 · 6b4c6cf* · mutant killed · exit 1 · `cmd/router/wire.go` · expired and revoked sessions are never deleted, so the table grows one row per login for the life of the deployment · acceptance-sha256:f5822feb58c7be1be37e4fcd63f8cf5dd58b94a267cdff79877dc328ee8f5ee8 · covers:the session sweep on the tick
- 2026-09-15 · 6b4c6cf* · mutant killed · exit 1 · `cmd/router/wire.go` · the session store is built and never attached to identity, so the login page authenticates nobody and the dashboard stays unreachable from a browser — which is the entire point of ADR-0003 · acceptance-sha256:f5822feb58c7be1be37e4fcd63f8cf5dd58b94a267cdff79877dc328ee8f5ee8 · covers:the store wired into the API
- 2026-09-15 · 6b4c6cf* · mutant killed · exit 1 · `cmd/router/password.go` · the confirmation is not compared, so a mistyped invisible password silently becomes the real one and the operator discovers it by being locked out of the dashboard · acceptance-sha256:f5822feb58c7be1be37e4fcd63f8cf5dd58b94a267cdff79877dc328ee8f5ee8 · covers:the terminal echo suppression

## Invariants

- A password is never printed, echoed, or logged.
- Only an admin account can be given a password.
- Session sweeping runs on the existing tick; no second goroutine is started.
- Every item this record defers exists in `docs/adr/BACKLOG.md`.

## Risks

- **`TestSessionSweepRunsOnTheReaperTick` is the one that catches a leaking table**, and the
  vacuous version — asserting `SweepExpired` was called — proves nothing. It must observe the row
  count drop, which means inserting an expired row first.
- **`TestPasswordFlagIsNotEchoedToStdout` must capture the REAL command output**, not inspect
  source. A `fmt.Printf` added later for debugging is exactly what it is guarding against.
- **The no-echo prompt cannot be tested without a TTY**, and CI has none. The prompt path is
  exercised by injecting the reader; the `term.ReadPassword` call itself is covered by the human
  sign-off rather than claimed as tested — recorded honestly, because a test that fakes the
  terminal proves nothing about the terminal.
- **`TestBinaryAcceptsALoginEndToEnd` needs TLS**, because S4 refuses login over plain HTTP. Use
  `httptest.NewTLSServer`; a plain server would make the test fail for the right reason and the
  wrong one, and the fixture would be adjusted until it passed, probably by weakening S4.
- **The `adr-debt` segment of the fence is easy to write so it always passes.** It must fail when
  the backlog entries are missing — check the run against a deliberately unfiled entry once before
  trusting it.
- **Confirming the password twice makes the prompt path branchier**, and a mismatch must exit
  non-zero rather than retry forever in a non-interactive context.

## Stop Condition

Stop and ask before changing the 12-hour session lifetime. It is a security decision the operator
owns, and the argument for it (one working day, absolute, not extended by use) is in ADR-0003
§Decision 2 rather than in this task.

## Out of Scope

- Self-service password change in the UI (deferred: `docs/adr/BACKLOG.md`).
- Two-factor authentication (deferred: `docs/adr/BACKLOG.md`).
- Session listing and bulk revocation (deferred: `docs/adr/BACKLOG.md`).
- Remembering the requested URL across a login redirect (deferred: `docs/adr/BACKLOG.md`).
- Password reset by email (permanent: boundary: this system has no email channel, and
  `set-password` is the recovery path for an operator with shell access).

## Verification Log
- 2026-09-15 · 6b4c6cf* · exit 2 · `set -o pipefail …` · acceptance-sha256:e288dbd66e65b3933e5ff02d376d572aca4be82804c8bc2ca63e6a808c548db6 · ms:20005
  ```
  --- last 10 line(s) of stdout (of 292 after folding 292 raw)
  {"time":"2026-09-15T10:51:20.821109+03:00","level":"INFO","msg":"transition","job":{"id":"01a0a40c-8bdb-7090-a1e4-23704dd2e22d","user_id":"01a0a40c-8bcb-74f5-b6e1-54dc563ff0c6","label":"ocr","params":[]},"from":"done","to":"delivered","actor":"client","attempt":0,"stage":0,"in_state":817815000}
  {"time":"2026-09-15T10:51:20.821314+03:00","level":"INFO","msg":"request","method":"GET","route":"/files/{id}","status":200,"duration":5117834,"user_id":"01a0a40c-8bcb-74f5-b6e1-54dc563ff0c6","token_id":"01a0a40c-8bce-7b0c-95e0-2a42bc10d90f","role":"client"}
  {"time":"2026-09-15T10:51:20.821592+03:00","level":"INFO","msg":"request","method":"GET","route":"/sse","status":200,"duration":32752917,"user_id":"01a0a40c-8bd0-7d75-83b8-298b0c202915","token_id":"01a0a40c-8bd3-77e3-b604-eaf4db6a30ba","role":"worker"}
  {"time":"2026-09-15T10:51:20.898837+03:00","level":"INFO","msg":"transition","job":{"id":"01a0a40c-8c38-7537-aab6-28bd6664973d","user_id":"01a0a40c-8c2c-744d-84a1-bab4fe1a248d","label":"ocr","params":[]},"from":"","to":"queued","actor":"client","attempt":0,"stage":0,"in_state":0}
  {"time":"2026-09-15T10:51:20.898934+03:00","level":"INFO","msg":"request","method":"POST","route":"/upload","status":201,"duration":13829834,"user_id":"01a0a40c-8c2c-744d-84a1-bab4fe1a248d","token_id":"01a0a40c-8c2e-7dbf-a157-a14fbaafc694","role":"client"}
  {"time":"2026-09-15T10:51:20.901638+03:00","level":"INFO","msg":"request","method":"GET","route":"/sse","status":200,"duration":19140583,"user_id":"01a0a40c-8c2f-78dd-9ea9-90cea2c3f895","token_id":"01a0a40c-8c30-7e09-aeb5-eb9da4bc33d0","role":"worker"}
  {"time":"2026-09-15T10:51:21.02863+03:00","level":"INFO","msg":"request","method":"POST","route":"/upload","status":404,"duration":3401708,"user_id":"01a0a40c-8cbb-70ab-9471-df5922129fe1","token_id":"01a0a40c-8cbd-77d7-afee-54d4fed0ed7c","role":"client"}
  FAIL
  FAIL	github.com/atvirokodosprendimai/ocr-router/cmd/router	15.747s
  FAIL
  --- last 1 line(s) of stderr
  grep: /tmp/adr3-t4d.out: No such file or directory
  ```
- 2026-09-15 · 6b4c6cf* · exit 1 · `set -o pipefail …` · acceptance-sha256:f5822feb58c7be1be37e4fcd63f8cf5dd58b94a267cdff79877dc328ee8f5ee8 · ms:20582
  ```
  --- last 10 line(s) of stdout (of 292 after folding 292 raw)
  {"time":"2026-09-15T10:55:26.507926+03:00","level":"INFO","msg":"transition","job":{"id":"01a0a410-4b90-7b3b-bb84-0124eec60e6d","user_id":"01a0a410-4b84-7bb8-8f45-c58b773a6749","label":"ocr","params":[]},"from":"done","to":"delivered","actor":"client","attempt":0,"stage":0,"in_state":504804000}
  {"time":"2026-09-15T10:55:26.508112+03:00","level":"INFO","msg":"request","method":"GET","route":"/files/{id}","status":200,"duration":4935084,"user_id":"01a0a410-4b84-7bb8-8f45-c58b773a6749","token_id":"01a0a410-4b87-73ad-8bac-54d0c6530f8a","role":"client"}
  {"time":"2026-09-15T10:55:26.508397+03:00","level":"INFO","msg":"request","method":"GET","route":"/sse","status":200,"duration":33735583,"user_id":"01a0a410-4b87-7e48-a1d5-d82458d1c1b0","token_id":"01a0a410-4b89-7484-b119-0dd5db9c7765","role":"worker"}
  {"time":"2026-09-15T10:55:26.584325+03:00","level":"INFO","msg":"transition","job":{"id":"01a0a410-4bee-79f6-b060-245b79427d19","user_id":"01a0a410-4be2-7e25-af47-047dacf22790","label":"ocr","params":[]},"from":"","to":"queued","actor":"client","attempt":0,"stage":0,"in_state":0}
  {"time":"2026-09-15T10:55:26.584414+03:00","level":"INFO","msg":"request","method":"POST","route":"/upload","status":201,"duration":12976500,"user_id":"01a0a410-4be2-7e25-af47-047dacf22790","token_id":"01a0a410-4be5-76c2-b327-f72775940d2b","role":"client"}
  {"time":"2026-09-15T10:55:26.587166+03:00","level":"INFO","msg":"request","method":"GET","route":"/sse","status":200,"duration":18340917,"user_id":"01a0a410-4be6-71f0-bf00-7cd89ddfd2a1","token_id":"01a0a410-4be7-777d-b364-c7d492f50671","role":"worker"}
  {"time":"2026-09-15T10:55:26.713744+03:00","level":"INFO","msg":"request","method":"POST","route":"/upload","status":404,"duration":3519584,"user_id":"01a0a410-4c6f-7aae-8f0a-8fd71317902a","token_id":"01a0a410-4c72-775a-9adc-ccf902a6f476","role":"client"}
  FAIL
  FAIL	github.com/atvirokodosprendimai/ocr-router/cmd/router	16.402s
  FAIL
  ```
- 2026-09-15 · 6b4c6cf* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5822feb58c7be1be37e4fcd63f8cf5dd58b94a267cdff79877dc328ee8f5ee8 · ms:32926
- 2026-09-15 · 6b4c6cf* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5822feb58c7be1be37e4fcd63f8cf5dd58b94a267cdff79877dc328ee8f5ee8 · ms:29974
- 2026-09-15 · 6b4c6cf* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5822feb58c7be1be37e4fcd63f8cf5dd58b94a267cdff79877dc328ee8f5ee8 · ms:29335
- 2026-09-15 · 6b4c6cf* · exit 0 · `set -o pipefail …` · acceptance-sha256:f5822feb58c7be1be37e4fcd63f8cf5dd58b94a267cdff79877dc328ee8f5ee8 · ms:29923
- 2026-09-15 · human-observed · S8 README: followed the first-run instructions against a fresh database with --insecure-cookies on 2026-09-15 — bootstrap set a password, the login page rendered a form over plain HTTP, sign-in returned 303 with a non-Secure HttpOnly SameSite=Strict cookie, /admin rendered, the same cookie was refused on /upload and /claim, a POST with no Origin and one from evil.example both got 403, a same-origin POST got 200, and after logout the replayed cookie got 401. The boot warning printed. Every documented flag name checked against router --help
