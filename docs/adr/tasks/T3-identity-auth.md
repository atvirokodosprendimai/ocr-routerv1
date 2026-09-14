# Task ADR-0001-T3: Create users by email, mint hashed bearer tokens, and authenticate every request

**Depends-on:** T2
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `identity.Service.Authenticate(ctx, header) (core.Principal, error)`, `identity.Service.CreateUser`, `identity.Service.MintToken`, `identity.Service.RevokeToken`
**Consumes:** `store.Repo` (T2), `core.User`, `core.Token`, `core.Role`, `core.Err*` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the constant-time comparison`, `the role check`

## Goal

Let an admin create customers by email and mint bearer tokens, and resolve any
`Authorization` header to a principal — or refuse it — with the token stored only as a
hash.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/identity/service.go` | add | user creation, token minting, revocation, authentication |
| `internal/identity/token.go` | add | generation, `sha256` hashing, constant-time compare |
| `internal/identity/service_test.go` | add | the failing tests |
| `internal/identity/token_test.go` | add | token format and hashing tests |

`identity.Service.Authenticate` is selected by the auth middleware in T7; T7's Affected
Files carries that middleware, and deleting the call there makes every authorization test
in T7 fail open — which is the mutation T7 records.

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write
   `service_test.go` asserting that an unknown token is refused and that a client token
   cannot create a user, before any implementation (TDD red). [proof: acceptance]
2. [S2] `GenerateToken()` returns a URL-safe string from `crypto/rand` (32 bytes) with a
   role-marking prefix (`ocr_c_`, `ocr_w_`, `ocr_a_`) so an operator can tell a leaked
   token's blast radius by looking at it. The **prefix is a label, never the authority** —
   the role is read from the database row.
3. [S3] Store only `sha256(token)` hex. The plaintext is returned exactly once, from
   `MintToken`, and never logged.
4. [S4] `Authenticate` parses `Authorization: Bearer <t>`, hashes, looks up by hash, and
   compares with `crypto/subtle.ConstantTimeCompare`. An unknown token, a revoked token and
   an inactive user are **all** `core.ErrUnauthorized` — one error, so the response cannot
   distinguish them and become an enumeration oracle.
5. [S5] `CreateUser(actor, email, …)` returns `core.ErrForbidden` unless
   `actor.Role == core.RoleAdmin`. Email is normalised (trimmed, lower-cased) and unique;
   a duplicate is `core.ErrConflict`, matched from the driver's constraint code via
   `errors.As` on `interface{ Code() int }` — **not** by matching the error string.
6. [S6] Return a `core.Principal{UserID, Role, TokenID}` rather than the `Token` row, so no
   caller can accidentally re-read the hash.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/identity/... -count=1 -race 2>&1 | tee /tmp/adr1-t3.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t3.out \
  && go test ./internal/store/... -count=1 2>&1 | tee /tmp/adr1-t3r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t3r.out
```

The new package runs first and alone, so it can carry the verdict by itself; T2's suite
runs second as regression. Red at authoring: `internal/identity` does not exist.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestAuthenticateUnknownToken` | `internal/identity/service_test.go` | an unknown token is `core.ErrUnauthorized` | — | S4 |
| `TestAuthenticateRevokedToken` | `internal/identity/service_test.go` | a revoked token is refused with the **same** error as an unknown one | — | S4 |
| `TestAuthenticateInactiveUser` | `internal/identity/service_test.go` | a valid token on a deactivated user is refused, with the same error | — | S4 |
| `TestAuthenticateMalformedHeader` | `internal/identity/service_test.go` | missing header, wrong scheme and empty token are each refused, not panicked | — | S4 |
| `TestAuthenticateReturnsRoleFromRow` | `internal/identity/service_test.go` | a token whose **prefix says admin** but whose row says client authenticates as a client — the prefix is a label, not authority | — | S2, S4, S6 |
| `TestCreateUserRequiresAdmin` | `internal/identity/service_test.go` | a client and a worker principal each get `core.ErrForbidden` | — | S5 |
| `TestCreateUserDuplicateEmail` | `internal/identity/service_test.go` | a duplicate email is `core.ErrConflict`, matched by driver code not by string | — | S5 |
| `TestCreateUserNormalisesEmail` | `internal/identity/service_test.go` | ` Foo@Example.COM ` and `foo@example.com` collide | — | S5 |
| `TestMintTokenReturnsPlaintextOnce` | `internal/identity/token_test.go` | the stored row holds only the hash, and no read path returns plaintext | — | S3 |
| `TestTokenHashIsStable` | `internal/identity/token_test.go` | the same token hashes identically across calls, and two tokens do not collide | — | S3 |
| `TestTokenRoleIsIndependentOfUserRole` | `internal/identity/service_test.go` | an ADMIN user holding a CLIENT-scoped token authenticates as a client and cannot create users — added after a SURVIVED mutant showed every other fixture minted tokens whose role equalled their user's, making `tok.Role` and `u.Role` indistinguishable | — | S4, S6 |
| `TestBootstrapCreatesOneAdmin` | `internal/identity/service_test.go` | the first admin is created, its printed token authenticates, and a second bootstrap is refused | — | S5 |
| `TestGenerateTokenShape` | `internal/identity/token_test.go` | tokens are unique, carry their role's prefix, and are long enough to hold 32 bytes of entropy | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the ten tests above |
| 2 — something selects it | T7's `RequireRole` middleware calls `Authenticate` on every route; the mutation recorded on T7 deletes that call and every T7 authorization test goes red |
| 3 — the caller can discover it | the `Authorization: Bearer` contract is documented in ADR §Wiring and surfaced in the dashboard when a token is minted |
| 4 — it is used | every request to the router passes through it; T8's end-to-end test authenticates a real client and a real worker |

## Mutation Log

- 2026-09-15 · a17fe3e* · mutant survived · exit 0 · `internal/identity/service.go` · The role must come from the TOKEN row, not the user row: a user may hold tokens of different scopes, and conflating them silently widens every token to the account's own role. · acceptance-sha256:44304868802f449d70978dad58d78cdb65fcd2361ec05c97bfaec031d6e100cd · covers:the role check
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```
- 2026-09-15 · a17fe3e* · mutant killed · exit 1 · `internal/identity/service.go` · Revocation must actually stop a token; a single 'unknown token is refused' test covers a different code path and would not notice. · acceptance-sha256:44304868802f449d70978dad58d78cdb65fcd2361ec05c97bfaec031d6e100cd
- 2026-09-15 · a17fe3e* · mutant killed · exit 1 · `internal/identity/service.go` · Only an admin creates accounts; without the gate any client token could mint users and tokens. · acceptance-sha256:44304868802f449d70978dad58d78cdb65fcd2361ec05c97bfaec031d6e100cd
- 2026-09-15 · a17fe3e* · mutant killed · exit 1 · `internal/identity/service.go` · The role must come from the TOKEN row, not the user row: a user may hold tokens of different scopes, and conflating them silently widens every token to the account's own role. · acceptance-sha256:44304868802f449d70978dad58d78cdb65fcd2361ec05c97bfaec031d6e100cd · covers:the role check

## Invariants

- A token's plaintext exists only in the `MintToken` return value and the client's config.
- The **role is read from the database row**, never parsed from the token string.
- Unknown, revoked and inactive all produce one indistinguishable error.
- Only `core.RoleAdmin` creates users or mints tokens.

## Risks

- **A test asserting "unknown is refused" passes even if revoked tokens are accepted** —
  the two are different code paths and a single negative test covers only one. Three
  separate tests above, one per cause, is the mitigation; the shared-error assertion is
  what stops them collapsing back into one path.
- **Matching the UNIQUE violation by error string** breaks on a driver upgrade and cannot
  tell one unique index from another. S5 matches the code via `errors.As`.

## Stop Condition

Stop and ask if the operator wants worker tokens to belong to a dedicated operator account
rather than to an ordinary user row — the ADR assumes they do, and a worker sees every
customer's file, so a customer-owned worker token would be a privilege escalation.

## Out of Scope

- HTTP middleware and status-code mapping — T7's.
- The admin UI for minting — T10's; this task exposes the service it calls.
- Password login and sessions; there are none, only bearer tokens.

## Verification Log
- 2026-09-15 · a17fe3e* · exit 0 · `set -o pipefail …` · acceptance-sha256:44304868802f449d70978dad58d78cdb65fcd2361ec05c97bfaec031d6e100cd · ms:4593
- 2026-09-15 · a17fe3e* · exit 0 · `set -o pipefail …` · acceptance-sha256:44304868802f449d70978dad58d78cdb65fcd2361ec05c97bfaec031d6e100cd · ms:3694
- 2026-09-15 · a17fe3e* · exit 0 · `set -o pipefail …` · acceptance-sha256:44304868802f449d70978dad58d78cdb65fcd2361ec05c97bfaec031d6e100cd · ms:2685
- 2026-09-15 · a17fe3e* · exit 0 · `set -o pipefail …` · acceptance-sha256:44304868802f449d70978dad58d78cdb65fcd2361ec05c97bfaec031d6e100cd · ms:2760
- 2026-09-15 · a17fe3e* · exit 0 · `set -o pipefail …` · acceptance-sha256:44304868802f449d70978dad58d78cdb65fcd2361ec05c97bfaec031d6e100cd · ms:3288
- 2026-09-15 · a17fe3e* · exit 0 · `set -o pipefail …` · acceptance-sha256:44304868802f449d70978dad58d78cdb65fcd2361ec05c97bfaec031d6e100cd · ms:2613
