# ADR-0002: Limit request rate per token and make single failures readable with structured logs

**Status:** Accepted
**Date:** 2026-09-15
**Owner:** M (operator) — authored by claude-code-aks
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/tasks/T11-monitoring.md`, `docs/adr/BACKLOG.md`
**Governs:** `internal/ratelimit/**`, `internal/logging/**`, `internal/httpapi/**`, `cmd/router/**`
**Enforced-by:** None — per-task Acceptance fences and their bound mutants. The two clauses that
would most repay a standing gate are "a limiter entry is evicted" and "a param VALUE is never
logged"; both are asserted by tests that a mutant is bound to, and neither has a cheap grep that
could not be defeated by a rename. Naming `go vet` here would name a check that cannot fail on
either.
**Invalidates:** none — ADR-0001 deferred both of these items explicitly rather than deciding
against them (`adr-state.mjs`, 2026-09-15: ADR-0001 is the only governing record, and the paths
this record adds under `internal/ratelimit/**` and `internal/logging/**` are new).
**Served-path change:** A customer that exceeds its request rate now receives `429` with
`Retry-After` instead of being served without bound; and an operator debugging a dead job can read
one JSON line per request and per state transition on the router's stdout, where previously nothing
recorded which worker took a job or how long it sat in each state.

## Context

ADR-0001 shipped the router with two things it deliberately did not decide, and T11 shipped
monitoring that exposed the shape of both. `adr-debt docs/adr` reports them as open deferrals
pointing at `docs/adr/BACKLOG.md`:

**Rate is unmetered.** ADR-0001 meters *concurrency* per customer through `users.buffer_limit`,
which stops one customer starving the worker pool. It says nothing about *rate*. Two concrete
consequences, both reachable today with a valid token:

- A client inside its buffer limit can hammer `POST /upload`. Every request past the limit is
  rejected with `ErrBufferFull`, but the rejection costs a multipart parse, a blob write that is
  then discarded, and a write-handle transaction — and the write handle is a single connection
  (`MaxOpenConns(1)`, ADR-0001), so rejected work from one customer delays accepted work from
  everyone.
- A compromised worker token can poll `POST /claim` without bound. The claim statement is one
  atomic `UPDATE … RETURNING` against that same single writer.

Neither is a denial-of-service by an outsider — both need a valid token — which is why this is
worth doing and worth doing narrowly.

**A single failure is unreadable.** T11 made the system's health *countable*: `ocrr_jobs_total`
says three jobs died, `ocrr_workers_live` says a label has nobody serving it. None of it says
*which* job, *which* worker, or *why*. When a job dies at attempt 3 nothing records which worker
took each attempt, what the subprocess wrote to stderr, or how long the job sat queued versus
leased. The operator's only current recourse is the admin dashboard's job list, which shows the
final state and nothing about the path to it.

**Both are deferred, not undecided.** This record closes the two the backlog marks as needing a
decision, and re-defers the third (OTLP export) with a fresh pointer rather than letting it age out
of view.

## Existing Primitives Audit

Checked before proposing anything new (`grep`, `go list ./...`, and `adr-context` over
`internal/httpapi` and `cmd/router`, 2026-09-15):

| Need | Existing primitive | Reused? |
|---|---|---|
| Per-caller identity on a request | `httpapi.principal(r)` / `PrincipalFrom` (T3, T7) | **Yes** — the limiter keys on `Principal.TokenID`, so it needs no lookup of its own |
| A place every authenticated request passes | `API.authenticate` middleware group in `internal/httpapi/api.go:69` | **Yes** — the limiter is a second middleware in the same group |
| A periodic sweep for expiring in-memory state | `App.StartReaper` ticker in `cmd/router/wire.go:180` | **Yes** — limiter eviction rides the existing tick rather than starting a second goroutine |
| An in-memory store with TTL eviction | `internal/results.Store` | **No** — it stores job results keyed by job id with a TTL measured from *insertion*; a limiter entry must expire from *last use*, and bending `results` to both would make one type answer two questions |
| Counting things for operators | `internal/monitor.Registry` (T11) | **Yes** — `ocrr_requests_throttled_total{state}` is a new series on the existing registry, not a new mechanism |
| Structured logging | none — the codebase uses `fmt.Printf` in `cmd/router/main.go` for three startup lines | **No** — `log/slog` is standard library since Go 1.21 |
| A rate limiter | none in-tree | **No** — see Alternatives |

## Decision

**We will limit request rate per bearer token with an in-memory token bucket applied as HTTP
middleware inside the existing authenticated group, and we will emit structured `log/slog` JSON on
stdout for every request and every job state transition, with customer-supplied param values
redacted by construction. We will not adopt OpenTelemetry.** The three sections below state each
half and what it costs.

### 1. Rate limiting is a middleware keyed on the token, inside the authenticated group

`internal/ratelimit` provides a `Limiter` holding one token bucket per **token id**, and
`httpapi` applies it as a middleware **inside** the existing `authenticate` group.

**Inside, not outside, and that is the whole shape of it.** The backlog asked for *per-token*
limiting. A middleware outside the authenticator has no token to key on — only an IP, which is
wrong for this system in both directions: workers are explicitly "anywhere on the internet" and
several may share one NAT egress, while one compromised token can move between addresses freely.
Keying on the authenticated principal is the only key that matches what is being limited.

⚠ **The consequence, stated rather than discovered later: this does nothing about unauthenticated
floods.** A caller with no token is rejected by `authenticate` before the limiter ever runs. That
rejection is cheap — a constant-time hash comparison and no database write — but it is unbounded,
and defending it is a reverse proxy's job, not this process's. Recorded in Risks.

**Buckets, with an injected clock.** `golang.org/x/time/rate.Limiter`, one per token id.
`AllowN(t time.Time, n int)` takes the time explicitly, so every test is deterministic rather than
a sleep — which is the property that decides this against a hand-rolled bucket (see Alternatives).

**Limits are per role, configured by flag**, because the three roles have genuinely different
shapes of traffic: a client uploads occasionally in bursts, a worker polls steadily, an admin
clicks. Defaults — `--rate-client 10/s burst 20`, `--rate-worker 30/s burst 60`,
`--rate-admin 30/s burst 60` — are chosen to be far above any legitimate use and are documented as
a ceiling on abuse rather than a quota. A limit tight enough to shape normal traffic would be a
product decision about what customers may do, and this record is not making one.

**Over the limit is `429` with `Retry-After`**, carrying the seconds until the next token, so a
well-behaved client backs off correctly instead of guessing.

**Entries are evicted on the existing reaper tick.** A map keyed by token id grows without bound
otherwise, and "the limiter is the memory leak" is the failure mode this design must not ship.
An entry unused for `--rate-idle` (default 10m) is dropped; dropping it forgives accumulated
history, which is correct — a token that has been silent for ten minutes is not mid-burst.

### 2. Structured logging is `log/slog` JSON on stdout, at two points

`internal/logging` builds the handler; two call sites emit.

**stdout, not a ring buffer over SSE.** Chimney exposed logs over its own stream; that is a second
delivery mechanism, a second authorization question, and a second place for a customer's data to
leak. Every deployment target already captures stdout, and `--log-format text` stays available for
a human at a terminal.

**One line per request**, from a middleware beside the limiter: method, route pattern (not the raw
path — `/files/{id}` rather than the id, so the series is bounded for the same reason the metric
labels are), status, duration, `user_id`, `token_id`, `role`.

**One line per job transition**, from `router.Service` at the points that already increment T11's
counters: `job_id`, `user_id`, `label`, `from` → `to`, `attempt`, `worker_id`, `stage`, and the
duration spent in the state just left. That last field is what turns "the job died" into "it sat
queued for forty minutes and then failed in two seconds", and it is the reason to log transitions
rather than only requests.

⚠ **Redaction is a rule about VALUES, and it is enforced by a type.** Job params are
customer-supplied: ADR-0001 lets a crawler worker take `?url=…`, and a URL carries credentials
often enough that treating it as safe is a decision to leak them eventually. So **param keys are
logged and param values never are.** The keys are already constrained to
`^[a-z][a-z0-9-]{0,31}$` by `core.ValidParamKey`, so they are bounded and safe; the values have no
constraint at all. Tokens are never logged — only the token id, which is already a database key.
Emails are not logged; `user_id` is, and the admin dashboard maps one to the other.

The rule is enforced by `logging.Job()` accepting the param **map** and emitting only its sorted
keys, rather than by asking every call site to remember. A call site cannot pass values in by
accident because there is no argument that takes them.

### 3. OTLP export is re-deferred, with the reason restated

T11 rejected OpenTelemetry *for now* because it adds a collector as a runtime dependency for a
single-process system. Nothing in this record changes that, and implementing it here would reverse
an accepted decision with no new evidence. It returns to `docs/adr/BACKLOG.md` under this record's
pointer. The metric names T11 defines remain exportable over OTLP without re-measuring anything, so
this stays a transport decision.

## Alternatives Considered

- **A hand-rolled token bucket:** ~40 lines, no new module, and this codebase has no third-party
  runtime dependencies beyond chi, templ, datastar, urfave/cli and the SQLite driver. Rejected
  because a limiter's correctness lives entirely in its burst accounting and its clock handling,
  and both are easy to get subtly wrong in a way no test written by the same author would catch.
  `golang.org/x/time/rate` is maintained by the Go team, is pure Go with no cgo, and —
  decisively — its `AllowN(t, n)` takes the instant as an argument, so the tests here are
  deterministic rather than sleeping. A hand-rolled bucket would need the same injected clock to
  be testable, at which point it is the same design with less review behind it.

- **Rate limiting in `router.Service` rather than middleware:** puts the limit next to the
  business rule it protects, and would cover any future non-HTTP caller. Rejected because there is
  no other caller, the service methods have no notion of a *token* (they take a user id, since
  ADR-0001 scoped the buffer limit to the customer), and pushing the limit down means the expensive
  part — the multipart parse and the discarded blob write — has already happened before the limit
  is consulted. The middleware rejects before any of it.

- **Limiting per user rather than per token:** simpler, and one obvious reading of "fair".
  Rejected because it is what `buffer_limit` already does, one axis over. The backlog entry named
  the token deliberately: the threat is a *leaked credential*, and a per-user limit lets a
  compromised token consume the legitimate one's allowance.

- **A distributed limiter (Redis, or a `rate_limits` table):** correct for a multi-process router.
  Rejected because ADR-0001's router is a single process by design, so an in-memory limiter is
  exactly as accurate as a shared one without the dependency. Recorded in Risks as the thing that
  must change first if the router is ever replicated.

- **`zerolog` / `zap`:** faster, with richer field helpers. Rejected because `log/slog` is
  standard library, this system logs a handful of lines per request rather than millions, and a
  logging dependency is one every other package ends up importing.

- **Logs over SSE, as chimney did:** considered seriously, since this system already has an SSE
  bus and an admin dashboard to render into. Rejected for this record because it needs a decision
  about which principals may read which logs — a customer must never see another's params, and the
  transition log is full of them — and that authorization design is larger than the logging it
  would serve. Re-deferred to the backlog rather than half-built.

- **Logging the full request path instead of the route pattern:** rejected for the same reason T11
  allow-lists metric label names. `/files/{uuid}` as a raw path makes every line unique, which
  defeats grouping in any log aggregator and embeds a job id in a field meant to identify a route.

- **Sampling the request log:** rejected because at this volume it saves nothing and makes "I
  cannot find the request" indistinguishable from "the request never arrived".

## Component / Boundary Impact

No new bounded context. Two new leaf packages inside the existing router boundary, both with a
single consumer:

- `internal/ratelimit` — depends on nothing in-tree. Consumed by `internal/httpapi`.
- `internal/logging` — depends on `internal/core` only (for the param-key type). Consumed by
  `internal/httpapi`, `internal/router`, and `cmd/router`.

The C4 container diagram is unchanged: no new process, no new listener, no new external
dependency. `internal/router` gains a `Logger` field on the same pattern as T11's `Counter` — an
interface **at the consumer** with a nop default, so `router` still depends on nothing above it and
every test written before this record compiles unchanged.

No repository architecture document exists (`docs/architecture.md` is absent, checked 2026-09-15),
so there is no Module Map to update; ADR-0001 plus this record are the structural sources.

## Wiring & Contract Changes

| Surface | Change | Breaking? |
|---|---|---|
| HTTP | New response `429 Too Many Requests` with `Retry-After: <seconds>` on every authenticated route | **Additive** — no existing status changes; a client that ignores it sees a new error code |
| `core` errors | New `core.ErrRateLimited`, mapped to 429 by `httpapi.writeError` | Additive |
| CLI | New flags `--rate-client`, `--rate-worker`, `--rate-admin`, `--rate-burst`, `--rate-idle`, `--log-level`, `--log-format` | Additive; all have defaults |
| `router.Service` | New `SetLogger(Logger)`, defaulting to a nop — same shape as T11's `SetCounter` | Additive; existing constructions compile |
| Metrics | New counter `ocrr_requests_throttled_total{role}` | Additive; `role` is **not** in T11's allow-list, so the allow-list gains it — a role is a fixed enum of three, so the cardinality argument holds |
| `go.mod` | New require `golang.org/x/time` | Additive |
| Database schema | **None** — the limiter is in memory, the logs go to stdout | — |
| SSE protocol | **None** | — |

⚠ The metric-label allow-list change is the one item here that touches a decision T11 made rather
than extending it. It is a widening of a deliberately narrow set, so T11's guard test must be
updated in the same commit as the constant, and the widening is justified on the same ground the
original three were: `role` takes one of three values, fixed at compile time.

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `ratelimit.Limiter` / `Limiter.Allow()` | T1 | T3, T4 | No — new type |
| `ratelimit.Limiter.EvictIdle()` | T1 | T4 | No — new method, called from the existing reaper tick |
| `logging.Logger` / `logging.New()` | T2 | T3, T4 | No — new type |
| `logging.Job()` (the key-only param redactor) | T2 | T4 | No — new function |
| `core.ErrRateLimited` → `429` | T3 | T4 | No — additive status on existing routes |
| `httpapi` rate-limit and request-log middlewares | T3 | T4 | No — applied inside the existing authenticated group |
| `ocrr_requests_throttled_total{role}` + the `role` allow-list entry | T3 | T4 | **Yes, narrowly** — widens the label allow-list ADR-0001 T11 froze, so T11's guard test changes in the same commit |
| `router.Service.SetLogger()` | T4 | — | No — nop default, existing constructions compile |

T4 is last because it is the composition root and consumes all three.

## Implementation

Four tasks in `docs/adr/tasks/`, executed in order: T1 (limiter) and T2 (logging) are independent
of each other and of everything else; T3 wires the limiter into the API; T4 wires both into the
binary and adds transition logging. See `docs/adr/tasks/README-0002.md`.

## Consequences

**Good.** A leaked token has a bounded blast radius against the single writer. A dead job is
readable end to end for the first time: which worker, which attempt, how long in each state.
Neither costs a schema change, a new process, or a runtime dependency on a collector.

**Bad.** Two more middlewares on every authenticated request — measured overhead is a map lookup
under a mutex and one JSON encode, but it is not zero. The limiter holds memory proportional to
*active tokens*, which is small but is now a thing that can leak if eviction breaks. A rate limit
is a new way for a legitimate integration to fail, and it will fail as a `429` that the customer's
client may not handle.

**Neutral but load-bearing.** The limiter is per process. The moment the router is replicated, the
effective limit is multiplied by the replica count — accurate *now* precisely because ADR-0001
committed to one process, and wrong the day that changes.

## Out of Scope

- OpenTelemetry / OTLP export (deferred: `docs/adr/BACKLOG.md`).
- Logs delivered over SSE to the admin dashboard, and the per-principal authorization that would
  require (deferred: `docs/adr/BACKLOG.md`).
- Unauthenticated request limiting and connection-level DoS defence (permanent: boundary: a caller
  with no token is rejected before any database work, and defending the socket is a reverse proxy's
  job — putting it here would mean this process re-implementing what every deployment already has
  in front of it).
- A distributed or shared limiter (permanent: fact: ADR-0001 commits the router to a single
  process, so a shared limiter would be exactly as accurate as the in-memory one; citation: file
  `docs/adr/0001-ocr-router-architecture.md:521`).
- Per-customer rate quotas as a billable product feature, as opposed to an abuse ceiling
  (permanent: boundary: what a customer is entitled to consume is a commercial decision, and this
  repository does not know the contracts).
- Log shipping, retention, and aggregation configuration (permanent: boundary: retention is valid
  for a deployment and this repository does not know the deployment — the same line T11 drew for
  alert thresholds).
- Audit logging of administrative actions as a separate tamper-evident stream (deferred:
  `docs/adr/BACKLOG.md`).
- Tracing spans across the router → worker → router round trip (deferred: `docs/adr/BACKLOG.md`).

## Risks

| # | Risk | Mitigation |
|---|---|---|
| 1 | **The limiter map is the memory leak.** One entry per token id, never evicted, is unbounded growth keyed by something an admin can mint freely. | `EvictIdle` on the existing reaper tick, with a test that asserts the map SHRINKS rather than that eviction was called. T1 binds a mutant to it. |
| 2 | **Eviction forgives a burst.** A caller could idle just past `--rate-idle` and start fresh. | Deliberate and harmless: ten minutes of silence to reset a twenty-request burst is a lower rate than the limit itself. Stated here so it is not later read as a bug. |
| 3 | **A rate limit is a new outage for a legitimate client.** Defaults far above real use make this unlikely, but a batch integration uploading a thousand files will meet it. | `Retry-After` on every 429, the limits on the command line, and `ocrr_requests_throttled_total{role}` so throttling is visible to the operator before the customer complains. |
| 4 | **Logging params could leak credentials.** A crawler's `?url=` may carry a password. | `logging.Job()` takes the map and emits only sorted keys; there is no argument that accepts a value. A mutant that makes it emit values must be killed. |
| 5 | **A test that asserts a param value is absent passes vacuously if the fixture has no params.** | Every redaction test uses a param whose value is a distinctive sentinel AND asserts the KEY is present — so "nothing was logged at all" fails too. |
| 6 | **The per-process limiter silently becomes wrong under replication.** No test can catch this; it is a deployment change. | Recorded in Consequences and in the README's operational notes, next to the existing "the router is a single process" note. |
| 7 | **`x/time/rate` uses the wall clock by default.** Calling `Allow()` instead of `AllowN(now, 1)` anywhere makes that call site untestable and inconsistent with the injected clock. | The limiter type takes a `Now func() time.Time` and never calls the argument-less forms; T1's tests advance the clock explicitly and would hang or flake if a wall-clock call crept in. |
| 8 | **Widening the metric label allow-list is precedent.** The next field will cite this one. | The widening is argued from the same bounded-enum test the original three passed, and T3 updates T11's guard test in the same commit, so the allow-list and its test can never disagree. |
| 9 | **Middleware order matters and is invisible.** The limiter must run after `authenticate` (it needs the principal) and the request logger must run outermost (it must log the 401s and the 429s). | The order is asserted by a test that checks a 401 IS logged and that an unauthenticated request does NOT consume limiter budget. |
| 10 | **`slog` at debug level could log a body.** Nothing does today, but the handler makes it one line away. | The request middleware logs a fixed field set from the response writer and the route pattern; it never has the body in scope. |
| 11 | **Duration-in-state requires knowing when the state was entered.** Reading it from a stale in-memory map would be wrong after a restart. | It is computed from the job row's existing timestamp columns, which survive a restart, rather than from anything held in memory. |
| 12 | **A 429 on `POST /claim` could make a worker back off into a stall** if the worker treats it as fatal. | The worker's runner already treats non-2xx claim responses as "try again later"; T4's test drives a real 429 through the worker agent and asserts it keeps polling. |

## Rollback

Nothing persistent changes — no schema migration, no on-disk format, no wire contract that a
client depends on receiving.

- **Rate limiting** is disabled by setting any `--rate-*` flag to `0`, which the limiter reads as
  unlimited and which is asserted by a test. That is the operational rollback; no redeploy of a
  previous build is needed.
- **Logging** reverts to the previous behaviour with `--log-level error`, and the three startup
  `fmt.Printf` lines are left in place deliberately so a misconfigured logger cannot make the
  process appear dead at boot.
- **Full revert** is `git revert` of this record's commits plus `go mod tidy` to drop
  `golang.org/x/time`. No data migration, forward or backward.

## Follow-ups

- Operator to confirm the default rate limits are above their real integration traffic before this
  is deployed to anyone who would notice a `429`. The defaults here are an abuse ceiling chosen
  without traffic data; they are the one number in this record that no test can validate.
- Revisit the in-memory limiter if the router is ever replicated (Risk 6).
- Revisit logs-over-SSE once there is a reason to authorize per-principal log reads.
