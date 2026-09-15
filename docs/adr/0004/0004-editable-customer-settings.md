# ADR-0004: Make customer settings editable from the dashboard, with credits adjusted rather than set

**Status:** Accepted
**Date:** 2026-09-15
**Owner:** M (operator) — authored by claude-code-aks
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/tasks/T10-admin-dashboard.md`, `docs/adr/0002/0002-rate-limiting-and-structured-logging.md`, `docs/adr/0003/0003-admin-password-login.md`
**Governs:** `internal/web/**`, `internal/identity/**`
**Enforced-by:** `internal/web/settings_test.go::TestCreditsAreNeverSetDirectly`
**Invalidates:** none — ADR-0001 defined these fields as admin-set and named no mechanism for
setting them; this record supplies the mechanism (`adr-state.mjs`, 2026-09-15).
**Served-path change:** An administrator can change a customer's buffer limit, priority, job TTL
and active state from `/admin/users`, and adjust their credit balance with a reason. Today every
one of those requires an `UPDATE` typed into SQLite by hand.

## Context

ADR-0001 gave every customer four knobs and an on/off switch — `buffer_limit`, `priority`,
`job_ttl_secs`, `credits`, `active` — and described each as operator-set. T10 built the dashboard
that displays all five and can change **none** of them. The only writes the UI has are: create a
user, mint a token, and set a per-label rate.

So the product today asks an operator to open `ocr-router.db` in a SQLite client and write
`UPDATE users SET buffer_limit = 8 WHERE id = '…'` to do something the dashboard already shows them.
That is the gap the operator hit: *"cant change buffer in gui"*.

**This is the third instance of one defect in this codebase**, and that pattern is the reason to fix
it properly rather than bolt on a form. `identity.ListTokens` and `identity.RevokeToken` were
written, tested and called by nothing until an hour ago; `identity.SetActive` still is —
admin-gated, tested, and unreachable from any HTTP route. A method with a unit test and no caller
passes every gate this pipeline has except Reachability rung 2, and rung 2 is the one a reviewer
signs off from memory.

**The constraint that shapes the whole record: credits are a LEDGER, not a number.**
`credit_entries` is an append-only audit of every movement, `Repo.AddCredits` writes the balance and
the entry in one transaction, and ADR-0002 added `ocrr_credits_debited_total` on the promise that a
metric used to reconcile the books agrees with them. A UI offering "set credits to 500" would write
the balance and no entry, and the audit trail would silently stop being an audit trail — with
nothing failing, because nothing checks.

## Existing Primitives Audit

Checked before proposing anything new (`grep` over `internal/store`, `internal/identity`,
`internal/web`, 2026-09-15):

| Need | Existing primitive | Reused? |
|---|---|---|
| Move a credit balance and record why | `Repo.AddCredits(userID, delta, reason, now)` — balance and ledger row in one transaction | **Yes, and it is the only permitted path.** See Decision 1 |
| Enable/disable an account | `identity.SetActive` — admin-gated, tested | **Yes** — and this record is its first caller ever |
| Admin authorisation on a write | `identity`'s `actor core.Principal` + `IsAdmin()` pattern on every mutating method | **Yes** — the new setter follows it rather than letting `web` call the repo directly |
| Write user columns | `Repo.UpdateUser(u core.User)` — whole-row | **No.** See Decision 2: a whole-row write from a read-modify-write is a lost update, and it can clobber `credits` from stale data |
| Patch a fragment into the page | `Web.patch` + datastar, used by every existing action | **Yes** |
| Render the customer table | `views.UserTable` | **Yes** — edited in place rather than replaced |
| Per-field validation | `core`'s sentinel errors + `friendly()` in web | **Yes** |

## Decision

**We will add one admin-gated settings writer and one credit-adjustment path, and surface both in
the customer table — with credits adjusted by a signed delta and a reason, never set to a value.**

### 1. Credits are ADJUSTED, never SET

The UI offers `+/- N` with a required reason, and calls `Repo.AddCredits`. There is no "set credits
to" control and no code path that writes `users.credits` directly.

⚠ **This is not a UI preference, it is the integrity of the ledger.** `credit_entries` is the
append-only record of every movement; a direct `SET` writes the balance and no entry, so the ledger
and the balance diverge permanently and silently. ADR-0002 put `ocrr_credits_debited_total` on the
metrics endpoint specifically so an operator could reconcile the two — that reconciliation is
worthless the moment one write bypasses the ledger.

The **reason is required**, not optional. An adjustment with no reason is indistinguishable from a
mistake six months later, and this is the only field in the system an administrator can change that
directly moves money.

### 2. A targeted settings writer, not a whole-row update

`Repo.SetUserSettings(ctx, userID, buffer, priority, ttl)` writes exactly three columns.

⚠ **`Repo.UpdateUser` is deliberately not used from this path.** It writes the whole row, which
means the caller must read the user first and send back every field — and `credits` moves
underneath that read every time a job is delivered. Two admins editing at once, or one admin editing
while a job completes, silently restores a stale balance. Writing three named columns cannot clobber
a fourth.

### 3. The three settings, and what each refuses

| Field | Accepted | Refused, and why |
|---|---|---|
| `buffer_limit` | ≥ 1 | **Zero is refused.** Admission compares in-flight jobs against the limit, so a limit of 0 silently rejects every upload that customer makes, forever, with a `buffer full` error that looks like back-pressure. Disabling a customer is what `active` is for, and it says so |
| `priority` | any integer | Nothing. Higher wins; ADR-0001 made it an integer rather than a named tier so adding a tier is an `UPDATE` |
| `job_ttl_secs` | ≥ 0 | Negative. Zero means **no deadline** and is a real, common choice |

### 4. `active` gets its first caller

A toggle in the customer row, calling `identity.SetActive`. ADR-0001 called deactivation "the blunt
instrument: every token the user holds stops working"; the UI says that where the operator clicks it,
because a control whose consequence is invisible is one that gets clicked by mistake.

### 5. Email and role stay read-only

**Email** is the login identity (ADR-0003) and the unique key; changing it is an account migration,
not a settings edit.

**Role** is deliberately not editable here. A token carries its OWN role, independent of its user's
(ADR-0001 T3 asserts this) — so demoting a user does not demote their tokens, and a UI control that
looked like it did would be actively misleading. Changing a role correctly means revoking and
re-minting, which the token list now supports.

## Alternatives Considered

- **A "set credits" field:** the obvious control, and what an operator asks for. Rejected because it
  breaks the ledger invariant (Decision 1). The adjustment form is one extra field — a reason — and
  buys a permanent audit trail.

- **Reusing `Repo.UpdateUser` and sending the whole user back from the form:** less new code, and it
  is what the repository already offers. Rejected: read-modify-write over a row whose `credits`
  column moves on every delivery is a lost update with real money in it (Decision 2).

- **Editing in a modal, or on a per-customer detail page:** more room, and conventional. Rejected
  for now because the table already shows every field, and a detail page means a second place the
  same values are rendered — two renderings of one row is how they drift. Inline editing keeps one.

- **Letting `web` call `store.Repo` directly for settings:** shorter, and `web` already holds a
  `Repo` for reads. Rejected: every mutating path in this system goes through `identity` with an
  `actor` and an admin check, and a second authorisation style is how one of them ends up missing
  the check.

- **Optimistic concurrency with a version column:** correct for a genuinely concurrent editor.
  Rejected as disproportionate — writing three named columns removes the clobbering hazard that
  matters, and two administrators editing the same customer's buffer limit within the same second is
  not a scenario this product has. Recorded so the omission is deliberate.

- **An audit log of settings changes:** ADR-0002's transition log records job state, not
  administrative action. Worth having and deferred, since it needs a decision about where such a log
  lives and how long it is kept.

## Component / Boundary Impact

No new package and no new bounded context. Two existing components widen:

- **`internal/identity`** — gains `UpdateSettings` and `AdjustCredits`, both admin-gated, both
  following the `actor core.Principal` pattern every other mutating method here uses. It remains the
  only package that authorises a write.
- **`internal/web`** — gains four routes and edits one view.
- **`internal/store`** — gains `SetUserSettings`, a three-column write.

The C4 container diagram is unchanged. No new process, listener, dependency or schema change.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| HTTP | `POST /admin/users/{id}/settings` | T2 | dashboard |
| HTTP | `POST /admin/users/{id}/credits` | T2 | dashboard |
| HTTP | `POST /admin/users/{id}/active` | T2 | dashboard |
| `identity` | `UpdateSettings(actor, userID, buffer, priority, ttl)` | T1 | T2 |
| `identity` | `AdjustCredits(actor, userID, delta, reason, now)` | T1 | T2 |
| `store` | `SetUserSettings(userID, buffer, priority, ttl)` | T1 | T1 |
| `core` errors | reuses `ErrInvalidParam` and `ErrForbidden`; no new sentinel | T1 | T2 |
| Database schema | **None** — every column already exists | — | — |
| SSE protocol | **None** | — | — |
| CLI | **None** | — | — |

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `identity.UpdateSettings` | T1 | T2 | No — new method |
| `identity.AdjustCredits` | T1 | T2 | No — new method |
| `store.SetUserSettings` | T1 | — | No — new method |
| The three settings routes and the edited `UserTable` | T2 | — | No — additive routes |

## Implementation

Two tasks in `docs/adr/0004/tasks/`: T1 is the authorised write surface, T2 is the UI that reaches
it. See `docs/adr/0004/tasks/README.md`.

## Consequences

**Good.** The dashboard stops being read-mostly: everything it displays about a customer, it can
change. Three service methods that had no caller acquire one. Every credit movement keeps an entry,
including manual ones — which is strictly better than today, where a manual change is a raw `UPDATE`
with no entry at all.

**Bad.** Five new ways for an administrator to break a customer, reachable in one click: a buffer
limit of 1 starves them, a negative priority buries them, a short TTL expires their work, and
deactivation stops every token at once. Validation catches the incoherent cases; it cannot catch the
merely unwise ones, and the UI says what each control does rather than pretending it is safe.

**Neutral but load-bearing.** Settings writes go through the single writer, like everything else.
They are rare and small; this would matter if they were not.

## Out of Scope

- Editing a customer's email (permanent: boundary: it is the login identity and the unique key —
  changing it is an account migration, not a settings edit).
- Editing a user's role (permanent: boundary: a token carries its own role independent of its
  user's, so a role control would not do what it appears to; revoke and re-mint is the correct
  path and the token list now supports it).
- An audit log of administrative changes (deferred: `docs/adr/BACKLOG.md`).
- Optimistic concurrency on the user row (permanent: boundary: three named columns cannot clobber a
  fourth, which removes the hazard that matters, and simultaneous edits of one customer are not a
  scenario this product has).
- Bulk edits across customers (deferred: `docs/adr/BACKLOG.md`).
- Setting a credit balance to an absolute value (permanent: boundary: it writes the balance without
  a ledger entry and silently ends the audit trail — Decision 1).

## Risks

| # | Risk | Mitigation |
|---|---|---|
| 1 | **A later change adds a direct credits write** and nobody notices, because nothing fails. | `TestCreditsAreNeverSetDirectly` drives an adjustment through the UI and asserts the ledger gained exactly one entry whose delta matches the balance change. Named in `Enforced-by:`. A mutant binds to it. |
| 2 | **`buffer_limit = 0` silently blocks every upload**, reported to the customer as `buffer full`, which reads like ordinary back-pressure. | Refused at the service layer with a message naming `active` as the way to stop a customer. Both bounds tested — 0 refused, 1 accepted. |
| 3 | **A settings write clobbers `credits`** from a stale read. | `SetUserSettings` writes three named columns and never touches `credits`; a test changes settings while the balance differs from the form's view and asserts the balance survives. |
| 4 | **A test that asserts "invalid input is refused" passes against a handler that refuses everything.** | Every negative case is paired with a positive one in the same run, the discipline ADR-0002 and ADR-0003 both used. |
| 5 | **The credit reason could be empty**, leaving an entry that explains nothing. | Required and non-blank after trimming; refused otherwise, and tested. |
| 6 | **A non-admin reaching the settings routes.** | The routes sit inside the admin group and `identity` re-checks the actor; asserted for a client token and for no token, on every route. |
| 7 | **CSRF on new state-changing routes.** ADR-0003's guard covers `/admin`, but a route added outside that group would miss it. | The routes are added inside the existing group; `TestOnlyLoginAndHealthzAreUnauthenticated` fails if one escapes it. |
| 8 | **An adjustment that would take the balance far negative** is accepted. | Deliberate: ADR-0001 already allows a balance to go negative by design, and an admin subtracting credits from a debtor is a legitimate correction. Recorded so the absence of a floor is not read as an oversight. |

## Rollback

No schema change and no contract an existing client depends on.

- **The feature** is removed by reverting this record's commits; the columns and the ledger are
  untouched and every value an administrator set through the UI remains valid, because it is the
  same value a hand-written `UPDATE` would have produced.
- **A bad adjustment** is corrected by the inverse adjustment, which is itself a ledger entry — the
  audit shows both, which is the point.
- No data migration in either direction.

## Follow-ups

- Operator to decide whether administrative changes need their own audit stream, now deferred.
- Revisit inline editing if the customer table grows past what a row can hold.
