# Task ADR-0007-T4: Give failures a place to be seen, and make the detail readable

**Depends-on:** T3
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `GET /admin/jobs?state=failed`, the exit code on the job row, readable detail
**Consumes:** `jobs.exit_code` populated end to end (T3)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the filter selecting only failures`, `the exit code being rendered distinctly from no-exit`, `the filter reading the same model as the dashboard`

## Goal

An administrator can list only failing jobs, see each one's exit code, and read
what the command actually printed without the table collapsing.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/web/web.go` | edit | The `state=failed` filter over `buildDashboard`'s existing rows, and the route — the line that SELECTS this view |
| `internal/web/views/views.templ` | edit | An `Exit` column, readable Detail, and the filter control; `templ generate` after, never editing `*_templ.go` |
| `internal/web/views/model.go` | edit | `JobRow` gains a rendered exit-code string, so the template holds no formatting logic |
| `internal/web/views/assets/app.css` | edit | The Detail cell wraps and bounds rather than stretching the table |
| `internal/web/failures_test.go` | add | The failing tests below |

## Ordered Steps

1. [S1] Write the failing tests: `TestFailedFilterShowsOnlyFailures`, `TestExitCodeIsShownDistinctlyFromNoExit`, `TestFailuresViewUsesTheSameReadModel`. Confirm red. [proof: acceptance]
2. [S2] Add the filter to the existing dashboard handler, as a FILTER over
   `buildDashboard`'s rows. ⚠ Not a second query: that function is already the one function of the
   world for the page load and every SSE patch, and a second one would be a second thing to keep
   correct with the first divergence invisible.
3. [S3] Render `Exit` as the code, or as `—` when there is none. The two must LOOK different: a
   timeout showing `0` would read as a clean exit, which is the exact confusion T1's nullable column
   exists to prevent.
4. [S4] Make the Detail cell readable — wrapped and bounded — so a 2000-byte stream does not
   stretch the table. The full text stays reachable rather than being truncated away.
5. [S5] Add the control that reaches the filter: a datastar `data-on:click` to the filtered route, **no `<form>` tag**, per the team's standing datastar decision. A view nothing links to is a view nobody finds. [proof: human: the reviewer loads /admin and reaches the failures list by clicking, without typing a URL]
6. [S6] Run `templ generate` and confirm `*_templ.go` was regenerated rather than hand-edited.
   [proof: acceptance]

## Acceptance

```bash
set -o pipefail
templ generate \
  && go test ./internal/web -run 'TestFailedFilterShowsOnlyFailures|TestExitCodeIsShownDistinctlyFromNoExit|TestFailuresViewUsesTheSameReadModel|TestDetailDoesNotBreakTheTable' -count=1 -v 2>&1 | tee /tmp/acc-0007-T4.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0007-T4.out \
  && go test ./internal/web/... ./internal/store/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestFailedFilterShowsOnlyFailures` | `internal/web/failures_test.go` | With a mix of delivered and dead jobs rendered, the filtered page lists the failures and NOT the successes — both halves asserted, or "shows failures" is satisfied by a page showing everything | — | S2 |
| `TestExitCodeIsShownDistinctlyFromNoExit` | `internal/web/failures_test.go` | A job that exited 3 renders `3`; a timeout renders the no-exit marker and NOT `0` — the distinction the nullable column exists for, carried to the one place a human reads it | — | S3 |
| `TestFailuresViewUsesTheSameReadModel` | `internal/web/failures_test.go` | A job appearing on the dashboard appears in the filter with the same state, cost and detail — a second query would drift and this is what notices | — | S2 |
| `TestDetailDoesNotBreakTheTable` | `internal/web/failures_test.go` | A 2000-byte detail renders inside a bounded cell — asserted on the markup that bounds it, not on the eye | — | S4 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestFailedFilterShowsOnlyFailures` |
| 2 — something selects it | The route and its handler; removing the filter makes the failed page identical to the dashboard and `TestFailedFilterShowsOnlyFailures` red — the mutation to record |
| 3 — the caller can discover it | The control added in S5. A filtered route reachable only by typing a URL is a view nobody finds, which is this ADR's version of the finished-and-unreachable defect |
| 4 — it is used | Nothing measures dashboard usage, and nothing should for this |

## Mutation Log

## Invariants

- The failures view is a FILTER over `buildDashboard`, never a second query.
- A missing exit code never renders as `0`.
- No `<form>` tag; `data-on:` and `data-bind:` per the team's datastar decision.
- `*_templ.go` is generated, never edited.
- Worker-supplied text stays a TEXT NODE and never reaches a `data-*` attribute —
  the existing comment at `views.templ:69` says why, and this task renders more of
  that text than before.

## Risks

- ⚠ This page now shows MORE worker-supplied text than before (stdout as well as
  stderr, per T2). The audience is unchanged — admin-only, already authenticated
  — and the budget is unchanged, but the existing escaping rule becomes more
  load-bearing, not less. `views_test.go` already carries a hostile-input case;
  it must keep covering the widened text.
- A filter parameter that reaches a query is an injection surface. It selects
  among fixed states in Go, never interpolated into SQL.

## Stop Condition

Stop if `buildDashboard` turns out not to carry enough rows for a useful failures
list — it caps at 50 recent jobs of all states, so a busy queue could push every
failure off the end. If that is real, the fix is a paging or query decision the
owner should take, not a cap this task quietly raises.

## Out of Scope

- Per-service failure counts on the Services table (deferred: `docs/adr/BACKLOG.md`).
- Alerting on failures (deferred: `docs/adr/BACKLOG.md`).
- Showing failure detail to the CLIENT (permanent: boundary: ADR-0001 keeps worker text out of the client's result contract).

## Verification Log
