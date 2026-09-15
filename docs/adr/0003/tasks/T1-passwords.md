# Task ADR-0003-T1: argon2id passwords on the user row, with no timing oracle

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (multi-file: migration, identity, store)
**Owner:** unassigned
**Produces:** `users.password_hash` (migration `00002`), `identity.HashPassword()`, `identity.VerifyPassword()`, `identity.ErrWeakPassword`, `Repo.SetPasswordHash()`, `Repo.PasswordHashByEmail()`
**Consumes:** none from this record's siblings — `core.User` and `store.Repo` come from ADR-0001 and already exist
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the argon2id parameters in the stored hash`, `the dummy verify`, `the backward-compatible migration`

## Goal

Store and verify administrator passwords with argon2id, and make a failed verification take the
same work whether the account exists, has no password, or has the wrong one — so login cannot be
used to discover which emails are registered.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/store/migrations/00002_password_and_sessions.sql` | add | `ALTER TABLE users ADD COLUMN password_hash TEXT NOT NULL DEFAULT ''` (the `sessions` half is T2's, same file) |
| `internal/identity/password.go` | add | `HashPassword`, `VerifyPassword`, PHC encode/decode, the dummy hash |
| `internal/identity/password_test.go` | add | the failing tests |
| `internal/store/repo.go` | edit | `PasswordHashByEmail` — a read that returns ONLY the hash |
| `internal/store/repo_write.go` | edit | `SetPasswordHash` |
| `internal/store/store_test.go` | edit | the migration-against-existing-data test |
| `go.mod` | edit | `golang.org/x/crypto` direct |

⚠ **`core.User` is deliberately NOT in this table.** The hash never becomes a field on the struct
the dashboard renders; it is read by one query, used, and discarded. A mutant binds to that.

## Ordered Steps

1. [S1] Write the failing test first: `VerifyPassword` against a hash produced by `HashPassword`
   returns true, and against a wrong password returns false — before either exists (TDD red).
   [proof: acceptance]
2. [S2] Migration `00002` adds `password_hash TEXT NOT NULL DEFAULT ''` to `users`. The default is
   what makes it backward compatible without a backfill: every existing row is immediately valid,
   and an empty hash means "cannot log in with a form", which is the correct state for every client
   and worker account.
3. [S3] `HashPassword(plain string) (string, error)` returns the standard PHC string
   `$argon2id$v=19$m=65536,t=3,p=4$<b64 salt>$<b64 hash>`, with 16 random salt bytes from
   `crypto/rand` and a 32-byte key. ⚠ The PARAMETERS ARE STORED WITH THE HASH so raising the cost
   later does not invalidate existing rows — verification reads them back out of the string.
4. [S4] `HashPassword` rejects a password shorter than 12 runes with `ErrWeakPassword`. Length only:
   composition rules produce `Password1!` and a sticky note. Counted in RUNES, not bytes, so a
   passphrase in any script is measured the way its author would count it.
5. [S5] `VerifyPassword(encoded, plain string) bool` decodes the PHC string, recomputes with the
   stored parameters, and compares with `subtle.ConstantTimeCompare`. A malformed or empty `encoded`
   returns false — never an error the caller might forget to check.
6. [S6] ⚠ **The dummy verify.** `VerifyPassword` called with an empty `encoded` still runs a full
   argon2 derivation against a fixed dummy hash and discards the result. Without it, "no such
   account" returns in microseconds where a real check takes tens of milliseconds, and login becomes
   an account-enumeration oracle that no amount of identical error messages can hide.
7. [S7] `Repo.PasswordHashByEmail(ctx, email) (userID, hash string, err error)` — a purpose-built
   read returning only what verification needs. `Repo.SetPasswordHash(ctx, userID, hash)` writes it
   through the write handle.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/identity/... -count=1 -race 2>&1 | tee /tmp/adr3-t1.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr3-t1.out \
  && go test ./internal/store/... ./internal/core/... -count=1 2>&1 | tee /tmp/adr3-t1r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr3-t1r.out
```

`internal/identity` carries the verdict alone; `store` and `core` run second as the regression for
the migration. Red at authoring: `password.go` does not exist, so the package does not compile and
`^FAIL` matches.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestHashThenVerifyRoundTrips` | `internal/identity/password_test.go` | a password hashed then verified returns true — the positive half, without which every negative test below is satisfied by a function that refuses everything | — | S3, S5 |
| `TestWrongPasswordIsRefused` | `internal/identity/password_test.go` | a different password against the same hash returns false | — | S5 |
| `TestSamePasswordHashesDifferently` | `internal/identity/password_test.go` | hashing one password twice yields two different strings — the salt is random and per-hash, so two admins with the same password are not visibly identical in the database | — | S3 |
| `TestHashIsPHCFormatted` | `internal/identity/password_test.go` | the output parses as `$argon2id$v=19$m=…,t=…,p=…$salt$hash` with the configured parameters present | — | S3 |
| `TestVerifyUsesTheParametersInTheHash` | `internal/identity/password_test.go` | a hash hand-built with DIFFERENT parameters than the current defaults still verifies — this is what makes raising the cost later a non-breaking change, and it is red if verification uses package constants instead of the stored values | — | S3, S5 |
| `TestShortPasswordIsRejected` | `internal/identity/password_test.go` | 11 runes returns `ErrWeakPassword` and 12 is accepted — both bounds, so an off-by-one is visible | — | S4 |
| `TestLengthIsCountedInRunes` | `internal/identity/password_test.go` | a 12-rune passphrase of multi-byte characters is accepted, where a byte count would reject it | — | S4 |
| `TestEmptyOrMalformedHashVerifiesFalse` | `internal/identity/password_test.go` | `""`, `"garbage"`, a truncated PHC string and one with a bad base64 segment all return false rather than panicking or erroring | — | S5, S6 |
| `TestVerifyAgainstEmptyHashStillCostsTime` | `internal/identity/password_test.go` | verifying against `""` takes at least a third of the time a real verification takes — **the anti-enumeration property**, measured rather than asserted by reading the source | — | S6 |
| `TestMigrationAddsColumnToAnExistingDatabase` | `internal/store/store_test.go` | a database created at schema `00001` WITH USER ROWS migrates to `00002` and every row keeps its data with an empty `password_hash` — red if the column is added `NOT NULL` without a default | — | S2 |
| `TestSetAndReadPasswordHash` | `internal/identity/password_test.go` | a hash written by `SetPasswordHash` comes back from `PasswordHashByEmail` with the user's id | — | S7 |
| `TestPasswordHashByEmailIsCaseInsensitive` | `internal/identity/password_test.go` | `Admin@Example.com` finds the row stored as `admin@example.com` — emails are normalised on write and a login form will not be | — | S7 |
| `TestPasswordHashByEmailOnUnknownEmail` | `internal/identity/password_test.go` | an unknown email returns `core.ErrNotFound` and an EMPTY hash, never a partially populated result a caller might verify against | — | S7 |
| `TestUserStructHasNoPasswordField` | `internal/identity/password_test.go` | reflection over `core.User` finds no field whose name contains "password" — the hash cannot reach a handler that renders a user table, because there is nowhere on the struct for it to sit | — | S7 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the fourteen tests above |
| 2 — something selects it | **nothing selects it yet** — no caller until T3's login handler and T4's CLI. Recorded rather than claimed; their mutants discharge this rung. |
| 3 — the caller can discover it | doc comments on every exported identifier; `ErrWeakPassword` is a sentinel a caller can match on rather than a string |
| 4 — it is used | nothing measures password use; the operator logging in is the use, and that is not observable from here |

## Mutation Log

## Invariants

- `password_hash` is never a field on `core.User`.
- The stored string carries its own parameters; verification never reads package constants.
- Verification costs real time even when there is nothing to verify against.
- An empty `password_hash` means the account cannot log in with a form, and is the default.
- Comparison of derived keys is constant-time.

## Risks

- **`TestVerifyAgainstEmptyHashStillCostsTime` is a timing test and timing tests flake.** It is
  written as a ratio against a measured real verification in the same run — not an absolute
  duration — and the threshold is deliberately loose (a third), because the defect it catches is
  microseconds-versus-milliseconds, a difference of four orders of magnitude. A tight threshold
  would buy nothing and flake on a loaded CI box.
- **`TestVerifyUsesTheParametersInTheHash` needs a hash built with non-default parameters**, which
  means constructing one by hand in the test rather than calling `HashPassword`. If it calls
  `HashPassword` it proves nothing, because the defaults would match either way.
- **argon2 at 64MB × parallelism 4 is memory-hungry in a test binary** running packages in
  parallel. Tests that only need a round trip use a reduced-cost parameter set exposed for that
  purpose; the two tests that assert the DEFAULTS use the real ones.
- **A migration test against a fresh database proves nothing about an existing one.** The whole
  risk of `ADD COLUMN` is the rows already there, so the test seeds rows at `00001` first.
- **`ErrWeakPassword` could leak into an HTTP response** and tell an attacker the password shape.
  It is returned only by `HashPassword`, which runs when an operator SETS a password, never during
  verification — T3 must not surface it on the login path, and that is stated there.

## Stop Condition

Stop and ask if the operator wants a password policy beyond length — a deny-list of common
passwords, or a strength estimator. Both are defensible, both need a data file or a dependency, and
neither is implied by "email and password for admins".

## Out of Scope

- Sessions and cookies — T2 and T3.
- The login route and its rate limit — T3.
- The CLI that sets a password — T4.
- Password reset by email (permanent: boundary: this system has no email channel).
- Passwords for client and worker accounts (permanent: boundary: they have no UI to log into).

## Verification Log
