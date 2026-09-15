# ADR-0004 tasks

Derived index — the task files are the source of truth. Execute in order.

| Task | Goal | Status | Depends-on | Acceptance (first command) |
|------|------|--------|------------|----------------------------|
| T1 | An authorised settings writer that cannot touch credits | pending | none | `go test ./internal/identity/... -race` |
| T2 | Edit a customer where you already read them | pending | T1 | `go test ./internal/web/... -race` |

## Order

    T1 ── T2

T1 is the authorised write surface; T2 is the UI that reaches it. Nothing is parallel: T2's whole
purpose is to be T1's first caller.

## The one decision that shapes both

**Credits are a LEDGER, not a number.** `credit_entries` is the append-only record of every
movement, and `Repo.AddCredits` writes the balance and the entry in one transaction. A UI offering
"set credits to 500" would write the balance and no entry, and the audit trail would silently stop
being an audit trail — with nothing failing, because nothing checks.

So the UI adjusts by a signed amount with a required reason, there is no method anywhere that sets a
balance, and `TestCreditsAreNeverSetDirectly` counts the ledger entries rather than checking the
balance. The balance-only version of that test passes against exactly the implementation this record
forbids, and it is the shorter one to write.

## Why this record exists at all

`identity.SetActive` is finished, tested, admin-gated, and reachable from nothing — the **third**
instance of that defect found in this codebase, after `ListTokens` and `RevokeToken` an hour
earlier. A method with a unit test and no caller passes every gate this pipeline has except
Reachability rung 2, and rung 2 is the one a reviewer signs off from memory.

T1 therefore records its own rung 2 as **"nothing selects it yet"** rather than claiming credit for
tests, and T2's mutants bind to the route lines rather than to handler bodies.

## Convention

Status here is derived from each task's `## Verification Log`: a task may be marked `done` only once
`adr-verify` has recorded an exit-0 entry whose digest matches the task's current Acceptance fence,
plus at least one killed mutant bound to the same digest. Do not hand-edit either log.
