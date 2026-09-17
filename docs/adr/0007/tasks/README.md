# ADR-0007 Tasks

Implementation tasks for ADR-0007: Carry a failed job's exit code and output as
data, and give failures a place to be seen. See the parent ADR for the decision.

**Source of truth:** the task files' `Depends-on` / `Produces` / `Consumes` /
`Covers` headers. This README is a derived index — when it disagrees with a task
file, the task file wins and the README must be regenerated.

## Execution Order

Four tasks, sequential. No wave table at this size.

| Order | Task | Depends-on |
|-------|------|------------|
| 1 | T1 | none |
| 2 | T2 | none |
| 3 | T3 | T1, T2 |
| 4 | T4 | T3 |

T1 and T2 are independent — one is the column, the other is what produces the
value — and T3 is the join that carries the value into the column. Doing them in
either order is fine; T3 needs both.

## Task Index

| ID | Title | Status | Covers | Acceptance |
|----|-------|--------|--------|------------|
| T1 | Store a failed job's exit code, nullable so "no exit" stays distinct from 0 | done | — | `go test ./internal/store -run 'TestExitCodeRoundTripsThroughTheJobRow\|…3 tests'` |
| T2 | Keep the command's own output, both streams, and return the exit code as a value | done | — | `go test ./internal/runner -run 'TestFailureKeepsStdout\|…5 tests'` |
| T3 | Carry the exit code from the worker to the job row and the log line | done | — | `go test ./internal/httpapi ./internal/router -run 'TestExitCodeSurvivesToTheJobRow\|…4 tests'` |
| T4 | Give failures a place to be seen, and make the detail readable | done | — | `templ generate && go test ./internal/web -run 'TestFailedFilterShowsOnlyFailures\|…'` |

Status: `pending` | `partial` | `blocked` | `done`.

## Contract Coupling

| Producer | Contract | Consumer(s) | Ordering note |
|----------|----------|-------------|---------------|
| T1 | `jobs.exit_code` + `core.Job.ExitCode *int` | T3, T4 | T1 before T3 |
| T2 | `runner.Failure` (`Kind`, `ExitCode`, `Output`) | T3 | T2 before T3 — `Run`/`RunRaw` change signature, and `internal/agent` is the only caller |
| T3 | `exit_code` on the worker report, `Router.Fail(…, exitCode, …)`, the log attribute | T4 | T3 before T4 — T4 shows what T3 persists |

## Notes

- **T4 carries the UX gate.** It is the only task touching a user-facing surface,
  so the project's UI idioms load before any markup: datastar `data-on:` /
  `data-bind:` with no `<form>` tag, `templ generate` after every `.templ` edit,
  and never a hand edit of `*_templ.go`.
- **Three signature changes, one caller each.** `Run`/`RunRaw` (T2, caller
  `internal/agent`), `Router.Fail` (T3, callers `internal/httpapi` and the
  reaper), and the two repo failure writers (T1). The compiler finds all of them;
  each task's Risks records the sweep command.
- **The nullable column is the load-bearing decision.** A timeout has no exit
  code, and 0 means success. T1, T3 and T4 each assert the distinction at their
  own layer, because collapsing `*int` to `int` at any hop restores the confusion
  silently.
- **Every task's first step is the TDD red run**, and `adr-verify`'s first
  Verification Log entry for each should be that failing run.
