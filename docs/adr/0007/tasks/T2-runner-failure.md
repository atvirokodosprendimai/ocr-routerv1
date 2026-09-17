# Task ADR-0007-T2: Keep the command's own output, both streams, and return the exit code as a value

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `runner.Failure` (`Kind`, `ExitCode *int`, `Output string`), returned by `Run` and `RunRaw`
**Consumes:** none
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `stdout surviving a failure`, `the exit code being a value rather than prose`, `each stream staying inside its byte budget`

## Goal

A failed command's exit code is a field, and the output an operator reads
contains what the tool actually printed — on both streams.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/runner/failure.go` | add | `Failure`, its `Kind`s, and its `Error()` — the formatting that used to be inline in `exec` |
| `internal/runner/runner.go` | edit | `exec` returns a `*Failure`; `Run`/`RunRaw` pass it up; the four failure sites name their kind |
| `internal/agent/agent.go` | edit | Reports `failure.Error()` as today's text; T3 adds the code. The ONLY caller of `Run`/`RunRaw` |
| `internal/runner/failure_test.go` | add | The failing tests below |

## Ordered Steps

1. [S1] Write the failing tests: `TestFailureKeepsStdout`, `TestFailureCarriesTheExitCodeAsAValue`, `TestTimeoutHasNoExitCode`. Confirm red. [proof: acceptance]
2. [S2] Add `runner.Failure` with `Kind` (`exit`, `timeout`, `output-limit`, `contract`), `ExitCode
   *int` and `Output string`, and an `Error()` that renders what `exec` renders today, so no
   existing message changes.
3. [S3] Build `Output` from BOTH streams, each labelled and each bounded by the existing
   `stderrTail` budget. ⚠ stderr FIRST: it is where a well-behaved tool explains itself, and an
   operator reading a truncated cell should meet the likeliest answer first.
4. [S4] Return `*Failure` from `exec`, `Run` and `RunRaw`; give each of the four failure sites its
   kind. `ExitCode` is set ONLY on the `exit` kind. [proof: acceptance]
5. [S5] Update `internal/agent` to report `failure.Error()`. Its behaviour is unchanged at this
   task's boundary — the code goes on the wire in T3. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/runner -run 'TestFailureKeepsStdout|TestFailureCarriesTheExitCodeAsAValue|TestTimeoutHasNoExitCode|TestOutputLimitHasNoExitCode|TestEachStreamIsBounded' -count=1 -v 2>&1 | tee /tmp/acc-0007-T2.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0007-T2.out \
  && go test ./internal/runner/... ./internal/agent/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestFailureKeepsStdout` | `internal/runner/failure_test.go` | A command that prints its explanation to STDOUT and exits non-zero produces a failure containing that text — the defect this task exists for, where the explanation was captured and thrown away | — | S3 |
| `TestFailureCarriesTheExitCodeAsAValue` | `internal/runner/failure_test.go` | `exit 3` yields `*ExitCode == 3` as a FIELD, not merely a substring of the message | — | S2, S4 |
| `TestTimeoutHasNoExitCode` | `internal/runner/failure_test.go` | A timed-out command yields `Kind == timeout` and `ExitCode == nil` — never 0, which is the code for success | — | S4 |
| `TestOutputLimitHasNoExitCode` | `internal/runner/failure_test.go` | An over-budget command yields `Kind == output-limit` and a nil code, for the same reason | — | S4 |
| `TestEachStreamIsBounded` | `internal/runner/failure_test.go` | A command flooding BOTH streams produces an `Output` bounded per stream, so one chatty stream cannot crowd out the other's explanation | — | S3 |

<!-- TestUnitsArmUnchanged (internal/runner/raw_test.go) is deliberately NOT a row here. It is
ADR-0006's, it runs in this fence's REGRESSION segment, and it is not a test T2 adds. That it stays
green IS the evidence that Failure.Error() renders what the runner rendered before — which is this
task's first invariant — but claiming it as coverage T2 produced would be claiming someone else's. -->

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestFailureCarriesTheExitCodeAsAValue` |
| 2 — something selects it | `exec`'s exit-status site is the only place `ExitCode` is set; making it set nil there turns `TestFailureCarriesTheExitCodeAsAValue` red — the mutation to record |
| 3 — the caller can discover it | `Failure` is an exported type with exported fields, and `internal/agent` is its one consumer. Its `Error()` keeps the existing string shape so nothing reading the message has to learn anything |
| 4 — it is used | Nothing measures this yet; T3 puts it on the wire and T4 shows it |

## Mutation Log

- 2026-09-17 · d22a779* · mutant killed · exit 1 · `internal/runner/failure.go` · the command own explanation on stdout is discarded again — the exact defect ADR-0007 was opened for · acceptance-sha256:2d248f59d36ec757c0eb43c9f8f3fefdbe7cd1df717661dd1d55b0313f8ad77b · covers:stdout surviving a failure
- 2026-09-17 · d22a779* · mutant inconclusive · exit 1 · `internal/runner/runner.go` · the one kind that HAS a code stops carrying it, so nothing downstream can filter or group by it · acceptance-sha256:2d248f59d36ec757c0eb43c9f8f3fefdbe7cd1df717661dd1d55b0313f8ad77b · covers:the exit code being a value rather than prose
  ```
  the fence failed on a build/parse error, not an assertion
  ```
- 2026-09-17 · d22a779* · mutant killed · exit 1 · `internal/runner/failure.go` · neither stream is bounded, so one chatty stream can carry an unbounded payload into the job row and the dashboard · acceptance-sha256:2d248f59d36ec757c0eb43c9f8f3fefdbe7cd1df717661dd1d55b0313f8ad77b · covers:each stream staying inside its byte budget
- 2026-09-17 · d22a779* · mutant killed · exit 1 · `internal/runner/runner.go` · every non-zero exit is recorded as 0 — the code for SUCCESS — so the field exists, is always wrong, and reads as a clean exit · acceptance-sha256:2d248f59d36ec757c0eb43c9f8f3fefdbe7cd1df717661dd1d55b0313f8ad77b · covers:the exit code being a value rather than prose

## Invariants

- `Failure.Error()` renders what the runner renders today. An operator's existing
  message does not change in this task; only what it CONTAINS grows.
- `ExitCode` is non-nil ONLY for `Kind == exit`. Every other kind reached the
  failure path without an exit status.
- Each stream keeps its own `stderrTail` budget. One chatty stream must not be
  able to crowd out the other's explanation, which a single shared budget allows.
- `parseUnits` and ADR-0006's raw arm are untouched.

## Risks

- ⚠ **This widens what an operator sees, and the record should say so.** A
  worker's stdout can contain customer document content. The audience is
  unchanged (admin-only, already shown stderr) and the budget is unchanged, but
  the SURFACE grows from one stream to two. ADR-0002's rule that param VALUES are
  never logged is untouched and remains the boundary for the log; this is the
  dashboard's Detail, which already showed worker-supplied text.
- `Run`'s signature change reaches `internal/agent` only — swept with
  `git grep -n "\.Run(ctx\|\.RunRaw(ctx" -- '*.go'`.

## Stop Condition

Stop if labelling the two streams turns out to change `Failure.Error()`'s text in
a way an existing test depends on: that would mean a message somebody is parsing,
which is a contract nobody declared, and the owner should decide before it is
altered.

## Out of Scope

- Putting the code on the wire or in the database — T1 and T3.
- Raising the byte budget (permanent: boundary: 2000 bytes per stream has been sufficient; changing it is a number, not a decision).

## Verification Log
- 2026-09-17 · d22a779* · exit 1 · `set -o pipefail …` · acceptance-sha256:2d248f59d36ec757c0eb43c9f8f3fefdbe7cd1df717661dd1d55b0313f8ad77b · ms:438
  ```
  --- last 10 line(s) of stdout (of 12 after folding 12 raw)
  internal/runner/failure_test.go:38:22: undefined: runner.FailureExit
  internal/runner/failure_test.go:39:49: undefined: runner.FailureExit
  internal/runner/failure_test.go:52:18: undefined: runner.AsFailure
  internal/runner/failure_test.go:56:22: undefined: runner.FailureTimeout
  internal/runner/failure_test.go:57:49: undefined: runner.FailureTimeout
  internal/runner/failure_test.go:71:18: undefined: runner.AsFailure
  internal/runner/failure_test.go:75:22: undefined: runner.FailureOutputLimit
  internal/runner/failure_test.go:76:84: undefined: runner.FailureOutputLimit
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/runner [build failed]
  FAIL
  ```
- 2026-09-17 · d22a779* · exit 0 · `set -o pipefail …` · acceptance-sha256:2d248f59d36ec757c0eb43c9f8f3fefdbe7cd1df717661dd1d55b0313f8ad77b · ms:14618
- 2026-09-17 · d22a779* · exit 0 · `set -o pipefail …` · acceptance-sha256:2d248f59d36ec757c0eb43c9f8f3fefdbe7cd1df717661dd1d55b0313f8ad77b · ms:13928
- 2026-09-17 · d22a779* · exit 0 · `set -o pipefail …` · acceptance-sha256:2d248f59d36ec757c0eb43c9f8f3fefdbe7cd1df717661dd1d55b0313f8ad77b · ms:13692
- 2026-09-17 · d22a779* · exit 0 · `set -o pipefail …` · acceptance-sha256:2d248f59d36ec757c0eb43c9f8f3fefdbe7cd1df717661dd1d55b0313f8ad77b · ms:14354
