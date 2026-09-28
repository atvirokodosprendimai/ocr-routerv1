# ADR-0010 Tasks

Implementation tasks for ADR-0010: Count each customer's work over four windows, from the job rows
already kept. See the parent ADR for the decision.

**Source of truth:** the task files' `Depends-on` / `Produces` / `Consumes` / `Covers` headers. This
README is a derived index — when it disagrees with a task file, the task file wins and the README must
be regenerated.

## Execution Order

Three tasks, strictly sequential: T1 produces the data, T2 is the only thing that makes T1 reachable,
T3 documents what both did.

| Order | Task | Depends-on |
|-------|------|------------|
| 1 | T1 | none |
| 2 | T2 | T1 |
| 3 | T3 | T1, T2 |

## Task Index

| ID | Title | Status | Covers | Acceptance |
|----|-------|--------|--------|------------|
| T1 | Count every customer's four windows in one query | done | — | `go test ./internal/store/ -run 'Usage' -count=1 …` |
| T2 | Render the counters as their own live fragment, and make the Customers page live | done | — | `templ generate && go test ./internal/web/ -run 'Usage\|Stream' -count=1 …` |
| T3 | Say what the counters mean, and file what this record deliberately did not build | done | — | `grep -q 'previous complete calendar month' README.md && … adr-debt docs/adr` |

Status: `pending` | `partial` | `blocked` | `done`.

## Contract Coupling

| Producer | Contract | Consumer(s) | Ordering note |
|----------|----------|-------------|---------------|
| T1 | `core.Usage`, `Repo.UsageByUser` | T2 | T1 before T2 — T2 does not compile without them |
| T2 | `views.Dashboard.Usage`, the `#usage` fragment | T3 | T2 before T3 — T3 documents what T2 renders |

## Notes

- **T1 ships deliberately unreachable and says so.** Its rung-2 row records the debt rather than
  claiming coverage; T2's `buildDashboard` call is what discharges it. A component finished and called
  by nothing is this codebase's most-repeated defect — four instances so far — and the only reason it is
  acceptable for one commit here is that the task file names it as owed.
- **T2's most valuable test is a NEGATIVE:** `TestTheStreamDoesNotPatchTheUserTable`. The obvious
  implementation patches the table the counters are about, which would re-render every row's edit
  inputs every fifteen seconds under an operator who is typing.
- **The cost lives on the SSE push path by M's choice**, against the recommendation recorded in the
  ADR's Alternatives. One aggregate bounded to ~62 days of job rows, per push, per connected
  administrator. T1's `TestUsageIsOneStatement` is what stops that quietly becoming 16 × customers.
- Run the whole project check before calling any task done: `templ generate && go build ./... &&
  go vet ./... && test -z "$(gofmt -l .)" && go test ./... -count=1`.
