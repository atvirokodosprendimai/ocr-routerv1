# ADR-0010: Count each customer's work over four windows, from the job rows already kept

**Status:** Proposed
**Date:** 2026-09-28
**Owner:** M
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/tasks/T10-admin-dashboard.md`, `docs/adr/tasks/T11-monitoring.md`, `docs/adr/0004/0004-editable-customer-settings.md`, `docs/adr/0007/0007-failure-detail.md`, `docs/adr/BACKLOG.md`
**Governs:** None — declared by its tasks. Every file this decision owns is created by T1 and T2, so naming them here before they exist would declare authority over paths that resolve to nothing.
**Enforced-by:** None — no standing gate can see these properties, and the check that will is ADR-0010-T1's `TestUsageCountsOnlyTheWindowAndOnlyTheOwner`, which does not exist until that task lands. It asserts the two things nothing else can: that a count includes only its own owner's jobs, and that a job one second outside a window floor is not counted. The Decision states the falsifying conditions rather than naming a gate that cannot yet see them.
**Invalidates:** none — checked. ADR-0001 task T11's cardinality allow-list is respected rather than changed: nothing here adds a metric or a label, and per-customer visibility stays on the authenticated dashboard exactly as that task decided.
**Served-path change:** An administrator opening `/admin/users` sees, per customer, how many jobs were pushed in the last 24 hours, 7 days and 31 days and in the previous calendar month, and how many of each cohort ended delivered, failed or expired — and those numbers update over the existing SSE stream without a reload.

## Context

M, 2026-09-28: *"would be nice to have counter forclient, 24h, 7d 31d past month counters, how much files it pushed/succes, error etc."*

**The data is already there, and that is the whole reason this is small.** Nothing deletes job rows:
the reaper removes blobs (`internal/router/reaper.go:74`) and swept results, never rows. So `jobs`
holds `user_id`, `state`, `created_at`, `units`, `size_byte` and `accrued_credits` for every job the
system has ever accepted. This record needs **no new table, no retention policy and no time series** —
it needs one index and one aggregate query.

That also means the adjacent backlog entry is only PARTLY taken up. *"Charts and historical analytics
on the admin dashboard"* (deferred by ADR-0001 task T10) says *"nothing retains a time series, so 'was
it like this yesterday' has no answer"*. That is still true of CHARTS, and now false of COUNTS: a count
over a window is a query, where a chart is a series of them at retained resolution. The entry stays
open for the charting half.

Where it does NOT go: ADR-0001 task T11 froze the metric label allow-list to `{label, state, action}`
and its record says outright that *"no metric carries user_id, job_id or email"*, enforced both at
registration and by a test that parses the rendered `/metrics` output. Per-customer usage is exactly
the unbounded-label case that guard exists for, and `/metrics` is loopback-only besides. The dashboard
is where this belongs, which is what T11 already said.

## Existing Primitives Audit

- **`jobs.created_at` + `jobs.state`** — REUSE, unchanged. Every number this record renders is a
  count over those two columns for a `user_id`. No column is added anywhere.
- **`Repo.CountJobsByState`** — RESHAPE by analogy, not by extension. It answers "the census right
  now" for the whole system with no time bound and no owner; this needs per-owner, per-window, and
  returning both from one call. Widening that method to take windows and an owner would make one
  function answer two unrelated questions.
- **`Web.buildDashboard`** — REUSE as the single read model. It is already the one function of the
  world for both the first paint and every SSE patch, so the counters join it rather than growing a
  second path that could disagree with it.
- **`views.Dashboard` + the fragment-per-id pattern** — REUSE. `Stats`, `Jobs` and `Workers` are each
  a fragment with a stable root id patched by the stream; `Usage` becomes a fourth of exactly that
  shape.
- **`idx_jobs_user_state`** (`user_id, state`) — NOT usable here. It cannot bound a scan by time, and
  the query filters on `created_at` first. One new index, `jobs(created_at)`.
- **`credit_entries`, indexed on `(user_id, created_at)`** — considered as the source and REJECTED:
  it records money, so it holds one row per DELIVERY and nothing for a job that failed, expired or was
  never collected. Three of the four buckets would be invisible.

## Decision

Add `Repo.UsageByUser(ctx, now)` returning `map[string]core.Usage` — every customer, all four windows,
all four buckets, from **ONE aggregate query**. `buildDashboard` calls it, so the counters are part of
the same read model the first paint and every SSE patch already share. A new `Usage` fragment renders
them on the Customers page, which gains the `data-init` subscription it has never had.

The four windows, computed from the caller's `now` so tests are deterministic:

| column | from | to |
|---|---|---|
| 24h | `now - 24h` | `now` |
| 7d | `now - 7d` | `now` |
| 31d | `now - 31d` | `now` |
| previous month | first instant of the previous calendar month, UTC | first instant of this month, UTC |

Four buckets per window: **pushed** (a job row created in the window), **delivered**, **failed**
(`dead` — the command exhausted its attempts) and **expired** (the deadline passed while queued, so
nothing ever ran). `dead` and `expired` are kept apart because an operator acts on them differently:
one is a failing command, the other is a service nobody served.

**What would falsify this.** The claim is *each number counts exactly the jobs of one owner whose
`created_at` falls in that window, and the four buckets reconcile*. It fails if a count includes
another customer's job, if a job one second outside a boundary is counted, or if
`delivered + failed + expired` ever exceeds `pushed` for the same window. A test database can produce
all three, which is why `Enforced-by` names a test rather than a gate.

### Four calls that are MINE, not M's — flagged for review

1. ★ **Every bucket is keyed on `created_at`, including the terminal ones.** A window's numbers are a
   COHORT: "of the jobs pushed in this window, how many ended delivered / failed / expired", and
   `pushed − delivered − failed − expired` is what is still in flight. The alternative — bucketing
   terminal states by `updated_at` — answers "how many finished during this window" and makes the four
   numbers stop reconciling, because a job pushed on Monday and delivered on Tuesday would land in two
   different windows. Cohort counting is what the preview M approved showed.
2. ★ **"Past month" is the PREVIOUS COMPLETE calendar month, in UTC** — August, when today is in
   September. M's chosen option was labelled "previous calendar month" while its preview happened to
   name the current one; rolling 31d already answers "roughly the last month", so the column that
   earns its place is the completed one an invoice matches. Flipping it to current-month-to-date is a
   one-line change to the window arithmetic. UTC because every timestamp in this schema is unix
   seconds and the system has no operator timezone.
3. ★ **The counters get their OWN fragment; the editable Customers table is NOT patched.** M asked for
   live counters, and the obvious implementation — patch `#user-table` — would re-render every row's
   number inputs every fifteen seconds under an operator who is typing in one. That table carries the
   per-row buffer-limit, priority, TTL, credit-delta and reason controls, and a known defect already
   has bound inputs refilling from stale signals after a morph. A separate `#usage` fragment gets the
   liveness M asked for and touches nothing being edited.
4. ★ **One query, not four per customer.** M chose live-in-the-stream over my recommendation of
   page-load-only, so the query shape stops being a style question: 4 windows × 4 buckets × N
   customers as separate counts would be 16N statements every fifteen seconds. One `GROUP BY user_id`
   with conditional sums is a single pass, and the index bounds that pass to recent rows.

### The cost, stated rather than assumed

Every SSE push runs one aggregate over the job rows newer than the OLDEST window floor — which is the
previous month's start, so up to about 62 days of jobs. It does not grow with total history. That is
the bound this design buys with its index; when it stops being cheap the answer is a rollup table, and
that is filed as a deferred entry rather than pre-built.

## Alternatives Considered

- **A `job_stats` rollup table updated on every transition.** Rejected for now: it is a second source
  of truth for numbers that are one query away, it has to be backfilled, and it can drift from `jobs`
  with nothing reporting the drift. It is the right answer once the window scan is measurably slow,
  and it is deferred on exactly that condition.
- **Prometheus counters labelled by customer.** Rejected, and not a close call: ADR-0001 T11 froze the
  label allow-list precisely because a user id is unbounded, `/metrics` is loopback-only, and a
  guard test parses the rendered output to catch it.
- **Page-load only, no SSE (my recommendation).** M chose live. Recorded because the cost paragraph
  above is the consequence, and a future session wondering why a query sits on the push path should
  find the decision rather than re-derive it.
- **Counting from `credit_entries`.** Rejected: it holds one row per delivery, so failed, expired and
  uncollected jobs — three of the four buckets — leave no trace in it.
- **Bucketing terminal states by `updated_at`.** Rejected; see call 1. It answers a different
  question and the four numbers stop reconciling.
- **Also exposing `GET /usage` to the customer.** Not rejected on merit — M chose dashboard-only, and
  it would be a new public contract needing its own scoping and rate-limit decision. Deferred.

## Component / Boundary Impact

| Component | Change | One reason to change? |
|-----------|--------|-----------------------|
| `internal/core` | a `Usage` value type | Yes — the domain's vocabulary |
| `internal/store` | one index, one aggregate read | Yes — persistence |
| `internal/web` | the read model gains a field; one new fragment; the stream patches it | Yes — it presents |
| `internal/web/views` | a `Usage` component | Yes — rendering |

No module moves, no boundary changes, and there is no `docs/architecture.md` in this repository.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| `jobs` index | `+ idx_jobs_created_at ON jobs(created_at)` (migration `00007_usage_index.sql`) | `internal/store` | `Repo.UsageByUser` |
| `core.Usage` | new value type: four windows × four counts | `internal/core` | `internal/store`, `internal/web` |
| `store.Repo` | `+ UsageByUser(ctx, now) (map[string]core.Usage, error)` | `internal/store` | `Web.buildDashboard` |
| `views.Dashboard` | `+ Usage map[string]core.Usage` | `internal/web` | `views.Usage` |
| HTML | `+ <section id="usage">`, patched by the existing `/admin/stream` | `internal/web/views` | the dashboard |
| HTML | the Customers page gains `data-init="@get('/admin/stream')"` | `internal/web/views` | the dashboard |

No HTTP route is added, no schema column, and no metric.

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `core.Usage` and `Repo.UsageByUser` (T1) | T1 | T2 | No — new type and new method |
| `views.Dashboard.Usage` and the `#usage` fragment (T2) | T2 | T3 | No — new field, new fragment |

## Implementation

See `tasks/README.md`. Three tasks, sequential.

## Consequences

- **Positive:** the operator can answer "who is pushing all this work, and is it working for them"
  without SQL, and the failure columns put ADR-0007's failure data next to the customer it happened to.
- **Positive:** no schema column, no rollup, no retention policy, and no new route — because the job
  rows were already kept.
- **Negative:** one aggregate query on the SSE push path, every fifteen seconds per connected
  administrator, bounded by ~62 days of job rows. This is the cost of live counters and it is the one
  number to watch as the system gets busier.
- **Negative:** four windows × four buckets is sixteen numbers per customer. The rendering has to stay
  legible on a narrow screen, which is a real constraint on T2 rather than a detail.
- **Neutral:** the Customers page becomes live, which it never was. That closes one of the dashboard
  audit findings of 2026-09-28 as a side effect rather than as its own work.

## Out of Scope

- Charts, or any time series at retained resolution. (deferred: `docs/adr/BACKLOG.md`)
- A `job_stats` rollup table. (deferred: `docs/adr/BACKLOG.md`)
- `GET /usage` for a customer to read its own counters. (deferred: `docs/adr/BACKLOG.md`)
- Bytes and credits per window, beside the counts. (deferred: `docs/adr/BACKLOG.md`)
- Per-service breakdown of a customer's usage. (deferred: `docs/adr/BACKLOG.md`)
- An operator timezone for the calendar-month boundary. (permanent: boundary: every timestamp in this schema is unix seconds and nothing anywhere carries a timezone; introducing one for a single column would make one number disagree with every other date in the system)
- Any per-customer metric or metric label. (permanent: fact: ADR-0001 task T11 froze the label allow-list to label/state/action and a test parses the rendered output to enforce it; citation: file `internal/monitor/registry.go:32`)

## Risks

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| The window scan becomes slow as `jobs` grows | Med | Med | The index bounds it to ~62 days rather than all history; the rollup alternative is filed with the condition that triggers it. T1 asserts the query is ONE statement, so the cost cannot silently become 16N. |
| A boundary off by one — `>=` vs `>` on a window floor | High | Low | T1 tests a job exactly ON each floor and one second outside it. This is the defect this shape invites and the cheapest to pin. |
| Counting another customer's jobs | Low | High | `Enforced-by` names the test; two customers with overlapping jobs are in the fixture, because a single-customer fixture cannot see the defect. |
| Patching the user table under an operator's fingers | — | — | Avoided by construction: the counters are their own fragment and `#user-table` is not patched. Called out as decision 3 rather than left implicit. |
| Sixteen numbers per row becoming unreadable | Med | Med | T2 owns the rendering and its acceptance asserts the labelling, not just the numbers. |

## Rollback

`goose down` on `00007_usage_index.sql` drops the index; the code changes are additive and revert with
the commits. Nothing persists: this record writes no data, so there is nothing to reconcile after a
revert. **Order on a live database:** revert the binary first, then the migration — dropping the index
while the query still runs makes it slow, not wrong, so the order is a performance matter here rather
than a correctness one.

## Follow-ups

- [ ] M to confirm call 2: "past month" is the PREVIOUS complete calendar month (August, in
      September), not the current month to date. One line either way.
