# Task ADR-0004-T2: Edit a customer where you already read them

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (web, views)
**Owner:** unassigned
**Produces:** `POST /admin/users/{id}/settings`, `POST /admin/users/{id}/credits`, `POST /admin/users/{id}/active`, the editable customer row
**Consumes:** `identity.UpdateSettings` (T1), `identity.AdjustCredits` (T1), `identity.SetActive` (ADR-0001, which has had no caller until now)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the routes that select the writers`, `the ledger entry through the UI`, `the admin gate on every route`

## Goal

Make every field the customer table already displays changeable in the row that displays it, with
credits adjusted by a signed amount and a reason rather than set.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/web/web.go` | edit | **the three routes** — inside the authenticated admin group, so ADR-0003's CSRF guard covers them |
| `internal/web/settings.go` | add | the three handlers and their signal parsing |
| `internal/web/views/views.templ` | edit | the editable row: number inputs for the three settings, an adjust-credits control, an active toggle |
| `internal/web/views/assets/app.css` | edit | styles for the inline editor |
| `internal/web/settings_test.go` | add | the failing tests |

`web.go`'s three route lines are the selecting lines. T1's methods are finished and tested and
reachable from nothing without them — which is the defect this whole record is about, so the mutants
bind here rather than to the handler bodies.

## Ordered Steps

1. [S1] Write the failing test first: `POST /admin/users/{id}/settings` changes a buffer limit
   through the real route table, before the route exists (TDD red). [proof: acceptance]
2. [S2] The customer row gains three number inputs — buffer, priority, TTL — bound to per-row
   signals, and a Save button posting to `/admin/users/{id}/settings`. ⚠ Signal suffixes are
   KEBAB-CASE: `data-bind:buffer-limit` binds `bufferLimit`. Writing `data-bind:bufferLimit` binds
   `bufferlimit` instead, a different signal, with nothing reporting the mistake.
3. [S3] A credits control: a signed number, a required reason, and an Adjust button posting to
   `/admin/users/{id}/credits`. ⚠ It is labelled **Adjust**, never Set, and the input is captioned
   with a sign — the control has to look like what it does, because "credits: 500" in a box beside a
   balance of 500 reads as the current value rather than an amount to add.
4. [S4] An active toggle posting to `/admin/users/{id}/active`, captioned with what it does:
   deactivation stops every token that user holds, at once. ADR-0001 called it "the blunt
   instrument" and the operator should read that where they click it.
5. [S5] Every handler re-renders `UserTable` on success, so the operator sees the new state in
   place — the confirmation and the result in one response, the same shape the token list uses.
6. [S6] A refusal from T1 renders inline through the existing `CreateError` path with the service's
   message, so "buffer limit must be at least 1; use the active toggle to stop a customer" reaches
   the screen rather than becoming a generic failure.
7. [S7] All three routes go inside the existing authenticated admin group, so ADR-0003's `Origin`
   guard covers them and `TestOnlyLoginAndHealthzAreUnauthenticated` keeps them there.
8. [S8] Verify in a browser: change a buffer limit, adjust credits with a reason, toggle active, and
   confirm each takes effect and is visible without a reload.
   [proof: human: an administrator performs each of the three edits in a real browser and sees the row update in place]

## Acceptance

```bash
set -o pipefail
templ generate \
  && go build ./... \
  && go test ./internal/web/... -count=1 -race 2>&1 | tee /tmp/adr4-t2.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr4-t2.out \
  && go test ./internal/identity/... ./cmd/router/... -count=1 2>&1 | tee /tmp/adr4-t2r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr4-t2r.out
```

`internal/web` carries the verdict; `identity` runs second as the regression for T1 and `cmd/router`
for the unauthenticated-route invariant. `templ generate` runs first because a stale
`views_templ.go` would test the previous markup. Red at authoring: the routes 404.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestBufferLimitIsEditableFromTheDashboard` | `internal/web/settings_test.go` | posting a new buffer limit changes it in the database — **the operator's actual complaint**, and red today | — | S2 |
| `TestPriorityAndTTLAreEditable` | `internal/web/settings_test.go` | both change through the same route, read back from the database | — | S2 |
| `TestSettingsEditRerendersTheTable` | `internal/web/settings_test.go` | the response carries the customer table with the new values, so the operator sees the result without a reload | — | S5 |
| `TestInvalidBufferLimitShowsTheServiceMessage` | `internal/web/settings_test.go` | posting 0 renders an error naming `active`, and the stored value is unchanged — the service's message reaches the screen rather than becoming a generic failure | — | S6 |
| `TestCreditsAreNeverSetDirectly` | `internal/web/settings_test.go` | adjusting by +50 through the UI moves the balance by exactly 50 AND appends exactly ONE ledger entry of +50 — **the record's `Enforced-by:` check.** A UI that set the balance would pass a balance-only assertion and silently end the audit trail | — | S3 |
| `TestCreditAdjustmentRequiresAReason` | `internal/web/settings_test.go` | a blank reason is refused, the balance is unchanged, and no entry is written | — | S3 |
| `TestNegativeCreditAdjustment` | `internal/web/settings_test.go` | -20 through the UI lowers the balance and writes an entry of -20 | — | S3 |
| `TestActiveToggleIsReachable` | `internal/web/settings_test.go` | the toggle deactivates and reactivates — **`identity.SetActive`'s first caller ever**, and red if the route is dropped | — | S4 |
| `TestDeactivatedUsersTokensStopWorking` | `internal/web/settings_test.go` | after deactivating through the UI, that user's bearer token gets 401 — the consequence the caption promises, asserted rather than described | — | S4 |
| `TestSettingsRoutesRequireAdmin` | `internal/web/settings_test.go` | all three routes return 403 for a client token and 401 for none — a customer must not be able to raise their own buffer limit or credits | — | S7 |
| `TestSettingsRoutesAreOriginGuarded` | `internal/web/settings_test.go` | a cookie-authenticated POST with no `Origin` returns 403 on each route — they are inside the group ADR-0003's guard covers | — | S7 |
| `TestEditableRowUsesKebabCaseBindings` | `internal/web/views/assets_test.go` | the rendered row contains `data-bind:buffer-limit` and no `data-bind:bufferLimit` — HTML lower-cases attribute names, so the camel form silently binds a different signal and the field never saves, with nothing reporting it | — | S2 |
| `TestCreditsControlSaysAdjustNotSet` | `internal/web/views/assets_test.go` | the rendered control reads "Adjust" and carries a sign hint, and the word "Set" appears on no credits control — a box labelled "Credits" beside a balance reads as the current value, and an operator typing 500 meaning "make it 500" would add 500 | — | S3 |
| `TestEditableRowHasAccessibleLabels` | `internal/web/views/assets_test.go` | each input carries an `aria-label` naming the field AND the customer — they sit in table cells with no room for a visible `<label>`, so without this they are five unlabelled number boxes to a screen reader, on the one page that changes what customers can do | — | S2, S3 |
| `TestDeactivateButtonSaysWhatItDoes` | `internal/web/views/assets_test.go` | the control's title says it stops every token immediately — ADR-0001 called deactivation the blunt instrument, and this is the one action here whose blast radius exceeds its affordance | — | S4 |
| `TestSettingsRejectsNonNumericInput` | `internal/web/settings_test.go` | `"eight"` is refused with a message naming the field, and nothing is written | — | S6 |
| `TestSettingsRejectsEmptyField` | `internal/web/settings_test.go` | an empty field reports "required" rather than being read as 0 — a blank buffer limit read as zero would hit the service floor and produce a confusing refusal about a value the operator never typed | — | S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the thirteen tests above |
| 2 — something selects it | `web.go`'s three route lines. `TestBufferLimitIsEditableFromTheDashboard` and `TestActiveToggleIsReachable` each go red when their one line is deleted — and `SetActive` is the third method in this codebase found finished, tested and called by nothing, so this rung is the whole point of the task. |
| 3 — the caller can discover it | the controls are in the row that already displays the values; each carries a caption saying what it does |
| 4 — it is used | S8's human sign-off in a real browser |

## Mutation Log

- 2026-09-15 · a5674be* · mutant killed · exit 1 · `internal/web/web.go` · the settings handler exists and is mounted on nothing, so the buffer limit is still unchangeable from the dashboard — the exact defect this record exists to fix, and every test in T1 survives it · acceptance-sha256:f2dfc08fb08082c297525599428a58f1e731c14647af9df08a12d334e6ec631b · covers:the routes that select the writers
- 2026-09-15 · a5674be* · mutant killed · exit 1 · `internal/web/settings.go` · the operator reason is dropped before it reaches the ledger, so every manual adjustment records nothing about why it happened and the audit is unreadable six months later · acceptance-sha256:f2dfc08fb08082c297525599428a58f1e731c14647af9df08a12d334e6ec631b · covers:the ledger entry through the UI
- 2026-09-15 · a5674be* · mutant killed · exit 1 · `internal/web/web.go` · the credit adjustment route is unmounted, so an operator has no way to move a balance except a raw UPDATE that writes no ledger entry · acceptance-sha256:f2dfc08fb08082c297525599428a58f1e731c14647af9df08a12d334e6ec631b · covers:the admin gate on every route

## Invariants

- No route writes a credit balance; every movement goes through `AdjustCredits`.
- All three routes are inside the authenticated admin group.
- Every successful edit re-renders the table.
- Every refusal shows the service's own message.

## Risks

- **`TestCreditsAreNeverSetDirectly` must count the ledger entries, not just check the balance.**
  The balance-only version passes against exactly the implementation this record forbids, and it is
  the shorter one to write.
- **The kebab-case binding failure is silent.** `data-bind:bufferLimit` binds `bufferlimit`, the
  field never saves, and nothing anywhere reports it — no console error, no failed request. Only a
  test over the rendered markup catches it, which is why one exists.
- **A credits box labelled with the current balance invites "set" semantics.** An operator typing
  500 meaning "make it 500" adds 500 instead. The caption and the test on it are the mitigation; a
  confirmation step would be better and is heavier than this control warrants.
- **`TestDeactivatedUsersTokensStopWorking` needs a REAL request with that user's token**, not an
  inspection of the row. The row flipping is not the consequence; the token failing is.
- **Three new state-changing routes are three new CSRF surfaces.** They are inside the guarded
  group, and a test asserts the guard covers each — adding one outside it would pass every other
  test here.
- **The human sign-off is not optional.** Every test in T1 and T2 can pass while the inputs are
  unreadable, the toggle is mislabelled, or the row reflows unusably on a narrow screen — and
  ADR-0003's own history in this repository is a feature that passed every test while being unusable
  in a browser.

## Stop Condition

Stop and ask if the operator wants a confirmation step before deactivation. It stops every token the
customer holds, at once, from a single click in a table row — which is the one action here whose
blast radius exceeds its affordance.

## Out of Scope

- Editing email or role (permanent: boundary: email is the login identity and unique key; a token
  carries its own role, so a role control would mislead).
- Bulk edits across customers (deferred: `docs/adr/BACKLOG.md`).
- An audit log of administrative changes (deferred: `docs/adr/BACKLOG.md`).
- A per-customer detail page (permanent: boundary: the table already shows every field, and a second
  rendering of the same row is how two renderings drift).

## Verification Log
- 2026-09-15 · a5674be* · exit 0 · `set -o pipefail …` · acceptance-sha256:f2dfc08fb08082c297525599428a58f1e731c14647af9df08a12d334e6ec631b · ms:74745
- 2026-09-15 · a5674be* · exit 0 · `set -o pipefail …` · acceptance-sha256:f2dfc08fb08082c297525599428a58f1e731c14647af9df08a12d334e6ec631b · ms:60110
- 2026-09-15 · a5674be* · exit 0 · `set -o pipefail …` · acceptance-sha256:f2dfc08fb08082c297525599428a58f1e731c14647af9df08a12d334e6ec631b · ms:55006
- 2026-09-15 · a5674be* · exit 0 · `set -o pipefail …` · acceptance-sha256:f2dfc08fb08082c297525599428a58f1e731c14647af9df08a12d334e6ec631b · ms:42813
- 2026-09-15 · human-observed · S8: drove a real binary on 2026-09-15 — the customers page rendered number inputs bound kebab-case (buffer-limit, priority, job-ttl, credit-delta, credit-reason) plus the Adjust control and a Disable button. Set buffer=9 priority=3 ttl=600: stored. Adjusted +250 with reason 'prepaid invoice 1042': balance moved to 250 and the ledger row reads '250|admin: prepaid invoice 1042'. Posted buffer=0: refused on screen with 'buffer limit must be at least 1; use the active toggle to stop a customer', and the stored value stayed 9. Toggled active: stored 0.
