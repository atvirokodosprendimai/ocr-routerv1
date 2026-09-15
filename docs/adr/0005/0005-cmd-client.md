# ADR-0005: Ship a CLI client that submits one file and blocks until the result lands

**Status:** Proposed
**Date:** 2026-09-15
**Owner:** M (operator) — authored by claude-code-aks
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/0002/0002-rate-limiting-and-structured-logging.md`, `docs/adr/BACKLOG.md`
**Governs:** `cmd/client/**`, `internal/client/**`
**Enforced-by:** `internal/client/client_test.go::TestStreamOpensBeforeUpload`
**Invalidates:** none — ADR-0001 specified the protocol and shipped no client for it
(`adr-state.mjs`, 2026-09-15).
**Served-path change:** `client --router … --token … -i scan.pdf -o out.txt` uploads a document,
blocks while it is processed, and writes the result. Today a customer must implement a multipart
upload, hold an SSE stream open, parse events, and fetch a result by id — four moving parts to OCR
one file.

## Context

ADR-0001 built the protocol and no client for it. Integrating today means:

1. `POST /upload` with a multipart body, keeping the returned `job_id`
2. holding `GET /sse` open, parsing `event:`/`data:` frames
3. watching for `ready` with a matching `job_id` — or `failed`, or neither
4. `GET /files/{id}` to collect, which is also what charges the credits

That is a reasonable protocol for a service integration and a lot to ask of someone who wants one
PDF OCR'd. The operator asked for the obvious tool: *token, router, `-i` input, hang while in
progress, show progress, write results to `-o`*.

**The gap is not capability, it is that nothing in this repository proves the protocol is usable.**
Every test to date drives the API from inside the module, with a Go test harness holding the SSE
stream. A CLI that a person runs is the first consumer that has to get the ORDERING right, handle a
dropped stream, and decide what an exit code means — and each of those is a place the protocol could
turn out to be awkward in a way no internal test would reveal.

## Existing Primitives Audit

Checked before proposing anything new (2026-09-15):

| Need | Existing primitive | Reused? |
|---|---|---|
| Upload a document | `POST /upload` multipart `file`, `?label=`/`?pipeline=`/arbitrary params | **Yes**, unchanged |
| Learn a job finished | `GET /sse` → `ready` / `failed` events carrying `job_id` | **Yes** |
| Survive a dropped stream | the `backlog` event on connect, which ADR-0001 added for exactly this | **Yes** — and it is what makes reconnection correct rather than hopeful |
| Collect a result | `GET /files/{id}` → `{"units":[…]}`, deletes and charges on success | **Yes** |
| Long-lived stream handling | `internal/agent` does this for workers | **No, not reused.** It polls `/claim` for work by label; a client waits for one known job id. The shapes rhyme and share nothing |
| Structured CLI | `urfave/cli/v3`, as both existing commands use | **Yes** |
| Terminal detection | `golang.org/x/term`, already a direct dependency (ADR-0003) | **Yes** |
| A testable seam | `internal/agent` + `cmd/worker`: logic in a package, wiring in `cmd` | **Yes** — `internal/client` holds the protocol, `cmd/client` holds flags and rendering |

## Decision

**We will ship `cmd/client`: a blocking, single-job CLI that opens the SSE stream BEFORE uploading,
writes progress to stderr and results to stdout-or-file, and exits with a code that says what to do
next.**

### 1. The stream opens BEFORE the upload, and that is the whole correctness argument

```
open GET /sse  →  POST /upload  →  wait for ready(job_id)  →  GET /files/{id}
```

⚠ **Uploading first is the obvious order and it is racy.** A fast job can finish between the upload
returning and the stream being established, and the `ready` event fires into a stream nobody is
holding — the client then waits forever for an event that already happened. The window is small,
which is worse than large: it passes every casual test and strands the caller in production on
exactly the jobs that went well.

Opening first closes it. The `backlog` event on connect is the second layer, covering a stream that
drops mid-wait: on reconnect the client is told which of its results are already waiting, so it does
not need the live event it missed.

### 2. Progress on stderr, results on stdout

`-o -` writes the result to stdout, which is what makes the tool composable
(`client … -o - | wc -l`). Progress therefore **cannot** go to stdout, and goes to stderr always.

**On a TTY** the progress is one line, rewritten in place: `uploading… → waiting… → done`.
**Off a TTY** it is one plain line per transition, because `\r` in a CI log is noise and a progress
bar in a pipe is corruption. `--quiet` suppresses all of it; errors still go to stderr.

### 3. Exit codes name an ACTION, not an error taxonomy

A script branches on what to do next, not on what went wrong:

| Code | Meaning | What the caller does |
|---|---|---|
| 0 | the result was written | carry on |
| 1 | **you must fix something** — bad flag, bad token, missing file, no credits, unknown service | do not retry; a human changes something |
| 2 | **try again later** — buffer full, rate limited, router unreachable, stream died | retry with backoff |
| 3 | **the job ran and failed** — dead after its attempts, or expired | investigate; the reason is on stderr |

⚠ **Exiting 0 on a failed job is the failure mode that matters most.** A client that writes an empty
file and exits 0 because "the request succeeded" turns a dead job into a silent data loss in
whatever pipeline called it. Code 3 exists so that cannot happen.

### 4. Output is text by default, the envelope on request

The result is `{"units": ["page one", "page two"]}`. By default the client writes the units joined
by newline — the natural form, and the same shape ADR-0001 specified for a worker's own stdout.
`--json` writes the envelope verbatim for a caller that wants the structure.

### 5. `-i` is optional, because a crawler job has no file

ADR-0001 supports a params-only upload: the job carries `?url=…` and the service fetches its own
input. So `-i` is required only when no parameters are given, and `--param k=v` is repeatable.
`--label` and `--pipeline` pass through unchanged.

### 6. Blocking forever is the default, with `--timeout` to opt out

The operator asked for "hang while in progress", and a queued job behind a busy worker pool is
supposed to take a long time — a default timeout would turn a slow success into a spurious failure.
`--timeout` bounds it for a caller who needs that, and exits 2 (retryable), because a timeout says
nothing about whether the job will eventually succeed.

## Alternatives Considered

- **Polling `GET /files/{id}` instead of SSE:** far simpler — no stream, no event parsing, no
  reconnection — and immune to a proxy that buffers responses. Rejected as the primary because the
  router has an SSE command bus built for exactly this and polling would load the single writer with
  work the design exists to avoid. ⚠ Recorded honestly as the real risk: **a buffering reverse proxy
  breaks this client**, and the symptom is a hang rather than an error. A `--poll` fallback is
  deferred rather than dismissed.

- **Uploading first, then connecting:** the order everyone writes first. Rejected — see Decision 1.
  It is the single most important thing in this record.

- **Reusing `internal/agent`:** it already manages a long-lived SSE connection with reconnection.
  Rejected because it polls `/claim` for any job of a label, while a client waits for one known job
  id; the shapes rhyme and share no logic. Forcing one type to do both would make the worker's hot
  loop carry a branch for a case it never takes.

- **A daemon or a batch mode (many files, a directory):** more useful for bulk work. Rejected for
  this record: the operator asked for one file, and a batch client needs decisions about
  concurrency, partial failure and ordering that one-file semantics do not. Deferred.

- **Writing results to stdout unconditionally:** simpler, no `-o`. Rejected because the common case
  is producing a file, and `> out.txt` cannot be combined with progress output on the same stream.

- **A single exit code for every failure:** conventional for small tools. Rejected because the
  caller's next action genuinely differs — retrying a dead job is pointless and retrying a
  rate-limited one is correct, and a script cannot tell them apart from code 1.

- **Reading the token from a flag only:** `--token` is in `ps` output and shell history.
  `OCRR_TOKEN` is read when the flag is absent, which is what a CI job will use.

## Component / Boundary Impact

No new bounded context. One new package and one new binary, mirroring the worker's split:

- **`internal/client`** — the protocol: open stream, upload, wait, collect. Depends on `core` only
  and talks HTTP; it holds no flags and writes no terminal output.
- **`cmd/client`** — flags, progress rendering, exit codes, stdout/file writing.

The router is untouched: no new endpoint, no schema change, no protocol change. That is the point —
if this client needed a server change, the protocol ADR-0001 specified would have been incomplete.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| New binary | `cmd/client` | T2 | a customer |
| New package | `internal/client` with `Submit()` | T1 | T2 |
| HTTP | **None** — uses `POST /upload`, `GET /sse`, `GET /files/{id}` exactly as they are | — | — |
| Database schema | **None** | — | — |
| `go.mod` | **None** — `urfave/cli/v3` and `x/term` are already direct | — | — |

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `client.Submit()` and `client.Config` | T1 | T2 | No — new package |
| `client.Progress` callback | T1 | T2 | No — new type |
| `client.Result` and its typed failure | T1 | T2 | No — new type |
| The `cmd/client` binary, flags and exit codes | T2 | — | No — new binary |

## Implementation

Two tasks in `docs/adr/0005/tasks/`: T1 is the protocol against a test server, T2 is the binary a
person runs. See `docs/adr/0005/tasks/README.md`.

## Consequences

**Good.** The protocol acquires its first outside consumer, which is the only way to find out
whether it is usable. A customer's integration becomes one command. The ordering hazard in Decision
1 is now written down and tested rather than waiting for each new integrator to rediscover it.

**Bad.** A second thing that must track the API: a change to the SSE event names or the result
envelope now breaks a shipped binary as well as the server's own tests. And this client is only as
robust as the stream — behind a buffering proxy it hangs, which is the worst failure shape there is.

**Neutral but load-bearing.** `GET /files/{id}` is what charges the credits, so a client that
collects and then fails to write its output has already been charged. The client writes to a
temporary file and renames, so a full disk cannot produce a charged-but-lost result.

## Out of Scope

- Batch or directory submission (deferred: `docs/adr/BACKLOG.md`).
- A polling fallback for buffering proxies (deferred: `docs/adr/BACKLOG.md`).
- Resuming a job across client restarts (permanent: boundary: the job id is the resume token and the
  backlog event already lists what is waiting — a caller who wants this can fetch by id with `curl`,
  and building a state file for it is a bigger commitment than the feature earns).
- Uploading several files as one job (permanent: fact: ADR-0001 defines a job as one document with
  one result; citation: file `docs/adr/0001-ocr-router-architecture.md:27`).
- A worker mode in the same binary (permanent: boundary: `cmd/worker` exists and the two have
  opposite lifecycles — one exits when its job is done, one runs forever).
- Progress as a percentage (permanent: boundary: the router reports states, not fractions, and a
  fabricated percentage is worse than an honest state name).

## Risks

| # | Risk | Mitigation |
|---|---|---|
| 1 | **Uploading before the stream is open loses the `ready` event** on fast jobs, and the client waits forever. The window is small, so it passes casual testing and fails in production on jobs that went well. | Decision 1; `TestStreamOpensBeforeUpload` asserts the ordering against a server that records it, and is named in `Enforced-by:`. A mutant binds to it. |
| 2 | **A buffering reverse proxy turns this into a hang with no error.** | Recorded in Consequences and the README; `--timeout` bounds it; a `--poll` fallback is deferred rather than pretended away. |
| 3 | **Exiting 0 on a dead job** turns a failure into silent data loss downstream. | Exit code 3, asserted for both `dead` and `expired`, and the output file is not created at all on failure. |
| 4 | **Progress on stdout corrupts `-o -`.** | Progress is on stderr unconditionally; a test pipes stdout and asserts it contains only the result. |
| 5 | **A charged result lost to a write failure.** `GET /files/{id}` deletes the in-memory result and charges the credits, so a failed write loses work the customer paid for. | Write to a temp file in the destination's directory and rename — the same fsync-then-rename discipline ADR-0001 T4 used for blobs. |
| 6 | **The token in `ps` and shell history.** | `OCRR_TOKEN` is read when `--token` is absent, and the README shows that form first. |
| 7 | **A test that asserts "it waits" can hang the suite forever.** | Every waiting test carries a context deadline and the fixture drives the event; a test that would hang fails instead. |
| 8 | **A `ready` event for a DIFFERENT job** of the same user — a client with two jobs in flight elsewhere. | The client matches on `job_id` and ignores every other event; asserted with an interleaved foreign event. |
| 9 | **`-o` pointing at a directory, or an unwritable path.** Discovered after the result is collected and charged. | The destination is opened and probed BEFORE the upload, so an unwritable path fails at exit 1 having spent nothing. |
| 10 | **SSE frames split across reads.** A naive line reader that assumes one frame per read drops events under load. | `bufio.Scanner` over the stream accumulating `event:`/`data:` pairs until the blank line, with a test feeding a frame in two writes. |

## Rollback

A new binary and a new package; nothing existing changes.

- **Remove it** with `git revert`; the router is untouched and no customer's existing integration
  depends on it.
- **No schema, no protocol, no data migration** in either direction.

## Follow-ups

- Operator to confirm whether the deployment's reverse proxy buffers responses; if it does, the
  deferred `--poll` mode becomes required rather than optional (Risk 2).
- Revisit batch submission once there is a caller with more than one file at a time.
