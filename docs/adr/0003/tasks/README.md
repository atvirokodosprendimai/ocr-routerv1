# ADR-0003 tasks

Derived index — the task files are the source of truth. Execute in the order below.

| Task | Goal | Status | Depends-on | Acceptance (first command) |
|------|------|--------|------------|----------------------------|
| T1 | argon2id passwords on the user row, with no timing oracle | done | none | `go test ./internal/identity/... -race` |
| T2 | Revocable, expiring sessions — a token row with a deadline | done | T1 | `go test ./internal/session/... -race` |
| T3 | The login page, the cookie, and a CSRF check that fails closed | pending | T1, T2 | `go test ./internal/web/... -race` |
| T4 | Set passwords from the CLI, sweep sessions on the existing tick | pending | T1, T2, T3 | `go test ./cmd/router/... -race` |

## Order

Strictly sequential — unlike ADR-0002, there is no parallel pair here. T2 verifies the password T1
stores, T3 serves the session T2 issues, and T4 wires all three into the binary.

    T1 ── T2 ── T3 ── T4

T1 and T2 share one migration file (`00002_password_and_sessions.sql`): T1 writes the `users`
column, T2 adds the `sessions` table. One migration rather than two because they ship together and
a half-applied pair has no useful meaning.

## What each task is actually for

- **T1** is the password primitive, and its least obvious requirement is the one that matters most:
  verification must cost real time even when there is nothing to verify against. Without the dummy
  hash, a missing account returns in microseconds where a real check takes tens of milliseconds, and
  login becomes an account-enumeration oracle that identical error messages cannot hide.
- **T2** is a session: a token row with a deadline. Two properties carry it — the secret is stored
  only as a SHA-256, and the expiry is absolute and never extended. The sliding-window version is
  friendlier and turns a laptop left open into a permanent admin credential.
- **T3** is the part a human touches, and it holds the single most likely bug in this record: the
  `Origin` check written the natural way (`if origin != "" && origin != want`) permits every
  request that omits the header, which is every request an attacker writes by hand. One test exists
  for exactly that spelling.
- **T4** is the wiring and the CLI. Every unit test in T1–T3 passes with `wire.go` never handing the
  session store to the API — the symptom would be a login that authenticates and then 401s on the
  next request — which is why this task's mutants bind there.

## The invariant this record changes

ADR-0001 made `/healthz` the only unauthenticated route in the process, and
`cmd/router/monitoring_test.go` asserts it. T3 **rewrites** that test to permit exactly three:
`/healthz`, `GET /admin/login`, `POST /admin/login`. Rewritten and not deleted — it is the only
guard on that count, and it is being changed at the moment the count changes.

## Convention

Status here is derived from each task's `## Verification Log`: a task may be marked `done` only once
`adr-verify` has recorded an exit-0 entry whose digest matches the task's current Acceptance fence,
plus at least one killed mutant bound to the same digest. Do not hand-edit either log.
