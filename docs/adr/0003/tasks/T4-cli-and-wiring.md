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
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr3-t4r.out \
  && adr-debt docs/adr 2>&1 | tee /tmp/adr3-t4d.out \
  && ! grep -q "broken pointers" /tmp/adr3-t4d.out || grep -q "0 broken pointers" /tmp/adr3-t4d.out
```

`cmd/router` carries the verdict; the four packages this task wires run second as regression; and
the `adr-debt` segment fails if S9's deferrals point at a file that never received them — the one
step whose failure mode is a document, not a test.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestBootstrapWithPasswordAllowsLogin` | `cmd/router/login_test.go` | `admin bootstrap --email … --password …` produces an account that `identity.Login` accepts | — | S2 |
| `TestBootstrapWithoutPasswordLeavesLoginDisabled` | `cmd/router/login_test.go` | omitting the flag in a non-interactive run leaves `password_hash` empty and login refused — the account still works by token, which is today's behaviour preserved | — | S2 |
| `TestSetPasswordChangesTheCredential` | `cmd/router/login_test.go` | after `set-password`, the OLD password is refused and the NEW one accepted — both halves, since "new one works" alone passes against a no-op that never rotated anything | — | S3 |
| `TestSetPasswordRefusesUnknownEmail` | `cmd/router/login_test.go` | a non-existent email exits non-zero and writes nothing | — | S3 |
| `TestSetPasswordRefusesNonAdmin` | `cmd/router/login_test.go` | a client account is refused rather than given a password it could never use | — | S3 |
| `TestSetPasswordRejectsShortPassword` | `cmd/router/login_test.go` | an 11-character password exits non-zero naming the minimum, and does not write a partial state | — | S3 |
| `TestPromptRefusesMismatchedConfirmation` | `cmd/router/login_test.go` | two different passwords typed at the prompt exit non-zero, write nothing, and do not loop — a mistyped invisible password that silently became the real one would lock the operator out with no way to discover why | — | S5 |
| `TestPromptAcceptsMatchingConfirmation` | `cmd/router/login_test.go` | the same password twice is accepted and stored — without it the test above is satisfied by a prompt that refuses everything | — | S5 |
| `TestPasswordFlagIsNotEchoedToStdout` | `cmd/router/login_test.go` | neither the plaintext password nor its hash appears in the command's captured output — a bootstrap that prints the password puts it in the operator's scrollback and CI log | — | S4 |
| `TestBinaryAcceptsALoginEndToEnd` | `cmd/router/login_test.go` | through `buildApp`'s own handler over TLS: `POST /admin/login` → cookie → `GET /admin` returns 200 → `POST /admin/logout` → the same cookie now 401s. **Red if `wire.go` never hands the store to the API**, which every test in T1–T3 survives | — | S6 |
| `TestSessionSweepRunsOnTheReaperTick` | `cmd/router/login_test.go` | with an expired session in the table and the reaper running, the row **disappears** — asserts the table SHRANK, not that a sweep function was called, and red if the `SweepExpired` line is deleted from `StartReaper` | — | S7 |
| `TestCLIExposesThePasswordFlags` | `cmd/router/login_test.go` | `--password` on `bootstrap` and the `set-password` subcommand appear on the real command tree — a subcommand in the code but not on the CLI is unreachable by an operator | — | S2, S3 |
| `TestDeferredItemsReachedTheBacklog` | `cmd/router/login_test.go` | each of the four strings this record defers appears in `docs/adr/BACKLOG.md` — the deferral is filed, not merely pointed at | — | S9 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the eleven tests above |
| 2 — something selects it | `wire.go` hands the store to `httpapi.Deps` (`TestBinaryAcceptsALoginEndToEnd`) and calls `SweepExpired` on the tick (`TestSessionSweepRunsOnTheReaperTick`). Both go red with one line deleted, and no test in T1–T3 can see either. |
| 3 — the caller can discover it | `--help` lists `set-password` and `--password` (`TestCLIExposesThePasswordFlags`); the README carries first-login instructions |
| 4 — it is used | human sign-off: a real browser, a real login, the dashboard rendering, a real logout — the verification T10 asked for and could not perform |

## Mutation Log

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
