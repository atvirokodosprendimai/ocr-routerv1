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
| `TestArgvIncludesInputOnlyWithBlob` | `internal/runner/runner_test.go` | `-i <path>` is present for a blob job and **absent** for a params-only crawler job | — | S2 |
| `TestArgvIsDeterministic` | `internal/runner/runner_test.go` | the same params yield the same argv across 50 calls, regardless of map iteration order | — | S2 |
| `TestArgvRejectsBadKeys` | `internal/runner/runner_test.go` | `UPPER`, `has space`, `--flag`, `a;b`, empty and a 33-char key are each rejected | — | S3 |
| `TestArgvValueWithShellMetacharsIsOneLiteralArgument` | `internal/runner/runner_test.go` | **THE INJECTION GUARD.** A value of ``; rm -rf <canary> `touch` $(touch) && echo pwned \| tee`` reaches a REAL forked child as exactly one argv element, byte-identical, with no extra arguments — and a canary file on disk is untouched. It asserts at the process boundary, not on the argv slice, because a slice assertion proves only what the builder did | — | S3, S4 |
| `TestRunExecutesWithoutShell` | `internal/runner/runner_test.go` | `$HOME` reaches the child unexpanded — an expanded value would mean a shell ran | — | S4 |
| `TestRunEnvironmentIsAllowListed` | `internal/runner/runner_test.go` | a secret exported in the worker's environment is `absent` inside the child | — | S4 |
| `TestRunParsesJSONArray` | `internal/runner/runner_test.go` | `["a","b"]` yields two units | — | S6 |
| `TestRunRejectsNonArrayStdout` | `internal/runner/runner_test.go` | an object, a bare string, a number array, prose and empty output are each a job failure, not a panic | — | S6 |
| `TestRunNonZeroExitIsJobFailureWithStderr` | `internal/runner/runner_test.go` | exit 3 yields an error naming the status AND carrying the stderr tail, so the dashboard shows a reason | — | S6 |
| `TestRunTimeoutKillsProcessGroup` | `internal/runner/runner_test.go` | after the timeout nothing from the job is still running. ⚠ It does NOT distinguish the group kill from a child-only kill on darwin — see §Risks; the corresponding mutant is recorded as SURVIVED | — | S5 |
| `TestRunRespectsCallerCancellation` | `internal/runner/runner_test.go` | the caller's cancellation wins over a long `--timeout`, so a shutting-down worker does not hang | — | S5 |
| `TestRunCapsStdout` | `internal/runner/runner_test.go` | a command emitting 10MB is bounded rather than exhausting the worker | — | S6 |
| `TestRunMissingCommand` | `internal/runner/runner_test.go` | a nonexistent `--cmd` is a job failure, not a crash | — | S6 |
| `TestRunEmptyCommandIsRejected` | `internal/runner/runner_test.go` | no configured command is `core.ErrInvalidParam` | — | S2 |
| `TestRunReadsInputFile` | `internal/runner/runner_test.go` | a real tool reads `-i` and emits one unit per line | — | S2, S6 |
| `TestParseUnitsAcceptsEmptyArray` | `internal/runner/runner_test.go` | `[]` is a legitimate outcome — a service can correctly produce nothing | — | S6 |
| `TestAgentClaimsRunsAndReports` | `internal/agent/agent_test.go` | on a `work` event the agent claims, downloads the blob, runs the program and posts the units | — | S7, S8 |
| `TestAgentClaimsWithoutBlob` | `internal/agent/agent_test.go` | a params-only job runs with no `-i` and no download | — | S8 |
| `TestAgentClaimsOnPing` | `internal/agent/agent_test.go` | with NO `work` event ever sent, a `ping` alone drives a claim — the dropped-event safety net | — | S7 |
| `TestAgentDrainsOnConnect` | `internal/agent/agent_test.go` | work queued before the worker existed is claimed on connect, with no event at all | — | S7 |
| `TestAgentRespectsSlots` | `internal/agent/agent_test.go` | with 5 jobs and 2 slots, peak observed concurrency is 2 and all 5 still complete — which also proves the agent re-drains as slots free rather than stalling | — | S9 |
| `TestAgentReportsFailureAndContinues` | `internal/agent/agent_test.go` | a failing job is reported with its stderr and the agent goes on to the next — one bad document must not stop a worker | — | S6, S7 |
| `TestAgentDeletesTempFileOnEveryPath` | `internal/agent/agent_test.go` | success, non-zero exit and unparseable output each leave the tmpdir empty | — | S8 |
| `TestAgentReconnectsAfterStreamDrop` | `internal/agent/agent_test.go` | a dropped stream is reconnected rather than ending the worker | — | S10 |
| `TestAgentStopsOnContextCancel` | `internal/agent/agent_test.go` | `Run` returns promptly and nil when its context is cancelled | — | S10 |
| `TestCLIExposesEveryFlag` | `cmd/worker/main_test.go` | every setting has a command-line flag — a setting an operator cannot reach is not configuration | — | S11 |
| `TestTokenCanComeFromTheEnvironment` | `cmd/worker/main_test.go` | `--token` reads `OCR_WORKER_TOKEN`, because a token on the command line is visible in the process table | — | S11 |
| `TestAllowedEnvExcludesEverythingElse` | `cmd/worker/main_test.go` | the worker's own router token and an unrelated secret are both absent from the child's environment, while `PATH` is present — built through the same `buildRunner` the binary uses | — | S4, S11 |
| `TestUsageDocumentsTheSubprocessContract` | `cmd/worker/main_test.go` | `--help` states `-i`, `-o -`, the JSON-array contract and the leading-`-` caveat — rung 3, since none of it is inferable from the flag names | — | S11 |
| `TestWorkerRunsAgainstARealScript` | `cmd/worker/main_test.go` | the runner the binary builds executes a real program and returns its units | — | S11 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the nineteen tests above |
| 2 — something selects it | `cmd/worker/main.go` constructs the runner and calls `agent.Loop`; `TestWorkerAgainstRealRouter` goes red if that call is removed |
| 3 — the caller can discover it | `--help` lists every flag; the README documents the subprocess contract (`-i`, `-o -`, `--key value`, JSON `[]string` on stdout) and the `--` caveat for values beginning with `-` |
| 4 — it is used | it is half the product; `TestCrawlerShapeNeedsNoBlob` demonstrates a second service on the same binary |

## Mutation Log

- 2026-09-15 · ab1c0cb* · mutant killed · exit 1 · `internal/runner/argv.go` · A value concatenated with its flag can split into several arguments at the process boundary; each value must be its own argv element. · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06 · covers:the argv construction
- 2026-09-15 · ab1c0cb* · mutant killed · exit 1 · `internal/runner/argv.go` · A key becomes a flag NAME; without validation a client supplies its own flags to the operator's program. · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06
- 2026-09-15 · ab1c0cb* · mutant survived · exit 0 · `internal/runner/runner.go` · Killing only the direct child leaves a grandchild holding stdout, and the worker then blocks forever on a read that never ends. · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06 · covers:the subprocess timeout
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```
- 2026-09-15 · ab1c0cb* · mutant survived · exit 0 · `internal/runner/runner.go` · Killing only the direct child leaves a grandchild alive on the worker host — one orphan per timed-out job, holding the inherited stdout pipe. · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06 · covers:the subprocess timeout
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```

## Invariants

- **No shell is ever invoked.** `exec.CommandContext` with an explicit argv, always.
- A param key is validated; a param value is data and never concatenated.
- The temp file is deleted on every exit path.
- A malformed job fails that job only; the worker process survives.
- The worker holds no durable state — it can be killed at any moment and the router's lease
  expiry recovers the job.
- One worker process serves exactly one label.

## Risks

⚠ **A SURVIVED MUTANT IS RECORDED IN THE MUTATION LOG AND IS LEFT THERE DELIBERATELY.**
Replacing the process-group kill with a child-only kill survives the fence. Measured
2026-09-15 on darwin/arm64 with a standalone probe: a `( sleep 1; touch marker ) &`
grandchild is reaped in BOTH arms — with no kill at all the marker appears, so the fixture
works, but `cmd.Process.Kill()` alone is enough to stop it on this platform. The fixture
therefore cannot exhibit the defect, which is not the same as the code being right.

The group kill is kept because it is correct where the platform does not do the work for
us: a program that genuinely detaches — double-forks, or calls `setsid` — leaves an orphan
holding the inherited stdout pipe, one per timed-out job. Reproducing that shape portably
needs a helper binary, which is more machinery than this property is worth today. Recorded
rather than hidden, and re-check it if this ever runs on Linux in CI.

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
- 2026-09-15 · ab1c0cb* · exit 0 · `set -o pipefail …` · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06 · ms:13420
- 2026-09-15 · ab1c0cb* · exit 0 · `set -o pipefail …` · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06 · ms:11131
- 2026-09-15 · ab1c0cb* · exit 0 · `set -o pipefail …` · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06 · ms:11163
- 2026-09-15 · ab1c0cb* · exit 0 · `set -o pipefail …` · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06 · ms:10939
- 2026-09-15 · ab1c0cb* · exit 0 · `set -o pipefail …` · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06 · ms:12311
- 2026-09-15 · ab1c0cb* · exit 0 · `set -o pipefail …` · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06 · ms:12256
- 2026-09-15 · ab1c0cb* · exit 0 · `set -o pipefail …` · acceptance-sha256:63955be0437b23a14cb54b6ac3aff2cd75cb6ec612ae702ca7dae1c6afc15e06 · ms:16447
