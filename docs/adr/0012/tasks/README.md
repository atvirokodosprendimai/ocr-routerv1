# ADR-0012 Tasks

Implementation tasks for ADR-0012: Publish a Session handle so one SSE stream can carry many jobs
across process restarts. See the parent ADR for the decision.

**Source of truth:** the task files' `Depends-on` / `Produces` / `Consumes` / `Covers` headers.
This README is a derived index — when it disagrees with a task file, the task file wins and the
README must be regenerated. `adr-lint` fails when the README lists a task with no file or omits
an existing task file; wave/order drift against Depends-on + Consumes edges is caught by
`adr-lint` (cycles too — the wave table must be a valid topological leveling of the task
DAG); Covers-column drift is caught at review. Regenerate rather than hand-edit.

## Execution Order

Four tasks, strictly sequential — no wave table and no DAG at this size.

| Order | Task | Depends-on |
|-------|------|------------|
| 1 | T1 | none |
| 2 | T2 | T1 |
| 3 | T3 | T2 |
| 4 | T4 | T3 |

The order is forced by the contract edges, not by preference: T2's methods hang off T1's type, T3
replaces `Submit`'s body with calls to T2's methods, and T4 documents the surface only once T3 has
settled what `Submit` is. The one place a different order was considered and rejected: `Services`
(T4) depends on nothing but `Config` and could run first. It is last anyway, because it is the only
task with no consumer inside this record — putting it early would spend the first commit on the
least load-bearing symbol, and ADR-0012's served path is the Session, not the label list.

## Task Index

| ID | Title | Status | Covers | Acceptance |
|----|-------|--------|--------|------------|
| T1 | The Session handle: `Open`, `Events`, `Backlog`, `Close`, `ErrNotOpen`, `RetryAfter` | pending | — | `set -o pipefail; go test ./client/... -race -run 'TestOpen\|TestBacklogIsACopy\|TestEvents\|TestSessionClose\|TestZeroValueSession\|TestClosedSession\|TestRetryAfter' …` |
| T2 | `Upload` and `Collect` on the Session, and `ErrNotReady` | pending | — | `set -o pipefail; go test ./client/... -race -run 'TestSessionUpload\|TestSessionCollect\|TestCollectNotReady\|TestConcurrentUploadIsSafe' …` |
| T3 | Re-implement `Submit` over `Session` with the existing tests unedited | pending | — | `set -o pipefail; git diff --exit-code -- client/client_test.go client/raw_test.go && go test ./client/... ./cmd/client/... -count=1 -race …` |
| T4 | `Services`, and document both shapes so neither rots | pending | — | `set -o pipefail; go test ./client/... -race -run 'TestServices\|TestReadmeClientExampleNamesRealSymbols\|TestEnforcedByPointersResolve\|ExampleOpen' … && grep -q -- '--- PASS: ExampleOpen' …` |

Status: `pending` | `partial` | `blocked` | `done`.

- `pending` — not started, or started and carrying no evidence yet.
- `partial` — genuinely part-done: some of the work has landed and some has not. It is a
  status with OBLIGATIONS, not a softer `pending`: everything its landed evidence claims is
  checked exactly as hard as for a `done` task, so a partial task with a passing Acceptance
  fence still owes a killed mutant. What it does not owe is a `done` row's exit-0 evidence,
  because it claims no completion.
- `blocked` — waiting on something outside this repository. Say what, in the task's
  `**Blocked-on:**` header, as an event a later reader can check HAS HAPPENED — ideally a
  command that exits 0 once it has.
- `done` — finished, with tool-written acceptance and mutation evidence to match.

## Contract Coupling

Derived from task-file `Produces`/`Consumes` headers.

| Producer | Contract | Consumer(s) | Ordering note |
|----------|----------|-------------|---------------|
| T1 | `Session`, `Open()`, `Events()`, `Backlog()`, `Close()`, `Event`, `ErrNotOpen`, `RetryableError.RetryAfter` | T2, T3, T4 | T1 before all others — they do not compile without the type. |
| T2 | `(*Session).Upload()`, `(*Session).Collect()`, `ErrNotReady` | T3, T4 | T2 before T3 — T3's `Submit` body calls both methods. |
| T3 | `Submit` re-implemented over `Session` | T4 | T3 before T4 — T4's `Example` and README document the final `Submit`. |

## Notes

- **The whole record's safety net is two files nobody may touch.** `client/client_test.go` and
  `client/raw_test.go` (776 lines, `package client_test`) must pass byte-unchanged through all four
  tasks. T3 asserts this mechanically with `git diff --exit-code` inside its fence, because that is
  the only task that rewrites a function those tests cover.
- **Every fence runs `-race`.** `Session` adds a goroutine (`pump`) and a mutex (`Close`) to a
  package that previously had one goroutine and no shared mutable state, and a data race here would
  otherwise surface as a flake in a consumer's batch run rather than here.
- ⚠ **No task can observe per-test red at its own step 1.** Go compiles a test package as a unit, so a
  test referencing a symbol that does not exist yet makes EVERY test in `client_test` unbuildable. Each
  task's step 1 therefore records the BUILD failure and the identifiers it names, and recovers per-test
  verdicts one step later once the package compiles. An earlier draft asked for per-test red and was
  not achievable as written.
- ⚠ **Two pre-existing checks are driven red and back to green by T4, not merely run**:
  `TestReadmeClientExampleNamesRealSymbols` (its surface list at `client/readme_test.go:28` is
  hand-written, so a new README symbol fails it until the list is extended) and
  `TestEnforcedByPointersResolve` (its record list at `client/public_test.go:121-128` is hardcoded and
  does not include ADR-0012, so this record's own `Enforced-by:` pointer is unchecked until T4 adds it).
- No task edits `cmd/client`. If one appears to need to, the re-implementation is not
  behaviour-preserving — see T3's Stop Condition.
- The motivating consumer (`e-tar-crawlerv1`) is in another repository, so no task here can show the
  duplication actually disappearing. ADR-0012 §Consequences says so explicitly rather than letting a
  green fence imply it.
