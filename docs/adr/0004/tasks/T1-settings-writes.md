# Task ADR-0004-T1: An authorised settings writer that cannot touch credits

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (store + identity)
**Owner:** unassigned
**Produces:** `store.SetUserSettings()`, `identity.UpdateSettings()`, `identity.AdjustCredits()`
**Consumes:** none from this record's siblings — `Repo.AddCredits` and `identity.SetActive` already exist
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the three-column write`, `the ledger entry`, `the buffer-limit floor`, `the admin gate`

## Goal

Give the dashboard a way to change a customer's settings and balance that is authorised like every
other write in this system, cannot clobber credits, and cannot move a balance without recording why.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/store/repo_write.go` | edit | `SetUserSettings` — three named columns, never the whole row |
| `internal/identity/settings.go` | add | `UpdateSettings` and `AdjustCredits`, both admin-gated and validating |
| `internal/identity/settings_test.go` | add | the failing tests |
| `internal/store/repo_test.go` | edit | the column-isolation test |

Nothing selects these yet — T2's routes do. Rung 2 is discharged there, and saying so is the point:
this record exists because three methods in this package already had tests and no caller.

## Ordered Steps

1. [S1] Write the failing test first: `AdjustCredits` moves the balance AND writes a ledger entry,
   before either exists (TDD red). [proof: acceptance]
2. [S2] `Repo.SetUserSettings(ctx, userID, buffer, priority, ttl)` writes exactly
   `buffer_limit`, `priority`, `job_ttl_secs`. ⚠ Not `UpdateUser`: a whole-row write from a
   read-modify-write restores a stale `credits` every time a job is delivered underneath it. Three
   named columns cannot clobber a fourth. It returns `core.ErrNotFound` when no row matched, because
   silence would report success for a user that does not exist.
3. [S3] `identity.UpdateSettings(ctx, actor, userID, buffer, priority, ttl)` checks
   `actor.IsAdmin()` first, then validates, then writes. The admin check is first so an
   unauthorised caller learns nothing about which values are valid.
4. [S4] ⚠ **`buffer_limit` below 1 is refused** with `core.ErrInvalidParam` and a message naming
   `active`. Admission compares in-flight jobs against the limit, so 0 silently rejects every upload
   that customer ever makes, reported to them as `buffer full` — which reads like ordinary
   back-pressure and is indistinguishable from a busy system.
5. [S5] `job_ttl_secs` below 0 is refused. **Zero is valid and means no deadline** — a real and
   common choice, so it must not be swept up by a `> 0` check.
6. [S6] `priority` accepts any integer, including negative. Higher wins; ADR-0001 made it an integer
   rather than a named tier so adding a tier is an `UPDATE`.
7. [S7] `identity.AdjustCredits(ctx, actor, userID, delta, reason, now)` is admin-gated, refuses a
   blank reason and a zero delta, and calls `Repo.AddCredits` — which writes the balance and the
   ledger entry in one transaction. ⚠ **There is no method anywhere that sets a balance.** A direct
   write would move the balance with no entry, and `credit_entries` would stop being an audit of
   every movement with nothing failing to say so.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/identity/... -count=1 -race 2>&1 | tee /tmp/adr4-t1.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr4-t1.out \
  && go test ./internal/store/... ./internal/router/... -count=1 2>&1 | tee /tmp/adr4-t1r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr4-t1r.out
```

`internal/identity` carries the verdict; `store` runs second for the new query and `router` because
it is what reads `buffer_limit` and `job_ttl_secs` at admission. Red at authoring: `settings.go`
does not exist, so the package does not compile and `^FAIL` matches.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestUpdateSettingsWritesAllThree` | `internal/identity/settings_test.go` | buffer, priority and TTL all change, read back from the database — the positive half every refusal test below needs | — | S2, S3 |
| `TestUpdateSettingsDoesNotTouchCredits` | `internal/identity/settings_test.go` | with a balance changed AFTER the caller read the user, a settings write leaves the new balance intact — **red against a whole-row `UpdateUser`**, which is the shorter implementation | — | S2 |
| `TestUpdateSettingsDoesNotTouchRoleOrEmail` | `internal/identity/settings_test.go` | role and email survive a settings write | — | S2 |
| `TestBufferLimitBelowOneIsRefused` | `internal/identity/settings_test.go` | 0 and -1 return `core.ErrInvalidParam` and 1 is accepted — both bounds, so an off-by-one is visible; and the error names `active`, because an operator who wanted to stop a customer needs to be told where that lives | — | S4 |
| `TestZeroJobTTLIsAccepted` | `internal/identity/settings_test.go` | 0 is stored and means no deadline — red against a `> 0` validation, which is the natural way to write "must be positive" and would forbid the common case | — | S5 |
| `TestNegativeJobTTLIsRefused` | `internal/identity/settings_test.go` | -1 returns `core.ErrInvalidParam` | — | S5 |
| `TestNegativePriorityIsAccepted` | `internal/identity/settings_test.go` | -5 is stored — deliberately allowed, and asserted so a later "tidy-up" validation cannot quietly forbid it | — | S6 |
| `TestUpdateSettingsRequiresAdmin` | `internal/identity/settings_test.go` | a client and a worker principal both get `core.ErrForbidden`, and nothing is written | — | S3 |
| `TestAdjustCreditsMovesBalanceAndWritesEntry` | `internal/identity/settings_test.go` | +50 moves the balance by exactly 50 AND appends one ledger entry with delta 50 and the given reason — **both halves in one assertion**, because a balance write with no entry is the failure this record exists to prevent | — | S7 |
| `TestAdjustCreditsNegativeDelta` | `internal/identity/settings_test.go` | -20 moves the balance down and writes an entry of -20 | — | S7 |
| `TestAdjustCreditsRefusesBlankReason` | `internal/identity/settings_test.go` | `""` and `"   "` are refused and nothing is written — an adjustment with no reason is indistinguishable from a mistake six months later | — | S7 |
| `TestAdjustCreditsRefusesZeroDelta` | `internal/identity/settings_test.go` | 0 is refused rather than writing an entry that records nothing happening | — | S7 |
| `TestAdjustCreditsRequiresAdmin` | `internal/identity/settings_test.go` | a client principal gets `core.ErrForbidden` and the balance is unchanged | — | S7 |
| `TestNoMethodSetsCreditsDirectly` | `internal/identity/settings_test.go` | reflection over `identity.Service` and `store.Repo` finds no exported method matching `Set.*Credit` — the structural half of Decision 1, so the rule survives a future author who never reads this task | — | S7 |
| `TestSetUserSettingsOnUnknownUser` | `internal/store/repo_test.go` | an unknown id returns `core.ErrNotFound`, not silent success | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the fifteen tests above |
| 2 — something selects it | **nothing selects it yet** — T2's routes are the first caller, and its mutants discharge this rung. Recorded rather than claimed: this record exists BECAUSE three methods in this package had tests and no caller, and repeating that here would be the same mistake with a fresh coat of paint. |
| 3 — the caller can discover it | doc comments on every exported identifier; the refusal messages name what to do instead |
| 4 — it is used | T2's human sign-off — an administrator changing a buffer limit in a browser |

## Mutation Log

- 2026-09-15 · a5674be* · mutant killed · exit 1 · `internal/store/repo_write.go` · the settings write also touches credits, which is what a whole-row UpdateUser does from a stale read — every delivery landing while an admin form is open is silently undone · acceptance-sha256:f2b0b6948002ee91f1f1046c6a29c673bbb943ff54c839c26b59350ae08e82a7 · covers:the three-column write
- 2026-09-15 · a5674be* · mutant killed · exit 1 · `internal/identity/settings.go` · the balance moves with no ledger entry, so credit_entries stops being an audit of every movement and nothing fails to say so · acceptance-sha256:f2b0b6948002ee91f1f1046c6a29c673bbb943ff54c839c26b59350ae08e82a7 · covers:the ledger entry
- 2026-09-15 · a5674be* · mutant killed · exit 1 · `internal/identity/settings.go` · a buffer limit of zero is accepted, which silently rejects every upload that customer ever makes and reports it to them as ordinary back-pressure · acceptance-sha256:f2b0b6948002ee91f1f1046c6a29c673bbb943ff54c839c26b59350ae08e82a7 · covers:the buffer-limit floor

## Invariants

- No exported method anywhere sets a credit balance; every movement goes through `AddCredits`.
- Every credit movement has a non-blank reason.
- A settings write touches exactly three columns.
- `buffer_limit` is never below 1; `job_ttl_secs` is never below 0; `priority` is unconstrained.
- Every mutating method checks `actor.IsAdmin()` before it validates or writes.

## Risks

- **`TestAdjustCreditsMovesBalanceAndWritesEntry` must assert BOTH.** Checking only the balance
  passes against a direct write that skips the ledger, which is the exact defect this record is
  about; checking only the entry passes against one that logs and does not pay.
- **`TestUpdateSettingsDoesNotTouchCredits` proves nothing unless the balance changes AFTER the
  read.** A test that reads a user, writes settings and checks credits will pass against
  `UpdateUser` too, because nothing moved in between. The fixture has to move it.
- **`TestNoMethodSetsCreditsDirectly` is a reflection test over method NAMES**, so a method called
  `OverwriteBalance` would slip past it. It is a tripwire, not a proof — the invariant above and the
  bound mutant are what a reviewer reads. Stated rather than overclaimed.
- **The admin check must come before validation**, or an unauthorised caller learns which values are
  valid by watching which errors come back.
- **`priority` accepting negatives looks like missing validation** to the next reader. The test
  asserting it is deliberate is the only thing that stops someone "fixing" it.

## Stop Condition

Stop and ask if the operator wants a floor on how far negative an adjustment may take a balance.
ADR-0001 already lets a balance go negative by design and an admin correcting a debtor is
legitimate, so this task allows it — but a floor is a policy decision and not one to invent.

## Out of Scope

- The routes and the UI — T2.
- Editing email or role (permanent: boundary: email is the login identity and the unique key; a
  token carries its own role, so a role control would not do what it appears to).
- An audit log of administrative changes (deferred: `docs/adr/BACKLOG.md`).
- Bulk edits across customers (deferred: `docs/adr/BACKLOG.md`).

## Verification Log
- 2026-09-15 · a5674be* · exit 0 · `set -o pipefail …` · acceptance-sha256:f2b0b6948002ee91f1f1046c6a29c673bbb943ff54c839c26b59350ae08e82a7 · ms:56498
- 2026-09-15 · a5674be* · exit 0 · `set -o pipefail …` · acceptance-sha256:f2b0b6948002ee91f1f1046c6a29c673bbb943ff54c839c26b59350ae08e82a7 · ms:45700
- 2026-09-15 · a5674be* · exit 0 · `set -o pipefail …` · acceptance-sha256:f2b0b6948002ee91f1f1046c6a29c673bbb943ff54c839c26b59350ae08e82a7 · ms:49337
- 2026-09-15 · a5674be* · exit 1 · `set -o pipefail …` · acceptance-sha256:f2b0b6948002ee91f1f1046c6a29c673bbb943ff54c839c26b59350ae08e82a7 · ms:216
  ```
  --- last 2 line(s) of stderr
  internal/web/settings.go:12:2: no required module provides package github.com/atvirokodosprendimai/ocr-router/internal/views; to add it:
  	go get github.com/atvirokodosprendimai/ocr-router/internal/views
  ```
- 2026-09-15 · a5674be* · exit 0 · `set -o pipefail …` · acceptance-sha256:f2b0b6948002ee91f1f1046c6a29c673bbb943ff54c839c26b59350ae08e82a7 · ms:64185
