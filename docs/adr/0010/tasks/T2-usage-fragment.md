# Task ADR-0010-T2: Render the counters as their own live fragment, and make the Customers page live

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `views.Dashboard.Usage`, the `#usage` fragment
**Consumes:** `core.Usage`, `Repo.UsageByUser(ctx, now)` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the read model carrying usage`, `the fragment's own id`, `the Customers page subscription`, `the user table NOT being patched`, `the per-number labelling`

## Goal

The Customers page shows each customer's four windows, updates them over the existing SSE stream, and
leaves the editable table alone while doing it.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/web/views/model.go` | edit | `Dashboard.Usage map[string]core.Usage` — the read model is the one source both the first paint and every patch read |
| `internal/web/web.go` | edit | `buildDashboard` calls `UsageByUser`; `stream`'s `push` patches `views.Usage` — this is what SELECTS T1's method, which is unreachable until this line exists |
| `internal/web/views/usage.templ` | add | the `#usage` fragment: one row per customer, four window columns |
| `internal/web/views/views.templ` | edit | the Customers page renders `@Usage(d)` and gains `data-init="@get('/admin/stream')"` |
| `internal/web/views/views_templ.go` | regenerate | `templ generate`; never hand-edited |
| `internal/web/views/assets/app.css` | edit | the usage table's compact cell |
| `internal/web/usage_test.go` | add | the rendering, the labelling, the subscription, and what must NOT be patched |

## Ordered Steps

1. [S1] Write the failing tests first: `/admin/users` renders a `#usage` section containing each
   customer's numbers, and carries a `data-init` subscription. Red — neither exists.
2. [S2] Add `Usage` to `views.Dashboard` and fill it in `buildDashboard`. ⚠ In `buildDashboard`, not in
   the handler: a field filled on the page-load path and not the patch path renders once and then
   blanks itself on the first SSE event.
3. [S3] Write `views.Usage(d)` with root id `usage`. ⚠ A fragment cannot be patched into existence —
   the id must be in the first paint — and sixteen numbers per customer has to stay legible narrow, so
   each window is ONE cell: the pushed count, then delivered / failed / expired beneath it, muted.
4. [S4] Label every number for a reader who cannot see the column group. Each cell carries a `title`
   naming the window and the four values in words; the counts are not bare digits whose meaning lives
   only in a header two rows up. [proof: human: an operator reads one cell and can say which window and which bucket each number belongs to, without scrolling to the header]
5. [S5] Render `@Usage(d)` on the Customers page and add the `data-init` subscription it has never
   had — the page is not live today, so the counters would otherwise be live in a stream nobody joined.
6. [S6] Patch `views.Usage` from `stream`'s `push`, beside `Stats`, `Jobs` and `Workers`.
   ⚠ **DO NOT PATCH `#user-table`.** That fragment carries every row's number inputs, and re-rendering
   it every fifteen seconds would land under an operator who is typing in one — see the ADR's decision
   3 and the stale-signal defect it names.

## Acceptance

```bash
set -o pipefail
templ generate && go test ./internal/web/ -run 'Usage' -count=1 2>&1 | tee /tmp/adr10t2.out && \
  ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/adr10t2.out && \
  go test ./internal/web/... ./cmd/router/ -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestUsageSectionRendersEveryCustomer` | `internal/web/usage_test.go` | the fragment exists with id `usage` and carries each customer's pushed count — red if the read model is filled on the page path only | — | S1, S2, S3 |
| `TestUsageNumbersAreLabelledNotBare` | `internal/web/usage_test.go` | each cell names its window and its four buckets in words, so a number is never a digit whose meaning is two rows away | — | S4 |
| `TestTheCustomersPageSubscribesToTheStream` | `internal/web/usage_test.go` | the Customers page carries `data-init` with the stream URL — without it every counter is live in a stream nobody joined | — | S5 |
| `TestTheStreamPatchesUsage` | `internal/web/usage_test.go` | a stream connection receives a `usage` fragment, so "live" is observed rather than asserted | — | S6 |
| `TestTheStreamDoesNotPatchTheUserTable` | `internal/web/usage_test.go` | ⚠ the NEGATIVE that protects an operator mid-edit: no patch names `user-table`. Red for the obvious implementation, which is to patch the table the counters are about | — | S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestUsageSectionRendersEveryCustomer` |
| 2 — something selects it | `buildDashboard`'s call to `UsageByUser` and `push`'s call to `views.Usage` — this task is what discharges T1's rung-2 debt; the mutation deletes the `push` line and `TestTheStreamPatchesUsage` must go red |
| 3 — the caller can discover it | the section is on the page the operator already opens, and its heading names the windows |
| 4 — it is used | nothing measures whether the numbers are read; ADR-0002's request log shows the page being loaded |

## Mutation Log

## Invariants

- `#user-table` is never patched by the stream.
- `Dashboard.Usage` is filled in `buildDashboard`, so the first paint and every patch agree.
- The `usage` id exists in the first paint.
- No new route: the counters ride the existing `/admin/stream`.

## Risks

- Sixteen numbers per customer is the readability risk, and a test can only pin the labelling, not the
  legibility. S4's proof is human for that reason and says what the person checks.
- Making the Customers page live is a behaviour change to a page that has never updated itself. If an
  operator relied on it being static while editing, the separate fragment is what keeps that true for
  the part they edit.

## Stop Condition

Stop if the counters cannot be rendered without patching `#user-table` — that would mean the row data
and the usage data are coupled in the view in a way the ADR assumed they are not, and the decision
needs revisiting rather than the invariant being dropped.

## Out of Scope

- Sorting or filtering the usage table. (deferred: `docs/adr/BACKLOG.md`)
- Charts. (deferred: `docs/adr/BACKLOG.md`)
- Fixing the stale-signal defect on the row inputs — this task avoids it, it does not repair it. (deferred: `docs/adr/BACKLOG.md`)

## Verification Log
