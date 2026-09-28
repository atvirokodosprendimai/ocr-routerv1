# Task ADR-0009-T1: Carry an `unmetered` flag on the user, read and written with the user row

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `core.User.Unmetered` and its persistence (`Repo.UserByID`, `Repo.ListUsers`, `Repo.CreateUser`, `Repo.UpdateUser`)
**Consumes:** none
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the column default`, `the round trip through UpdateUser`

## Goal

Add `unmetered` to the `users` table and to `core.User`, and make every existing read and write of a
user carry it, changing no behaviour.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/store/migrations/00006_unmetered.sql` | add | the column, with the Down that drops it |
| `internal/core/user.go` | edit | `Unmetered bool` on `User`, with the comment saying why it is not a balance |
| `internal/store/repo.go` | edit | `userColumns` gains `unmetered`; both scans read it — this is what SELECTS the column on the read side, and a missing scan is a runtime error on every user read |
| `internal/store/repo_write.go` | edit | `CreateUser`'s INSERT and `UpdateUser`'s UPDATE carry it — `UpdateUser` is what SELECTS it on the write side, and without it the flag can be set and never stored |
| `internal/store/unmetered_test.go` | add | the round trip and the default |
| `internal/store/migrate_internal_test.go` | edit | the migration-count/name assertion this corpus keeps |

## Ordered Steps

1. [S1] Write the failing tests first: a freshly created user is metered (`Unmetered == false`), and a
   user whose `Unmetered` is set to true and written with `UpdateUser` reads back true. Both are red
   because the field does not compile yet.
2. [S2] Add `00006_unmetered.sql`: `ALTER TABLE users ADD COLUMN unmetered INTEGER NOT NULL DEFAULT 0;`
   with a Down that drops it, and a comment saying why `-1` in `credits` was rejected — the next
   reader of this column will otherwise wonder why the balance was not used.
3. [S3] Add `Unmetered bool` to `core.User`, documented as an EXEMPTION FROM METERING rather than a
   balance, naming the ledger reason.
4. [S4] Extend `userColumns` and both `Scan` call sites in `internal/store/repo.go`. ⚠ The scan list
   and the column list must move together; a column added to one is a runtime error on every user
   read, which is every authenticated request.
5. [S5] Extend `CreateUser`'s INSERT and `UpdateUser`'s UPDATE. Keep `UpdateUser`'s existing comment
   true: it still never touches `credits`.
6. [S6] Update whatever assertion in `migrate_internal_test.go` pins the migration set. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/store/ -run 'Unmetered|Migrat' -count=1 2>&1 | tee /tmp/adr9t1a.out && \
  ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/adr9t1a.out && \
  go build ./... && go test ./internal/store/ ./internal/core/ -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestANewUserIsMetered` | `internal/store/unmetered_test.go` | `CreateUser` leaves `Unmetered` false, so the safe state is what you get by not thinking about it | — | S1, S2, S3 |
| `TestUnmeteredSurvivesTheRoundTrip` | `internal/store/unmetered_test.go` | set true → `UpdateUser` → `UserByID` reads true; red if the column is missing from either the UPDATE or the scan | — | S4, S5 |
| `TestUnmeteredIsCarriedByListUsers` | `internal/store/unmetered_test.go` | the dashboard's read path carries it too — `ListUsers` has its own scan and is the one a single-user test cannot reach | — | S4 |
| `TestUpdateUserStillDoesNotTouchCredits` | `internal/store/unmetered_test.go` | ADR-0004's property survives this edit: an `UpdateUser` on a user with a balance leaves the balance alone | — | S5 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestANewUserIsMetered` |
| 2 — something selects it | `userColumns` + both scans + `UpdateUser`'s UPDATE; `TestUnmeteredSurvivesTheRoundTrip` goes red if any one of them drops the column |
| 3 — the caller can discover it | the column exists in the schema after `goose up`, asserted by the migration test |
| 4 — it is used | nothing yet — T2, T3 and T4 are the users of this field |

## Mutation Log

## Invariants

- `Repo.UpdateUser` never writes `credits`.
- Every existing user row reads as metered after the migration: `NOT NULL DEFAULT 0`.
- No behaviour changes in this task. Admission, delivery and the dashboard are untouched.

## Risks

- Adding a column to `userColumns` without adding it to a scan is a runtime error on every user read,
  and there are two scan sites. `TestUnmeteredIsCarriedByListUsers` exists because the single-user
  test cannot reach the second one.

## Stop Condition

Stop if `users` turns out to be read anywhere outside `internal/store` with a hand-written column
list — that would be a second place to keep in step, and it needs a decision rather than a patch.

## Out of Scope

- Any use of the flag. This task makes it storable and nothing more.

## Verification Log
