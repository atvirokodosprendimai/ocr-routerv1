# ADR-0009: Let a customer be unmetered by a flag on the user, not by a sentinel in the balance

**Status:** Accepted
**Date:** 2026-09-28
**Owner:** M
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/0004/0004-editable-customer-settings.md`, `docs/adr/0006/0006-raw-passthrough.md`, `docs/adr/BACKLOG.md`
**Governs:** `internal/core/user.go`, `internal/identity/settings.go`, `internal/router/service.go`, `internal/web/settings.go`, `internal/web/views/views.templ`
**Enforced-by:** None — no standing gate can see this property, and the check that will is ADR-0009-T3's `TestAnUnmeteredDeliveryDebitsNothingAndStillAccrues`, which does not exist until that task lands. It asserts both halves directly against `users.credits` and `jobs.accrued_credits`, which is why the Decision states the falsifying conditions rather than naming a gate that cannot yet see them.
**Invalidates:** ADR-0001 — the clause of its Decision gating admission on a positive balance gains one exemption. ADR-0004's rule that credits are adjusted and never set is NOT touched, and keeping it untouched is the whole reason this is a flag rather than a sentinel.
**Served-path change:** `POST /upload` from a customer marked unmetered succeeds with a zero or negative balance where it previously returned `402 Payment Required`, and their deliveries no longer move the balance.

## Context

M asked for this on 2026-09-15, in his words: *"can make -1 for users ( so unlimited credits )"*. It
has sat in `docs/adr/BACKLOG.md` under **Open** since, as *"Unmetered customers — `-1` credits means
infinite"*, because the request and the existing design conflict and the conflict is not resolvable
in code.

What exists today. Admission is `u.Credits <= 0` (`internal/router/service.go:134`). Every delivery
debits — `job.AccruedCredits` for a units job (`:667`), a flat `1` for a raw job (`:478`) — inside
`Repo.DeliverJob`, which is the only place a job's credits move and which writes the balance
decrement, the `credit_entries` row and the `done→delivered` transition in one transaction
(`internal/store/repo_write.go:292`). A customer who must never be refused therefore has to be
topped up by hand, for ever.

Why `-1` cannot simply be stored. Credits are an **append-only ledger**: `credit_entries` is the
audit of every movement, ADR-0004 made "adjust, never set" explicit, and
`TestCreditsAreNeverSetDirectly` pins it. `-1` is a SENTINEL, not a balance, and adjusting a sentinel
is meaningless — `-1 + 50` is `49`, not "infinite plus fifty". A sentinel in that column would also
be silently destroyed by the delivery path, which subtracts from it, and by the credit-adjust control
an administrator already has. Nothing would report either.

M settled both open questions on 2026-09-28: the flag over the sentinel, and an unmetered job still
accrues a cost for reporting.

## Existing Primitives Audit

- **`core.User.Active` + `identity.SetActive` + `POST /admin/users/{id}/active`** — RESHAPE by
  copying. This is already the shape for a boolean the administrator flips: a column on the user, a
  toggle route that reads the current value SERVER-SIDE and inverts it, and a button whose label is
  the action. ADR-0004 chose that over posting a boolean precisely so a stale page cannot re-assert
  an old state. Unmetered is the same kind of thing and gets the same shape rather than a new one.
- **`Repo.DeliverJob`'s `if charge != 0` guard** (`internal/store/repo_write.go:314`) — REUSE
  unchanged. It already skips both the decrement and the ledger row when the charge is zero, so an
  unmetered delivery needs no new store path: the caller passes 0 and the ledger correctly records
  nothing, because nothing moved.
- **`Repo.UpdateUser`** — REUSE. It writes the admin-settable knobs and deliberately never touches
  credits; `unmetered` is an admin-settable knob and belongs in it, which also keeps the
  never-touches-credits property literally true.
- **`monitor` metric constants + `router`'s duplicated copies** (`internal/router/metrics.go:29`) —
  RESHAPE: one name added at both ends, following the existing comment that the pair is pinned by a
  test rather than imported.
- **`core.ErrNoCredits`** — REUSE. No new sentinel: an unmetered customer simply never reaches it.

## Decision

Add `unmetered` to the user: a boolean column, `INTEGER NOT NULL DEFAULT 0`, surfaced as
`core.User.Unmetered`. It is set only by an administrator, through a toggle that mirrors `Active`
exactly — `identity.SetUnmetered` reads the stored value and writes its inverse, so a stale page
cannot re-assert one.

Three behaviours follow, and they are deliberately three rather than one:

1. **Admission stops refusing them.** `Upload` gates on `!u.Unmetered && u.Credits <= 0`. The
   `Active` check above it is untouched: unmetered is not a way past deactivation, and an
   administrator who wants to stop an unmetered customer still uses the blunt instrument that stops
   every token.
2. **Delivery accrues and does not debit.** `job.AccruedCredits` is computed and stored exactly as
   now — a stage's cost still lands on the job row — and the charge handed to `Repo.DeliverJob`
   becomes `0` for an unmetered owner, for a units job and a raw job alike. So the balance never
   moves, `credit_entries` gains no row for a movement that did not happen, and "what would this
   customer have cost" remains answerable from `jobs.accrued_credits`.
3. **The waived cost gets its own counter.** `ocrr_credits_debited_total` continues to mean *credits
   actually taken from a balance*, and a new `ocrr_credits_waived_total` carries what an unmetered
   delivery would have charged. This is the conflation the backlog entry named: one counter cannot
   honestly answer both "what did we bill" and "what did the work cost", and adding the second is
   cheaper than making the first ambiguous. No labels, so it adds no cardinality.

**What the criterion for correctness is, and what would falsify it.** The claim this record rests on
is *an unmetered customer's balance never moves on delivery, and the cost is still visible*. It fails
if a delivery for an unmetered owner changes `users.credits` by any amount, if it writes a
`credit_entries` row, or if `jobs.accrued_credits` comes back zero for work that had units. Data that
can produce every one of those failures exists in any test database: a user with a balance, a rate
above zero and a completed multi-unit job. That is why `Enforced-by` names one test rather than a
gate — the property is checkable against the two tables directly.

★ **ONE DEVIATION FROM THE LITERAL REQUEST, for M to accept or overrule at review.** M asked for
`-1`, and `-1` is stored nowhere and typed nowhere. The dashboard's Credits column reads `unlimited`
for an unmetered customer instead of a number, and the control is a labelled toggle, not a value an
administrator types. The reason is the one above — a `-1` an operator can type into the existing
credit box is an ADJUSTMENT, so it would land as `balance − 1` and look like it worked. If M wants
`-1` to be the thing he types, that needs a SET control, and a set control is the thing ADR-0004
removed on purpose.

## Alternatives Considered

- **A sentinel `-1` in `users.credits`.** Rejected: it puts a non-balance in a ledger column. The
  delivery path subtracts from that column and the admin credit control adds to it, so the sentinel
  is destroyed by two existing code paths that are both behaving correctly, and neither reports it.
  It also makes every reader of the balance — the dashboard, the admission check, any future report —
  responsible for knowing that one value is magic.
- **A `credits IS NULL` sentinel instead of `-1`.** Rejected for the same reason plus a worse one:
  `NULL` propagates through arithmetic silently, so `credits - charge` becomes `NULL` rather than
  failing, and the column is `NOT NULL` today.
- **A very large credit grant (e.g. one billion) through the existing adjust control.** Rejected: it
  needs no code at all, which is its appeal, and it is a lie in the ledger — an entry claiming a
  billion credits were granted — that also eventually runs out and reads as a real balance to every
  report.
- **A per-customer `metered` boolean defaulting to true.** Rejected: identical mechanics, inverted
  default. `unmetered INTEGER NOT NULL DEFAULT 0` means every existing row and every new row is
  metered without anyone thinking about it, which is the direction a billing default should fail in.
- **Deciding it per TOKEN rather than per customer.** Rejected: `core.User`'s own comment states why
  the metering knobs are properties of the customer — one balance, one buffer limit, one priority —
  and issuing another token must not multiply any of them. An unmetered token would be exactly that
  multiplication, and a leaked one would be unbillable work with no balance to bound it.

## Component / Boundary Impact

| Component | Change | One reason to change? |
|-----------|--------|-----------------------|
| `internal/core` | one field on `User` | Yes — it is the domain's description of a customer |
| `internal/store` | one column, read and written with the existing user row | Yes — persistence |
| `internal/router` | admission condition; charge selection on two delivery paths | Yes — it remains the single writer of job state and the ledger |
| `internal/identity` | one admin-gated setter, mirroring `SetActive` | Yes — it owns who may change what about a user |
| `internal/web` | one toggle route, one button, one cell rendering | Yes — it presents and nothing else |
| `internal/monitor` | one metric name | Yes — observation |

No module moves and no boundary changes, so the architecture map is unaffected. There is no
`docs/architecture.md` in this repository.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| `users` table | `+ unmetered INTEGER NOT NULL DEFAULT 0` (migration `00006_unmetered.sql`) | `internal/store` | `Repo.UserByID`, `Repo.ListUsers`, `Repo.CreateUser`, `Repo.UpdateUser` |
| `core.User` | `+ Unmetered bool` | `internal/core` | router, identity, web |
| `identity.Service` | `+ SetUnmetered(ctx, actor, userID string, unmetered bool) error` | `internal/identity` | `internal/web` |
| HTTP | `+ POST /admin/users/{id}/unmetered` — admin-only, inside ADR-0003's authenticated + same-origin group | `internal/web` | the dashboard |
| metrics | `+ ocrr_credits_waived_total` (no labels) | `internal/router` | `/metrics` |

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `core.User.Unmetered` and its persistence | T1 | T2, T3, T4 | No — a new field, default false, so every existing caller keeps today's behaviour |
| `identity.Service.SetUnmetered` | T4 | none | No — new method |
| `ocrr_credits_waived_total` | T3 | T5 (documents it) | No — new series |

## Implementation

See `tasks/README.md`. Five tasks, sequential enough to list in order rather than in waves.

## Consequences

- **Positive:** a customer who must never be refused stops needing a standing manual top-up, and the
  mechanism says what it means — a flag named `unmetered` rather than a number that means something
  else when it is negative.
- **Positive:** the ledger stays an audit. No row is written for a movement that did not happen, and
  `TestCreditsAreNeverSetDirectly` keeps holding.
- **Negative:** one more admin-settable property, and one more state the dashboard has to render
  legibly. An administrator can now make work free by clicking one button, which is a real
  capability and is why it is admin-gated and, like every other control on that table, recorded only
  by ADR-0002's request log — an audit trail for administrative actions is still deferred.
- **Negative:** `Deliver` and `DeliverRaw` each gain a user read they did not need before. One extra
  indexed row read per delivery, on a path that already runs a multi-statement transaction.
- **Neutral:** `ocrr_credits_debited_total` becomes narrower in meaning, not different: it always
  counted actual debits, and now there is a second series for the rest. A dashboard summing only the
  first will under-report total consumption for unmetered customers, which is the honest answer.

## Out of Scope

- Any control that SETS a balance to a value. (permanent: boundary: ADR-0004 removed set-to controls on purpose, and this record's whole argument is that a ledger has movements rather than assignments)
- Storing or accepting `-1` as a balance anywhere. (permanent: boundary: it is a sentinel in a ledger column that two correct code paths already destroy — see Alternatives)
- Per-token unmetered status. (permanent: boundary: `core.User`'s metering knobs are per-customer by design, and a per-token exemption would multiply them)
- A quota or cap on an unmetered customer — "unlimited but not more than N per month". (deferred: `docs/adr/BACKLOG.md`)
- An audit trail of who marked a customer unmetered and when, beyond ADR-0002's request log. (deferred: `docs/adr/BACKLOG.md`)
- Showing a customer's credit ledger (`Repo.Ledger`) anywhere in the dashboard, which is what would make a balance explainable. (deferred: `docs/adr/BACKLOG.md`)

## Risks

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| The flag is set on the wrong customer and work becomes free silently | Med | Med | The toggle sits in the customer's own row beside `Disable`, its label names the action, and the Credits column changes to `unlimited` — a visible state rather than an invisible one. T4 asserts the rendering. |
| A later reader "simplifies" the admission check back to `u.Credits <= 0` | Med | High | T2's test is named for the property and fails on exactly that edit; the mutation log will carry the kill. |
| A future delivery path is added and forgets the waiver | Med | High | T3 changes BOTH existing delivery paths in one task and its test table covers both, so the pattern is visible at the only two call sites of `Repo.DeliverJob`. |
| Someone reads `ocrr_credits_debited_total` as total consumption | High | Low | The new series is named in the README (T5) beside it, and the ADR states the narrowing. |
| A migration on a live database | Low | Med | `ADD COLUMN` with `NOT NULL DEFAULT 0` is the same shape as `00004` and `00005`; the goose Down drops it. |

## Rollback

`goose down` on `00006_unmetered.sql` drops the column; the code changes are additive and revert
cleanly with the commits. **Order matters on a live database:** revert the binary first, then the
migration — a running binary whose `userColumns` names a dropped column fails every user read, which
is every authenticated request. Nothing else persists: no `credit_entries` row is written by this
feature, so there is no data to reconcile after a revert, and any customer who was unmetered simply
becomes metered again with their balance exactly as the ledger left it.

## Follow-ups

- [ ] M to accept or overrule the one deviation marked ★ in the Decision: `-1` is not stored and not
      typed; the control is a toggle and the column reads `unlimited`.
