# ADR-0007: Carry a failed job's exit code and output as data, and give failures a place to be seen

**Status:** Accepted
**Date:** 2026-09-16
**Owner:** M
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/0002/0002-rate-limiting-and-structured-logging.md`, `docs/adr/0006/0006-raw-passthrough.md`
**Governs:** `internal/runner/**`, `internal/agent/**`, `internal/httpapi/upload.go`, `internal/router/service.go`, `internal/router/reaper.go`, `internal/store/repo.go`, `internal/store/repo_write.go`, `internal/store/migrations/**`, `internal/web/web.go`, `internal/web/views/**`, `internal/core/job.go`

<!-- Class: every hop a FAILED job's cause crosses between the forked command and
an operator's eye. Enumerated 2026-09-16 with
`git grep -ln 'LastError|last_error|failJob|Fail(|stderrTail|ExitCode' -- '*.go' '*.templ' '*.sql'`
filtered of tests and generated files → 9 files. cmd/worker and cmd/router are
excluded and named in Out of Scope: they wire flags, and neither carries a cause. -->

**Enforced-by:** None — the check is `TestExitCodeSurvivesToTheJobRow`, created by T3; naming it before it exists would be the pointer-to-nothing this header exists to prevent. Set this to `internal/httpapi/failure_test.go::TestExitCodeSurvivesToTheJobRow` when T3 lands.
**Invalidates:** ADR-0002 — one clause, narrowed rather than removed: §Decision 2's transition log carries `reason` as its only failure detail; a failed job's line gains `exit_code` beside it, which is additive to that schema and changes nothing it already emits.
**Served-path change:** An operator whose service fails sees the command's own output and its exit code on the dashboard, and can list only failing jobs — today a tool that explains itself on stdout produces `exit status 1: ` with nothing after the colon.

## Context

M, 2026-09-16: *"need to handle exit status code and error from worker forked command, and show errors in dashboard"*.

Three quarters of this already works, and the record should say so rather than
re-solve it. Measured against the running deployment's database on 2026-09-16:
11 of its job rows carry a non-empty `last_error`, and the dashboard renders it
in a Detail column (`internal/web/views/views.templ:72`). A `dead` job reads
`stdout is not a JSON array of strings (got "labas")` — the runner's message,
stored and displayed end to end.

What does not work is narrower and worth naming precisely.

**1. The command's stdout is discarded on failure.** `Runner.exec` captures both
streams, and the non-zero-exit path interpolates only `stderr`
(`internal/runner/runner.go:123`). A tool that fails and explains itself on
stdout — the common shape for CLI converters — yields `exit status 1: ` and
nothing after the colon. The explanation existed and was thrown away.

**2. The exit code is prose, not data.** `exitErr.ExitCode()` appears exactly
once in the repository, interpolated into a format string. It is not a field on
the wire, not a column, and not an attribute on ADR-0002's structured log, so
nothing can filter by it, group by it, or alert on it. "Handle the exit status
code" is precisely this: a number that only exists inside a sentence cannot be
handled.

**3. Failures have no place of their own.** The dashboard lists 50 recent jobs of
every state, with the cause in one muted cell. Answering "what is failing on this
service right now" means reading the table. A 2000-byte stderr tail in a table
cell also destroys the layout it lands in.

## Existing Primitives Audit

| Primitive | Disposition |
|-----------|-------------|
| `jobs.last_error` + the `reason` threaded through `failJob`, `RequeueJob`, `FailJobDead` | **Reuse unchanged.** The transport for a failure's human text already exists and already reaches the dashboard. This ADR adds a sibling field; it does not replace the one that works. |
| `Runner.exec`'s captured `stdout`/`stderr` buffers and `stderrTail` | **Reshape.** Both buffers exist and are bounded; only the failure message's use of them changes. |
| `workerResult` (`internal/httpapi/upload.go`) — the worker's JSON report | **Reshape**: gains `exit_code`. It already carries `error`, so the envelope is the right place and needs no second endpoint. |
| ADR-0002's `logTransition` / `Logger` | **Reshape**: one additive attribute on a failure line. ADR-0002 owns the log's shape, which is why it is named in `Invalidates:` rather than quietly extended. |
| The dashboard's `Detail` column and `JobRow` | **Reuse and extend.** The column stays; the failures view is a filter over the same read model, not a second one. |
| `core.ErrModeMismatch` / the `fatalRefusal` split in `internal/agent` | **Reuse unchanged.** A refusal is a configuration answer and is already fatal; this ADR is about a command that RAN and failed. |

## Decision

**A failure's cause becomes three fields instead of one sentence.** The runner
returns a structured failure carrying the exit code, the captured output, and the
kind of failure (`exit`, `timeout`, `output-limit`, `contract`). The worker posts
`exit_code` beside the existing `error` string. The router stores it on
`jobs.exit_code` and puts it on the failure's structured log line. The dashboard
shows it, and gains a view that lists only failing jobs.

**The output that reaches the operator is stderr AND stdout**, in that order,
each labelled, bounded by the existing `stderrTail` budget per stream. Discarding
stdout was never a decision — it was the `%s` that happened to be written — and
the tools most likely to be configured here explain themselves there.

**`exit_code` is NULL, not 0, for a failure that never reached an exit status.**
A timeout, an output-limit trip and a contract violation have no exit code, and 0
is the code for SUCCESS. Storing 0 for "no exit happened" would make the one
value an operator most wants to filter on — "show me the clean exits" — silently
include every job that never exited at all. The column is nullable and
`core.Job.ExitCode` is a `*int`.

**The falsifying case, and whether data exists to produce it:** the claim is that
a non-zero exit's code and output reach the job row unchanged. It fails whenever
a component drops either, and that case is constructible today with a two-line
shell script (`echo boom; exit 3`) — which is what T1's test uses. It is valid
for the subprocess contract ADR-0001 defines; it says nothing about a worker
implementation that does not fork, and there is none.

**The failures view is a FILTER, not a second read model.** `buildDashboard` is
already the one function of the world that both the page load and every SSE patch
call; a second query for failures would be a second thing to keep correct, and
the first divergence would be invisible.

## Alternatives Considered

- **Leave the exit code in the message and only fix the stdout loss.** One
  commit, no schema, no contract change. Rejected as the answer to what was
  asked: a code inside a sentence cannot be filtered, grouped or alerted on, and
  the request was to *handle* it. Recorded because it is a legitimate smaller
  shape if the structured half is ever judged not to earn its keep.
- **A separate `job_failures` table, one row per attempt.** Keeps a full history
  rather than only the last failure. Rejected as speculative for now: `jobs`
  already keeps `attempts` and the last cause, nobody has asked to see attempt 2
  of 3, and a second table is a migration, a retention policy and a join for a
  question nobody has posed. Named in Out of Scope so the shape is on record.
- **Store the whole output as a blob.** Unbounded fidelity, and the blob store
  already exists. Rejected: it makes every failure a file to reap, on the path
  that is already the least healthy, to preserve output past the 2000 bytes that
  have so far been enough.
- **Surface failures only on the Services table** (count and last error per
  label). Rejected as the sole answer — it is a good summary and a poor
  diagnosis, because it cannot show WHICH job or let an operator read one — but
  it is worth having later, and is deferred rather than dismissed.
- **Parse the exit code back out of `last_error` for the dashboard.** No schema
  change at all. Rejected outright: deriving data from a message means the
  message can never be reworded, which is the shape that makes prose load-bearing.

## Component / Boundary Impact

| Component | Ownership after change | One reason to change? |
|-----------|------------------------|-----------------------|
| `internal/runner` | Owns the subprocess contract and now returns a structured failure | Yes — what the forked command did |
| `internal/agent` | Reports that failure faithfully | Yes — worker behaviour |
| `internal/httpapi` | Owns the wire envelope, gains one field | Yes — HTTP surface |
| `internal/router` | Owns job state and now persists the code; emits it on the log line | Yes — job-state rules |
| `internal/store` | Owns `jobs.exit_code` | Yes — persistence |
| `internal/web` | Owns the failures view and the readable Detail | Yes — admin UI |

No component gains a second reason to change: the failure detail is threaded
through existing owners.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| `jobs.exit_code` (schema) | new NULLABLE integer column | migration `00004` | `internal/store`, `internal/router`, `internal/web` |
| `runner.Failure` (type) | new: `Kind`, `ExitCode *int`, `Output string`; `Run`/`RunRaw` return it | `internal/runner` | `internal/agent` |
| `POST /upload` worker JSON | `exit_code` added beside `error`; absent means none | `internal/agent` | `internal/httpapi/upload.go` |
| `Router.Fail(ctx, workerID, jobID, reason, exitCode, now)` | gains the code | `internal/httpapi` | `internal/router` |
| ADR-0002 transition log | `exit_code` attribute on a failure line only | `internal/router` | operators, log search |
| `GET /admin/jobs?state=failed` | new filter on the existing dashboard read model | `internal/web` | administrators |
| `core.Job.ExitCode *int` | new field | `internal/core` | everything above |

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `jobs.exit_code` + `core.Job.ExitCode` | T1 | T3, T4 | No — additive and nullable |
| `runner.Failure` with `Output` carrying both streams | T2 | T3 | Yes — `Run`/`RunRaw` signatures change; `internal/agent` is the only caller |
| `exit_code` on the worker's report + `Router.Fail`'s parameter | T3 | T4 | Yes — `Fail`'s signature changes; `internal/httpapi` is the only caller |
| `jobs.exit_code` populated end to end | T3 | T4 | No |

## Implementation

See `tasks/README.md` — 4 tasks, sequential.

## Consequences

- **Positive:** a failing service is diagnosable from what the tool actually
  said, including on stdout, instead of from a truncated sentence.
- **Positive:** the exit code becomes filterable, groupable and alertable —
  "every job that exited 137" is a query rather than a grep over prose.
- **Positive:** failures get one place to be looked at, so the question an
  operator actually asks is one click rather than a scan of 50 mixed rows.
- **Negative:** a nullable column means every reader must handle absence. That is
  the honest shape — a timeout has no exit code — but it is a `*int` to get wrong.
- **Negative:** `Runner.Run`, `Runner.RunRaw` and `Router.Fail` all change
  signature. Each has exactly one caller, so the compiler finds them all, but it
  is three breaking changes for one feature.
- **Neutral:** `last_error` keeps its current meaning and content. Nothing that
  reads it today needs to change.

## Out of Scope

- `cmd/worker` and `cmd/router`, though both sit beside the enumerated class — they wire flags and carry no failure cause (permanent: boundary: a composition root is not a hop on the path from the command to the operator)
- A `job_failures` table keeping every attempt rather than the last (deferred: `docs/adr/BACKLOG.md`)
- Per-service failure counts on the Services table (deferred: `docs/adr/BACKLOG.md`)
- Storing subprocess output beyond the existing per-stream byte budget (permanent: boundary: `stderrTail` is 2000 bytes and has been sufficient; raising it is a number to change, not a decision to take)
- Alerting or notification on failures — this ADR makes the data queryable and stops there (deferred: `docs/adr/BACKLOG.md`)
- Any change to what a REFUSAL does. A worker refused for a configuration mismatch never ran a command (permanent: boundary: ADR-0006 and the fatal-refusal path own that, and a refusal has no exit code by construction)
- Exposing failure detail to the CLIENT. A customer sees that their job failed, not the operator's tool's stderr (permanent: fact: ADR-0001 keeps worker-supplied text out of the client's result contract, which carries units only; citation: file `internal/httpapi/files.go:27`)

## Risks

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| A worker's stdout carries customer data, and the dashboard now shows more of it | Med | High — an operator page could display document contents | The budget is unchanged (2000 bytes per stream) and the audience is unchanged (admin-only, already showing stderr). T2 states the exposure explicitly rather than widening it silently; ADR-0002 already decided param VALUES are never logged, and that rule is untouched |
| `exit_code` stored as 0 for a timeout, making "clean exits" a lie | Med | Med | The column is NULLABLE and the field is `*int`; T1's test asserts a timeout leaves it NULL, and T4's view distinguishes "no exit" from "exited 0" |
| Three signature changes land unevenly and a caller keeps the old shape | Low | Med | Each has exactly one caller and Go will not compile a mismatch; each task's sweep command is recorded in its Risks |
| The failures view diverges from the main dashboard | Low | Med | It is a filter over `buildDashboard`, not a second query — stated in the Decision and asserted by T4 |
| Long output breaks the table layout it is shown in | High | Low | T4 renders detail so it wraps or is bounded in the cell, with the full text reachable; this is the defect that exists today |

## Rollback

Persistent state and a wire contract, so both halves are needed.

1. **Schema:** migration `00004` adds one nullable column; `goose down` drops it.
   Nothing reads it before T3, so a rollback to any point loses only the codes
   recorded since.
2. **Contract:** `exit_code` is additive and optional on the worker's report. A
   rolled-back router ignores an unknown field and a rolled-back worker simply
   omits it, so a mixed fleet degrades to today's behaviour — the code is absent,
   the message is unchanged — rather than failing. Roll workers and router back
   in either order.
3. **UI:** the failures view is a route and a template; removing it leaves the
   existing Detail column exactly as it is now.

## Follow-ups

- [ ] Decide whether `exit_code` earns a metric dimension (`ocrr_jobs_total{state,exit_code}`) once there is data — deliberately NOT added here, because T11's cardinality allow-list exists to stop exactly this kind of speculative label
