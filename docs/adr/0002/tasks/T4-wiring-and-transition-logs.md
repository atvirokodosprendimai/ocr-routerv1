# Task ADR-0002-T4: Wire both into the binary and make a job's whole path readable

**Depends-on:** T1, T2, T3
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary: router, cmd, README)
**Owner:** unassigned
**Produces:** `router.Logger`, `router.Service.SetLogger()`, the transition log lines, the `--rate-*` and `--log-*` flags, the eviction tick
**Consumes:** `ratelimit.Limiter` (T1), `logging.Logger` (T2), `httpapi` middlewares and `Deps` fields (T3)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the eviction tick`, `the transition log call sites`, `the flag wiring`, `the redaction at the call site`

## Goal

Make the two packages reachable from the running binary — flags, limiter eviction on the existing
reaper tick, a logger on the writer — and emit one line per job transition carrying the duration in
the state just left, so a dead job can be read end to end.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/router/logger.go` | add | the `Logger` interface **at the consumer**, its nop default, and `SetLogger` — the same shape as T11's `Counter` |
| `internal/router/service.go` | edit | transition log calls at the points that already increment T11's counters |
| `internal/router/reaper.go` | edit | log each requeue, expiry and sweep |
| `internal/router/logger_test.go` | add | the transition-log tests, including redaction at the real call site |
| `cmd/router/main.go` | edit | **`--rate-client`, `--rate-worker`, `--rate-admin`, `--rate-burst`, `--rate-idle`, `--log-level`, `--log-format`** |
| `cmd/router/wire.go` | edit | **build the limiter and logger, pass them into `httpapi.Deps`, call `rt.SetLogger`, and call `EvictIdle` on the reaper tick** |
| `cmd/router/logging_test.go` | add | the composition-root tests |
| `README.md` | edit | the flags, the log shapes, the redaction rule, and the single-process caveat |

`wire.go` and `main.go` are the selecting files, and every unit test in T1–T3 passes with all of
those lines deleted. That is precisely why the mutants for this task bind there.

## Ordered Steps

1. [S1] Write the failing test first: drive a job through the binary and assert a transition line
   appears on the captured log output, before any logging call exists (TDD red). [proof: acceptance]
2. [S2] `router.Logger` is an interface **at the consumer** — `Transition(ctx, TransitionEvent)` —
   with a nop default set in `New`, so `router` still depends on nothing above it and every test
   written before this task compiles unchanged.
3. [S3] `TransitionEvent` carries `JobID`, `UserID`, `Label`, `From`, `To`, `Attempt`, `WorkerID`,
   `Stage`, `Params map[string]string`, and `InState time.Duration`. ⚠ The event carries the param
   MAP and the logging adapter passes it to `logging.Job()`, which emits keys only — so the value
   crosses one boundary inside the process and never reaches a line.
4. [S4] `InState` is computed from the job row's existing timestamp columns, never from an
   in-memory map: an in-memory start time is wrong after a restart, and a duration that is silently
   wrong is worse than an absent one.
5. [S5] Call sites are exactly the points that already increment T11's counters — upload, claim,
   complete, deliver, stage advance, and each reaper action. Pairing them means a transition can
   never be counted without being loggable, and the pairing is what a reviewer checks.
6. [S6] Flags: `--rate-client 10`, `--rate-worker 30`, `--rate-admin 30` (requests per second),
   `--rate-burst 20`, `--rate-idle 10m`, `--log-level info`, `--log-format json`. `0` on any rate
   flag means unlimited, which is ADR-0002's documented operational rollback.
7. [S7] `wire.go` builds the limiter and the logger, passes them into `httpapi.Deps`, and calls
   `rt.SetLogger`. An unparseable `--log-level` fails the boot with the error `logging.New`
   returned, rather than starting with logging the operator did not ask for.
8. [S8] `App.StartReaper` calls `limiter.EvictIdle()` on each tick, beside the existing
   `Router.Reap`. No second goroutine: the tick already exists, and a second timer is a second
   thing to leak.
9. [S9] README: the new flags, an example of each log shape, the redaction rule stated as a
   guarantee a customer can rely on, and the per-process limiter caveat beside the existing "the
   router is a single process" note.
   [proof: human: a reader checks each documented flag name against `--help` and each log example against a real emitted line]

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/router/... -count=1 -race 2>&1 | tee /tmp/adr2-t4.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr2-t4.out \
  && go test ./cmd/router/... ./internal/httpapi/... ./internal/ratelimit/... ./internal/logging/... -count=1 2>&1 | tee /tmp/adr2-t4r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr2-t4r.out
```

`internal/router` carries the verdict alone first; the composition root and the three packages this
task wires run second as regression. Red at authoring: `router.Logger` does not exist, so the new
test file does not compile and `^FAIL` matches.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestTransitionIsLoggedForEveryStateChange` | `internal/router/logger_test.go` | upload → claim → complete → deliver produces four transition events with the expected `From`/`To` pairs — red if any call site is dropped | — | S2, S5 |
| `TestTransitionCarriesInState` | `internal/router/logger_test.go` | a job claimed 300s after upload reports `InState` ≈ 300s on the queued→leased event, driven by the injected clock rather than a sleep | — | S4 |
| `TestInStateSurvivesARestart` | `internal/router/logger_test.go` | with a fresh `Service` over the same database, the first transition still reports a duration derived from the stored timestamp — red if `InState` is read from an in-memory map | — | S4 |
| `TestReaperActionsAreLogged` | `internal/router/logger_test.go` | a `Reap` that expires a lease emits a transition event naming the reaper as the actor | — | S5 |
| `TestServiceWithNoLoggerStillWorks` | `internal/router/logger_test.go` | a `Service` with no logger drives a full job — the nop default, on which every pre-existing test in the package depends | — | S2 |
| `TestTransitionLogNeverContainsAParamValue` | `internal/router/logger_test.go` | a job uploaded with `?url=https://user:hunter2@example.com/x` produces lines containing `url` and never `hunter2` — **at the real call site**, through the real logging adapter, and asserting the key IS present so "logged nothing" fails too | — | S3 |
| `TestBinaryAppliesTheRateLimit` | `cmd/router/logging_test.go` | a client driven past `--rate-burst` through the binary's own handler receives `429` — red if `wire.go` stops passing the limiter into `httpapi.Deps` | — | S7 |
| `TestRateFlagZeroDisablesLimiting` | `cmd/router/logging_test.go` | with the client rate at 0, 200 uploads are never throttled — the documented rollback, asserted rather than described | — | S6 |
| `TestEvictionRunsOnTheReaperTick` | `cmd/router/logging_test.go` | after idle keys and enough ticks, the limiter's `Len()` **drops** — red if the `EvictIdle` call is deleted from `StartReaper`, which no `internal/ratelimit` test can see | — | S8 |
| `TestBinaryLogsRequestsAndTransitions` | `cmd/router/logging_test.go` | one upload through the binary produces both a request line and a transition line on the captured output — red if either wiring line is deleted | — | S7 |
| `TestBinaryNeverLogsAParamValue` | `cmd/router/logging_test.go` | a real upload through the binary carrying `?url=…hunter2…` logs the key `url` and never the secret — through `wire.go`'s OWN adapter, which `internal/router`'s test cannot reach because it builds its own. Added after a mutant survived | — | S3 |
| `TestConfigFromReadsEveryNewFlag` | `cmd/router/logging_test.go` | running the real command with distinct prime values for all seven flags lands each one on the matching `Config` field — added after a mutant that read `--rate-burst` and discarded it survived the whole suite | — | S6 |
| `TestBadLogFormatFailsBoot` | `cmd/router/logging_test.go` | `--log-format logfmt` fails the boot rather than falling back | — | S7 |
| `TestBinaryLogsAreValidJSONLines` | `cmd/router/logging_test.go` | every emitted line beginning `{` parses as JSON, and at least one exists so the assertion is not vacuous | — | S7 |
| `TestBadLogLevelFailsBoot` | `cmd/router/logging_test.go` | `buildApp` with `LogLevel: "verbose"` returns an error and starts nothing — a silent fallback would leave the operator with logging they cannot discover is wrong | — | S7 |
| `TestCLIExposesTheNewFlags` | `cmd/router/logging_test.go` | all seven flags appear on the real command — a flag in `Config` that is not on the command line is unreachable by an operator | — | S6 |
| `TestWorkerKeepsPollingThroughA429` | `cmd/router/logging_test.go` | a worker agent facing a `429` on `/claim` backs off and claims successfully afterwards rather than exiting — ADR-0002 Risk 12 | — | S7 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the thirteen tests above |
| 2 — something selects it | `wire.go` passes the limiter into `httpapi.Deps` (`TestBinaryAppliesTheRateLimit`), calls `rt.SetLogger` (`TestBinaryLogsRequestsAndTransitions`), and calls `EvictIdle` on the tick (`TestEvictionRunsOnTheReaperTick`). Each goes red with its one line deleted, and none of T1–T3's tests can see any of them. |
| 3 — the caller can discover it | `--help` lists all seven flags (`TestCLIExposesTheNewFlags`); the README documents the log shapes and the redaction guarantee |
| 4 — it is used | `ocrr_requests_throttled_total{role}` shows throttling; nothing in this repository consumes the logs, which is the operator's deployment and is recorded honestly rather than claimed |

## Mutation Log

- 2026-09-15 · a8fb0be* · mutant killed · exit 1 · `cmd/router/wire.go` · nothing ever evicts, so the limiter map grows one entry per token for the process lifetime — the memory leak no internal/ratelimit test can see · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · covers:the eviction tick
- 2026-09-15 · a8fb0be* · mutant killed · exit 1 · `cmd/router/wire.go` · no job state change is ever logged, so a dead job leaves nothing saying which worker took it or how long it waited · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · covers:the transition log call sites
- 2026-09-15 · a8fb0be* · mutant killed · exit 1 · `internal/router/logger.go` · every transition reports an instantaneous duration, so a job wedged for forty minutes is indistinguishable from one that flew through · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · covers:the flag wiring
- 2026-09-15 · a8fb0be* · mutant survived · exit 0 · `cmd/router/wire.go` · the adapter smuggles a customer-supplied param VALUE into a key position, so a crawler URL carrying a password reaches every log line despite the logging package being unchanged · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · covers:the redaction at the call site
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```
- 2026-09-15 · a8fb0be* · mutant survived · exit 0 · `cmd/router/main.go` · the burst flag is read and discarded, so every role gets a zero burst which the limiter treats as unlimited — the flag exists, is documented, and does nothing · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · covers:the flag wiring
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```
- 2026-09-15 · a8fb0be* · mutant killed · exit 1 · `cmd/router/wire.go` · the binary adapter smuggles a customer-supplied param VALUE into a key position, so a crawler URL carrying a password reaches every log line despite the logging package being unchanged · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · covers:the redaction at the call site
- 2026-09-15 · a8fb0be* · mutant killed · exit 1 · `cmd/router/main.go` · the burst flag is read and discarded, so the flag exists, --help advertises it, and it does nothing · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · covers:the flag wiring

## Notes on the Mutation Log

Two mutants **survived** on the first run, and both were real coverage gaps rather than bad
mutants. Both were then closed with a test and re-run to a kill; the surviving rows stay in the log
because the run happened.

- **`the redaction at the call site` survived.** `internal/router/logger_test.go` builds its OWN
  adapter, which proves the logging package redacts and that an adapter *can* be written correctly
  — and proves nothing about `routerLogger` in `wire.go`, the one the binary actually uses. A
  mutant that made that adapter smuggle a value into a key position passed the whole suite. Closed
  by `TestBinaryNeverLogsAParamValue`, which drives a real upload through the binary.
- **`the flag wiring` survived.** Every test in `cmd/router` builds a `Config` struct directly, so
  `configFrom` — the function turning flags into that struct — was executed by nothing. A mutant
  that read `--rate-burst` and discarded it survived: the flag existed, `--help` advertised it, and
  it did nothing. Closed by `TestConfigFromReadsEveryNewFlag`, which runs the real command.

⚠ **One row's `covers:` is mislabelled and cannot be corrected.** The `InState: 0` mutant is
recorded as `covers:the flag wiring`; it is about `InState`, not flags. The Mutation Log is
append-only and tool-written, so the wrong binding stays and this note is the correction. The
mechanism it should have named is not in `Rests-on` at all — the durable lesson is to pick the
`--covers` name before running, since nothing downstream re-reads the `--why` text that makes the
mismatch obvious.

## Invariants

- `router` depends on no package above it: the `Logger` interface is declared at the consumer.
- A transition that increments a counter is also loggable, and vice versa.
- `InState` is derived from stored timestamps, never from process memory.
- No param value reaches a log line, asserted at the real call site and not only in `logging`.
- Eviction runs on the existing tick; no second goroutine is started.

## Risks

- **`TestEvictionRunsOnTheReaperTick` is the one that catches the memory leak**, and it is the
  easiest to write vacuously — asserting `EvictIdle` was called proves nothing. It must assert
  `Len()` DROPPED, which requires the fixture to first put entries in.
- **A redaction test inside `internal/logging` does not prove the router redacts.** The router
  builds the event; a call site passing values into a different field would pass every T2 test.
  `TestTransitionLogNeverContainsAParamValue` therefore runs through the real service.
- **`TestWorkerKeepsPollingThroughA429` needs the worker to actually meet a 429**, which means the
  fixture must set a burst low enough to trigger one and then assert a later success. If the burst
  is left at the default the test passes without ever throttling, proving nothing.
- **Seven new flags is a large surface to leave undiscoverable.** `TestCLIExposesTheNewFlags`
  enumerates the real command rather than a list maintained by hand.
- **The per-process limiter becomes wrong under replication** and no test can catch it. Recorded in
  the README beside the existing single-process note, which is the only place an operator would
  look.
- **Logging on every transition adds write-path work** — one JSON encode per transition, on the
  single writer's goroutine. At this system's transition rate that is negligible, but it is not
  zero and it is on the path that matters most.

## Stop Condition

Stop and ask before changing any default rate limit away from the values in ADR-0002. They are an
abuse ceiling chosen without traffic data, they are the one number in this record no test can
validate, and lowering one turns a limit into a product decision about what customers may do.

## Out of Scope

- Tracing spans across the router → worker round trip (deferred: `docs/adr/BACKLOG.md`).
- Logs delivered over SSE to the dashboard (deferred: `docs/adr/BACKLOG.md`).
- Structured logging inside `cmd/worker` (deferred: `docs/adr/BACKLOG.md`).
- Log retention and shipping (permanent: boundary: retention is valid for a deployment and this
  repository does not know the deployment).

## Verification Log
- 2026-09-15 · a8fb0be* · exit 0 · `set -o pipefail …` · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · ms:6910
- 2026-09-15 · a8fb0be* · exit 0 · `set -o pipefail …` · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · ms:5826
- 2026-09-15 · a8fb0be* · exit 0 · `set -o pipefail …` · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · ms:5832
- 2026-09-15 · a8fb0be* · exit 0 · `set -o pipefail …` · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · ms:5925
- 2026-09-15 · a8fb0be* · exit 0 · `set -o pipefail …` · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · ms:6265
- 2026-09-15 · a8fb0be* · exit 0 · `set -o pipefail …` · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · ms:5824
- 2026-09-15 · a8fb0be* · exit 0 · `set -o pipefail …` · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · ms:6426
- 2026-09-15 · a8fb0be* · exit 0 · `set -o pipefail …` · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · ms:5739
- 2026-09-15 · a8fb0be* · exit 0 · `set -o pipefail …` · acceptance-sha256:b75fdcaa599d7ac9a46118379d1f11c7d41ac056266ed78065eaed3ce14417a2 · ms:5608
- 2026-09-15 · human-observed · README (S9): flag names in the Flags block checked against `router --help`; the request and transition JSON examples checked against real lines captured by TestBinaryLogsRequestsAndTransitions
