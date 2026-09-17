# ADR-0008: Stop charging a worker restart against a job's retry budget

**Status:** Proposed
**Date:** 2026-09-17
**Owner:** M
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/0006/0006-raw-passthrough.md`, `docs/adr/0007/0007-failure-detail.md`
**Governs:** `internal/router/service.go`, `internal/router/reaper.go`, `internal/store/repo.go`, `internal/store/repo_write.go`, `internal/store/migrations/**`, `internal/httpapi/claim.go`, `internal/agent/**`, `cmd/worker/main.go`, `cmd/router/main.go`, `cmd/router/wire.go`, `internal/core/job.go`

<!-- Class: every place a job's retry budget is spent or its lease is given up.
Enumerated 2026-09-17 with
`git grep -ln 'Attempts|attempts|LeaseExpired|lease_expires_at|failJob|MaxAttempts' -- '*.go' '*.sql'`
→ 13 files, of which FOUR are a different "attempts" entirely and are excluded:
internal/web/login.go and internal/web/web.go are ADR-0003's LOGIN attempts, and
internal/bus/bus.go and internal/core/core.go only mention the word in comments.
The remaining nine are this decision's, plus internal/httpapi/claim.go and
cmd/worker/main.go which the release path adds. -->

**Enforced-by:** None — the check is `TestReclaimDoesNotSpendAnAttempt`, created by T2; naming it before it exists would be the pointer-to-nothing this header exists to prevent. Set this to `internal/router/reclaim_test.go::TestReclaimDoesNotSpendAnAttempt` when T2 lands.
**Invalidates:** ADR-0001 — one clause, narrowed rather than removed: §Decision's retry model treats every requeue as an attempt against `max-attempts`. A lease reclaimed because a worker vanished now spends a SEPARATE budget; a failure the work itself caused is unchanged.
**Served-path change:** Restarting a worker mid-job requeues that job immediately instead of stalling its client for the full lease, and no longer counts against the job's retry budget — today three restarts kill a customer's job with "lease expired".

## Context

M, 2026-09-17: *"i'v restart in the middle of process worker, and client hanged in waiting"*, and in the same breath *"also need 3 retries if exit code not 0"*.

Measured against the running deployment, 2026-09-17 08:11:

- Job `01a0adc6` sat `processing`, claimed 08:10:42, lease until **08:15:42**. The
  client was not hung; it was waiting out `--lease`, which defaults to 5 minutes.
  The reaper runs every 30s but will not touch a live lease, by design.
- **Three retries already happen.** `--max-attempts` defaults to 3, every failure
  reaches one path (`Service.Fail` → `failJob`), and all 11 failed rows in that
  database show `attempts=3`. A non-zero exit is already retried; nothing needs
  building for that half, and this record exists to say so rather than let it be
  re-implemented.

What neither of those covers is the defect underneath both. **A reclaimed lease
spends an attempt** (`internal/router/reaper.go:48` calls `failJob` with
`"lease expired"`). So a worker restarted three times during a deploy kills the
job it was holding, with a reason that blames the job for the operator's action,
and burns the budget that exists for genuine failures. The person most likely to
restart a worker three times is the person developing against it.

Worse, the information needed to avoid the wait already exists at the worker:
`cmd/worker/main.go:72` catches SIGINT and SIGTERM, so on a deliberate restart
the worker KNOWS it is going down while it is still connected — and says nothing.

## Existing Primitives Audit

| Primitive | Disposition |
|-----------|-------------|
| `jobs.attempts` + `failJob`'s `attempts+1 < MaxAttempts` | **Reuse unchanged** for failures the work caused. This ADR stops a second, different event from spending it. |
| The lease (`jobs.lease_expires_at`, `holdsLease`, `LeaseExpired`) | **Reuse unchanged.** Expiry remains the safety net for a worker that dies without warning; this ADR only adds a faster, cooperative path beside it. |
| `jobs.expires_at` — the per-customer job TTL | **Reuse as the global backstop.** It already bounds how long a job can churn whatever requeues it, which is why the release path below needs no budget of its own. |
| `signal.NotifyContext` in `cmd/worker` | **Reuse.** Graceful shutdown is already detected; nothing acts on it. |
| `Service.Claim` / the worker-only route group | **Reuse.** The release endpoint is worker-only and lease-guarded exactly as claim and completion are. |
| ADR-0007's `exit_code` and failure detail | **Untouched.** That record is about WHY a command failed; this one is about who pays for a requeue. They meet only in `failJob`, and T2 keeps them separate. |

## Decision

**A requeue has two causes, and they stop sharing one counter.**

`jobs.attempts` keeps its meaning: how many times the WORK was tried and failed.
A new `jobs.reclaims` counts how many times a lease was taken back because a
worker stopped answering. The reaper spends `reclaims`, never `attempts`, and a
job dies when either budget is exhausted — `--max-attempts` (default 3) for
failures, `--max-reclaims` (default 3) for abandonment.

**Both budgets are kept, and that is the point.** Removing the bound on
reclamation would be the easy version and the wrong one: a job that reliably kills
its worker — an OOM on one enormous document — is a poison pill, and unbounded
requeue turns it into an infinite loop that takes every worker down in turn. What
changes is not that abandonment is free, but that it is no longer charged to the
budget reserved for the job's own failures.

**A worker that is shutting down hands its leases back.** `POST /release?job_id=`
is worker-only and lease-guarded, and requeues the job immediately, spending
NEITHER budget. A cooperative release is not evidence of anything wrong: the
operator stopped the worker. The job returns to the queue and the restarted
worker picks it up in seconds instead of minutes.

**Release spends nothing, and needs no budget of its own.** A worker restarted in
a loop could in principle release the same job forever — but that requires a human
repeatedly restarting it, and `jobs.expires_at` already bounds the job's total
lifetime regardless of what requeues it. Adding a third counter to bound a loop a
person has to drive by hand would be machinery for a failure nobody has.

**The falsifying case, and whether data exists to produce it:** the claim is that
abandonment and failure draw on different budgets. It fails if a reclaimed job's
`attempts` moves, or a failed job's `reclaims` does — both constructible today
with the existing harness (claim, then either expire the lease or report a
failure, then read the row), which is what T2's tests do. Valid for the
single-router deployment ADR-0001 describes; a multi-router future would need the
reaper's reclaim to stay idempotent, which it already is.

## Alternatives Considered

- **Shorten `--lease`.** No code at all; the operator sets `--lease 30s` in
  development. Rejected as the whole answer, and recorded because it is the right
  IMMEDIATE workaround and was given as one: a short lease trades the stall for a
  real risk, because a lease shorter than the longest legitimate command causes
  the reaper to requeue work that is still running, and then two workers process
  the same job. It treats the symptom at the cost of the guarantee.
- **Let the reaper requeue without any bound.** Simplest. Rejected: it turns a
  poison-pill job into an infinite loop across every worker, which is strictly
  worse than the current defect because it has no terminal state.
- **Reclaim a worker's leases when its SSE stream drops**, with no new endpoint.
  Rejected, and it is the tempting one: a stream can drop transiently while the
  subprocess is still running, so the job would be requeued and run TWICE — the
  exact double-execution the lease exists to prevent. A release must be an
  explicit statement, not an inference from a disconnect.
- **Have the worker report a failure on shutdown** (`"worker restarting"`), reusing
  the existing report path with no new route. Rejected: it consumes an attempt,
  which is the defect this record is about, and it writes a failure into
  `last_error` for a job that never failed.
- **Track reclaims in memory rather than a column.** No migration. Rejected: the
  count must survive the router restart that is one of the causes of abandonment
  in the first place.

## Component / Boundary Impact

| Component | Ownership after change | One reason to change? |
|-----------|------------------------|-----------------------|
| `internal/store` | Owns `jobs.reclaims` | Yes — persistence |
| `internal/router` | Owns which budget a requeue spends, and the release transition | Yes — job-state rules |
| `internal/httpapi` | Owns the worker-only release route | Yes — HTTP surface |
| `internal/agent` / `cmd/worker` | Releases what it holds when it is told to stop | Yes — worker behaviour |
| `cmd/router` | Owns `--max-reclaims` | Yes — operator configuration |

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| `jobs.reclaims` (schema) | new integer column, default 0 | migration `00005` | `internal/store`, `internal/router` |
| `core.Job.Reclaims` | new field | `internal/core` | `internal/router`, `internal/web` |
| `Repo.ReclaimJob(ctx, id, reason, now)` | new: requeues and increments `reclaims`, never `attempts` | `internal/store` | `internal/router` |
| `POST /release?job_id=<id>` | new worker-only route → `204`; `409` without the lease | `internal/agent` | `internal/httpapi/claim.go` |
| `Service.ReleaseLease(ctx, workerID, jobID, now)` | new | `internal/router` | `internal/httpapi` |
| `--max-reclaims` (default 3) | new router flag → `router.Config.MaxReclaims` | `cmd/router` | `internal/router` |
| ADR-0002 transition log | `"lease expired"` lines carry `reclaims`; release lines carry actor `worker` and reason `released` | `internal/router` | operators |

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `jobs.reclaims` + `core.Job.Reclaims` + `Repo.ReclaimJob` (T1) | T1 | T2, T3 | No — additive |
| `Service.ReclaimJob` bounded by `MaxReclaims` (T2) | T2 | T3 | No |
| `POST /release` + `Service.ReleaseLease` (T3) | T3 | none | No — new route beside the existing ones |

## Implementation

See `tasks/README.md` — 3 tasks, sequential.

## Consequences

- **Positive:** restarting a worker no longer costs a customer's job an attempt,
  and three restarts no longer kill it.
- **Positive:** a deliberate restart requeues in seconds rather than after the
  full lease, so the client's wait matches what the operator actually did.
- **Positive:** "this job keeps killing workers" and "this job keeps failing"
  become distinguishable in the data, which they are not today — both read as
  `attempts`.
- **Negative:** a second budget is a second thing to reason about, and a job can
  now die for two different reasons that both look like "it was requeued too
  often". The log line and the dashboard must say which, or the split buys
  clarity in the data and loses it in the explanation.
- **Negative:** a new worker-only route is a new surface that must enforce the
  lease check. It is the third place that check appears.
- **Neutral:** `--lease` keeps its meaning and default. Nothing about the safety
  net changes; this adds a faster path beside it.

## Out of Scope

- Reclaiming on SSE disconnect (permanent: boundary: a stream can drop while the subprocess is still running, and requeueing then runs the job twice — the exact thing the lease exists to prevent)
- Any change to `--max-attempts` or to what a failure costs (permanent: boundary: ADR-0001 owns the failure retry model and it is correct; this record only stops a different event from spending it)
- A budget for cooperative releases (permanent: boundary: a release loop requires a human restarting a worker repeatedly, and `jobs.expires_at` already bounds the job's total lifetime)
- Draining in-flight work before shutdown — the worker releases rather than finishes (deferred: `docs/adr/BACKLOG.md`)
- Surfacing `reclaims` on the dashboard (deferred: `docs/adr/0007/0007-failure-detail.md`)
- Multi-router lease coordination (permanent: fact: ADR-0001 specifies a single router process owning SQLite as the single writer; citation: file `internal/router/service.go:1`)

## Risks

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| The release route is called for a job the caller does not hold, requeueing another worker's live job | Low | High — the job runs twice | `ReleaseLease` uses the SAME `holdsLease` guard as `Complete` and `Fail`; T3 tests a second worker's release being refused, and that the holder's still works |
| A worker releases, then its subprocess keeps running and reports a result later | Med | Med — a late result for a requeued job | Already handled: `holdsLease` refuses a completion from a worker whose lease is gone, which is why the release must also clear `worker_id` |
| Unbounded reclaim loop on a poison-pill job | Low | High | `--max-reclaims` bounds it; T2 asserts the job dies at the bound with a reason naming abandonment, not failure |
| The two death reasons become indistinguishable to an operator | Med | Med | The terminal reason names which budget ran out, and ADR-0007's failures view shows it. Stated as a Negative above rather than mitigated away |
| `ReclaimJob` and `RequeueJob` drift apart, so one path forgets a field | Med | Med | Both live in `repo_write.go` beside each other; T1's test asserts a reclaim leaves `attempts` untouched AND a requeue leaves `reclaims` untouched — the pair, not one direction |

## Rollback

Persistent state and a new route, so both halves are needed.

1. **Schema:** migration `00005` adds one column with a default; `goose down`
   drops it. A database rolled back loses the reclaim counts and nothing else —
   `attempts` is untouched throughout.
2. **Contract:** `POST /release` is additive. A rolled-back router 404s it and the
   worker treats a failed release as non-fatal (it is best-effort on a path that
   is already shutting down), so an un-upgraded router degrades to today's
   behaviour — the lease expires as it does now. Roll the router back first; a
   worker releasing into a router that does not have the route simply stops
   early.
3. **Behaviour:** with `00005` rolled back, the reaper's reclaim falls back to
   spending `attempts`, which is exactly today's behaviour.

## Follow-ups

- [ ] Decide whether `reclaims` deserves a metric (`ocrr_jobs_reclaimed_total{label}`) — a service whose workers keep dying is an operational signal, and ADR-0001 T11's cardinality allow-list is the constraint on adding it
