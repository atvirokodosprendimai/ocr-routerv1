# Task ADR-0009-T4: Give an administrator the toggle, and make the state visible on the table

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `identity.Service.SetUnmetered`, `POST /admin/users/{id}/unmetered`
**Consumes:** `core.User.Unmetered` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the admin gate`, `the server-side inversion`, `the route's presence in the authenticated group`, `the visible state`

## Goal

An administrator can mark a customer unmetered and back from the Customers table, and can see which
customers are unmetered without clicking anything.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/identity/settings.go` | edit | `SetUnmetered`, admin-gated, reading the stored value and writing its inverse — the same shape as `SetActive` |
| `internal/web/settings.go` | edit | the handler, ending in `refreshUsers` like every other row action |
| `internal/web/web.go` | edit | `r.Post("/users/{id}/unmetered", …)` INSIDE the authenticated + `requireAdmin` + `requireSameOrigin` group — this is what SELECTS the handler, and a handler mounted outside that group is both unreachable to the dashboard and an unauthenticated route |
| `internal/web/views/views.templ` | edit | the Credits cell renders `unlimited` for an unmetered customer, and a toggle button beside `Disable` |
| `internal/web/views/views_templ.go` | regenerate | `templ generate`; never hand-edited |
| `internal/web/unmetered_test.go` | add | the route, the gate, the toggle and the rendering |
| `cmd/router/monitoring_test.go` | — | no edit expected: the new route is authenticated, so `TestOnlyLoginAndHealthzAreUnauthenticated` covers it automatically. If it needs an edit, STOP — that means the route escaped the group |

## Ordered Steps

1. [S1] Write the failing tests first: `POST /admin/users/{id}/unmetered` as an admin flips the stored
   flag, and the Customers page renders `unlimited` in the Credits cell for an unmetered customer. Both
   red — the route does not exist.
2. [S2] Add `identity.SetUnmetered(ctx, actor, userID, unmetered bool)`: refuse a non-admin with
   `core.ErrForbidden`, read the user, set the field, `UpdateUser`. Mirror `SetActive` deliberately,
   including its doc comment's reasoning.
3. [S3] Add the web handler. It takes NO signal from the browser: it reads the current value and
   writes the inverse, for the reason `setActive`'s comment already gives — trusting a posted boolean
   lets a stale page re-assert a state somebody just changed. End in `refreshUsers` so the operator
   sees the new state in place.
4. [S4] Mount the route inside the authenticated group, beside `/users/{id}/active`.
5. [S5] Render the state: the Credits cell shows `unlimited` (not a number and not `-1`) for an
   unmetered customer, and a button reading `Meter` for one that is unmetered or `Unmeter` for one
   that is not, with a `title` naming the consequence — that this customer's work stops being charged.
6. [S6] `templ generate`, then confirm the served HTML carries the button and the word. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
templ generate && go test ./internal/web/ -run 'Unmeter' -count=1 2>&1 | tee /tmp/adr9t4a.out && \
  ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/adr9t4a.out && \
  go test ./internal/web/... ./internal/identity/ ./cmd/router/ -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestAdminTogglesUnmetered` | `internal/web/unmetered_test.go` | the route flips the stored flag, and flips it back on a second call | — | S1, S3, S4 |
| `TestUnmeteredRouteRefusesANonAdmin` | `internal/web/unmetered_test.go` | a client and a worker token both get 403 — the gate, not the handler, and red if the route is mounted outside `requireAdmin` | — | S2, S4 |
| `TestUnmeteredRouteIgnoresAPostedValue` | `internal/web/unmetered_test.go` | posting `{"unmetered":false}` at an already-metered customer still turns it ON; the browser's opinion is not consulted | — | S3 |
| `TestTheCreditsCellReadsUnlimitedWhenUnmetered` | `internal/web/unmetered_test.go` | the rendered page says `unlimited` for an unmetered customer and the number for a metered one — red if the state is stored and invisible | — | S1, S5 |
| `TestTheUnmeterToggleButtonNamesTheDirection` | `internal/web/unmetered_test.go` | the button reads `Unmeter` on a metered row and `Meter` on an unmetered one, so the label is the action rather than the state | — | S5 |
| `TestSetUnmeteredRefusesANonAdminActor` | `internal/identity/settings_test.go` | the service refuses below the HTTP layer too, which is what makes the API route safe as well as the dashboard | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestSetUnmeteredRefusesANonAdminActor` |
| 2 — something selects it | the `r.Post` in `web.Mount`; `TestAdminTogglesUnmetered` goes red if that line is deleted, which is the defect this codebase has shipped three times (`identity.ListTokens`, `identity.SetActive`, `identity.UpdateSettings`) |
| 3 — the caller can discover it | the button is on the page the operator is already looking at, asserted by `TestTheUnmeterToggleButtonNamesTheDirection`; the Credits cell advertises the state |
| 4 — it is used | ADR-0002's request log records the call. Nothing counts how many customers are unmetered, and nothing needs to yet |

## Mutation Log

## Invariants

- The route stays inside the authenticated + admin + same-origin group; the count of unauthenticated
  routes in the process stays at five.
- The new state is never inferred from a posted value.
- `-1` is rendered nowhere. The cell reads `unlimited`.
- Nothing about credits becomes settable: this task adds no control that writes a balance.

## Risks

- A row of five identical secondary buttons is easier to misclick than four. The label names the
  direction and the Credits cell changes visibly, which is the cheapest honest mitigation; a
  confirmation step on every row action is a separate decision and is not taken here.
- ⚠ An administrator can mark their OWN account unmetered. Harmless — an admin has no jobs — but note
  it rather than discovering it: the same table has a self-disable footgun that locks the operator out
  permanently, filed separately and not fixed by this task.

## Stop Condition

Stop if `cmd/router/monitoring_test.go` needs an edit to pass: that test walks the real route table and
asserts only five routes answer without a credential. Needing to touch it means this route was mounted
outside the authenticated group, which is a defect and not a test to update.

## Out of Scope

- A confirmation dialog on row actions. (deferred: `docs/adr/BACKLOG.md`)
- Marking several customers unmetered at once. (deferred: `docs/adr/BACKLOG.md`)
- Refusing self-deactivation, and the lockout it causes. (deferred: `docs/adr/BACKLOG.md`)
- Showing the credit ledger so a balance can be explained. (deferred: `docs/adr/BACKLOG.md`)

## Verification Log
