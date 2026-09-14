# Task ADR-0001-T9: Build a label-agnostic worker that forks any command and streams its stdout back

**Depends-on:** T7
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `runner.Runner.Run(ctx, Job) ([]string, error)`, `agent.Loop`, the `cmd/worker` binary
**Consumes:** `httpapi.New()` wire contract (T7), `core.Job`, `core.Err*` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the argv construction`, `the tmpfile cleanup`, `the subprocess timeout`

## Goal

Build one binary that serves **any** service — OCR, HTML stripping, crawling — by claiming
a job for its label, materialising whatever input the job has, forking a configured command
with the job's parameters as argv, and posting the command's stdout back as the result.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/runner/runner.go` | add | argv construction, fork, timeout, stdout parsing |
| `internal/runner/argv.go` | add | param validation and argv assembly — **the injection boundary** |
| `internal/runner/runner_test.go` | add | fork, parse, timeout, cleanup tests |
| `internal/runner/argv_test.go` | add | the injection and validation tests |
| `internal/agent/agent.go` | add | the SSE + claim + download + run + upload loop |
| `internal/agent/agent_test.go` | add | loop tests against a fake router |
| `cmd/worker/main.go` | add | flags and composition root |
| `cmd/worker/main_test.go` | add | binary-level test against a real router handler |
| `README.md` | edit | worker flags, the subprocess contract, the `--` caveat |

The package is `internal/runner`, **not** `internal/ocr`: naming it for OCR would make every
future service read as a special case of one of them. `cmd/worker/main.go` is what selects
`agent.Loop`; deleting that call is the mutation recorded here.

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write
   `argv_test.go` asserting that a param value containing shell metacharacters reaches the
   child as one literal argument, before any implementation (TDD red). [proof: acceptance]
2. [S2] `argv.Build(cmd string, inputPath string, params map[string]string) ([]string, error)`
   produces `[cmd, "-i", inputPath, "-o", "-", "--k", "v", …]`, omitting `-i <path>` entirely
   when the job has no blob. Keys are sorted, so the argv is deterministic and testable.
3. [S3] Validate each key against `^[a-z][a-z0-9-]{0,31}$` and reject the job otherwise. A
   key becomes a flag name, which is why the key is the constrained half. Values are **not**
   pattern-validated — they are data — but each is its own argv element and is never
   concatenated with anything.
4. [S4] `exec.CommandContext(ctx, argv[0], argv[1:]...)`. **No `sh -c`, no `exec.Command`
   with a joined string, no template expansion into a shell.** The child gets a clean
   environment plus an explicit allow-list of variables (`PATH`, `HOME`, `TMPDIR`), so a
   param can never become an environment variable either.
5. [S5] Enforce `--timeout` through the context, and on expiry kill the **process group**
   (`Setpgid`, then signal `-pgid`) — killing only the direct child leaves a grandchild
   holding the pipe and the worker blocks forever on a read that never ends.
6. [S6] Parse stdout as a JSON `[]string`. A non-zero exit, unparseable stdout, or stdout
   that is valid JSON but not an array of strings, are each a **job failure** reported to
   the router, never a worker crash — one malformed document must not take down a worker
   serving every other customer. Capture the tail of stderr into the failure reason, so the
   dashboard shows why.
7. [S7] `agent.Loop`: connect `GET /sse?label=<l>`; on `hello`, and on every `work` event,
   and on every `ping` as a safety net, try to fill idle slots by calling `POST /claim`
   until it returns `204`. Claiming on ping as well as on `work` is what makes a dropped
   event cost one ping interval rather than a stalled worker.
8. [S8] For each claimed job: if it has a blob, `GET /files/{id}` into
   `<tmpdir>/<job-id>` with `O_EXCL`; run; `POST /upload` with the pages or the error;
   then delete the temp file in a `defer`, on **every** path including panic and timeout.
9. [S9] `--slots N` bounds concurrent subprocesses with a buffered channel. The worker
   claims only while a slot is free, so the router's queue — not the worker's memory — is
   where work waits.
10. [S10] Reconnect the SSE stream with capped exponential backoff and jitter. A router
    restart must not leave every worker in the fleet reconnecting on the same tick.
11. [S11] `cmd/worker` flags: `--router`, `--token`, `--label` (default `ocr`), `--cmd`,
    `--tmpdir` (default `os.TempDir()`), `--slots` (2), `--timeout` (5m).

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/runner/... ./internal/agent/... -count=1 -race 2>&1 | tee /tmp/adr1-t9.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t9.out \
  && go test ./cmd/worker/... -count=1 -race 2>&1 | tee /tmp/adr1-t9b.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t9b.out
```

Red at authoring: neither package exists. The tests fork `/bin/sh` and small helper scripts
written into `t.TempDir()`, so nothing outside the checkout is required.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestArgvIncludesInputOnlyWithBlob` | `internal/runner/argv_test.go` | `-i <path>` is present for a blob job and **absent** for a params-only crawler job | — | S2 |
| `TestArgvIsDeterministic` | `internal/runner/argv_test.go` | the same params yield the same argv regardless of map iteration order | — | S2 |
| `TestArgvRejectsBadKeys` | `internal/runner/argv_test.go` | `UPPER`, `has space`, `--flag`, `a;b`, `` and a 33-char key are each rejected | — | S3 |
| `TestArgvValueWithShellMetacharsIsOneLiteralArgument` | `internal/runner/argv_test.go` | a value of ``; rm -rf / `id` $(id) && echo pwned`` arrives at the child as **one** argv element, byte-identical, and the child observes no extra arguments — **the injection guard** | — | S3, S4 |
| `TestRunExecutesWithoutShell` | `internal/runner/runner_test.go` | a param value of `$HOME` reaches the child unexpanded, proving no shell interpreted it | — | S4 |
| `TestRunEnvironmentIsAllowListed` | `internal/runner/runner_test.go` | the child sees only the allow-listed variables, and no param appears in its environment | — | S4 |
| `TestRunParsesJSONArray` | `internal/runner/runner_test.go` | a child printing `["a","b"]` yields two result units | — | S6 |
| `TestRunRejectsNonArrayStdout` | `internal/runner/runner_test.go` | `{"a":1}`, `"str"`, `[1,2]` and `not json` are each a job failure, not a panic | — | S6 |
| `TestRunNonZeroExitIsJobFailure` | `internal/runner/runner_test.go` | exit 1 yields an error carrying the stderr tail, and the worker survives | — | S6 |
| `TestRunTimeoutKillsProcessGroup` | `internal/runner/runner_test.go` | a child that spawns a grandchild holding stdout is fully killed at the timeout and `Run` returns — **fails if only the direct child is signalled**, which is the shape that hangs a worker forever | — | S5 |
| `TestRunDeletesTempFileOnEveryPath` | `internal/runner/runner_test.go` | success, non-zero exit, timeout and parse failure each leave the tmpdir empty | — | S8 |
| `TestLoopClaimsUntilEmpty` | `internal/agent/agent_test.go` | on one `work` event the agent claims repeatedly until the fake router answers 204 | — | S7 |
| `TestLoopClaimsOnPing` | `internal/agent/agent_test.go` | with the `work` event suppressed, a `ping` still drives a claim — the dropped-event safety net | — | S7 |
| `TestLoopRespectsSlots` | `internal/agent/agent_test.go` | with `--slots 2` and 5 jobs queued, at most 2 subprocesses run concurrently and the 3rd is claimed only as one finishes | — | S9 |
| `TestLoopReportsFailureAndContinues` | `internal/agent/agent_test.go` | a failing job is posted as an error and the agent goes on to claim the next | — | S6, S7 |
| `TestLoopReconnectsWithBackoff` | `internal/agent/agent_test.go` | the stream dropping causes reconnects with growing, jittered delays rather than a tight loop | — | S10 |
| `TestLoopSendsItsLabel` | `internal/agent/agent_test.go` | the SSE request carries `?label=<l>` and the agent is never handed a job of another label | — | S7, S11 |
| `TestWorkerAgainstRealRouter` | `cmd/worker/main_test.go` | against a real `httpapi` handler: a queued job is claimed, the blob downloaded, `/bin/sh` forked as the "service", and the result posted — the whole loop end to end | — | S7, S8, S11 |
| `TestCrawlerShapeNeedsNoBlob` | `cmd/worker/main_test.go` | a params-only job (`label=crawl`, `url=…`, no file) runs a command that reads `--url` and returns output — the generalisation working, not just permitted | — | S2, S8 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the nineteen tests above |
| 2 — something selects it | `cmd/worker/main.go` constructs the runner and calls `agent.Loop`; `TestWorkerAgainstRealRouter` goes red if that call is removed |
| 3 — the caller can discover it | `--help` lists every flag; the README documents the subprocess contract (`-i`, `-o -`, `--key value`, JSON `[]string` on stdout) and the `--` caveat for values beginning with `-` |
| 4 — it is used | it is half the product; `TestCrawlerShapeNeedsNoBlob` demonstrates a second service on the same binary |

## Mutation Log

## Invariants

- **No shell is ever invoked.** `exec.CommandContext` with an explicit argv, always.
- A param key is validated; a param value is data and never concatenated.
- The temp file is deleted on every exit path.
- A malformed job fails that job only; the worker process survives.
- The worker holds no durable state — it can be killed at any moment and the router's lease
  expiry recovers the job.
- One worker process serves exactly one label.

## Risks

- **`TestArgvValueWithShellMetacharsIsOneLiteralArgument` is the single most important test
  in the repository**, and it is easy to write so it cannot fail: asserting the argv *slice*
  proves only what the builder returned. It must fork a real child that **reports back how
  many arguments it received and their exact bytes**, so the assertion is about the process
  boundary rather than about a slice.
- **A value beginning with `-`** may still be read as a flag by the child. Not solved here;
  documented in the README and the ADR. Naming it is the mitigation, because a silent
  partial mitigation would read as a solved problem.
- **Process-group kill is platform-specific.** `Setpgid` is Unix; on Windows this needs a
  job object. The ADR targets Unix hosts; if that changes the timeout path needs a
  build-tagged implementation, and `TestRunTimeoutKillsProcessGroup` will fail loudly rather
  than silently leaking processes.
- **`t.TempDir()` on macOS returns a symlinked path** (`/var` → `/private/var`), so a test
  comparing the tmpfile path literally can fail for reasons unrelated to the code. Compare
  resolved paths.

## Stop Condition

Stop and ask before adding any second parameter-passing mechanism (stdin JSON, environment
variables, a template language). The ADR chose exactly one — argv — and a second mechanism
doubles the injection surface for a convenience nobody has requested.

## Out of Scope

- Any specific OCR, crawling or HTML-stripping implementation (permanent: boundary: the worker forks whatever `--cmd` names; the operator supplies the tool).
- Sandboxing the subprocess with containers or seccomp (deferred: docs/adr/BACKLOG.md).
- Workers serving several labels in one process (permanent: boundary: the operator chose one process per label on 2026-09-15).
- Windows support (deferred: docs/adr/BACKLOG.md).

## Verification Log
