# ADR-0009 Tasks

Implementation tasks for ADR-0009: Let a customer be unmetered by a flag on the user, not by a
sentinel in the balance. See the parent ADR for the decision.

**Source of truth:** the task files' `Depends-on` / `Produces` / `Consumes` / `Covers` headers. This
README is a derived index — when it disagrees with a task file, the task file wins and the README must
be regenerated.

## Execution Order

Five tasks, so sequential order only — no wave table and no DAG. T2, T3 and T4 depend only on T1 and
are independent of each other; they are listed in the order that makes the change externally
observable soonest, which is admission before pricing before the control.

| Order | Task | Depends-on |
|-------|------|------------|
| 1 | T1 | none |
| 2 | T2 | T1 |
| 3 | T3 | T1 |
| 4 | T4 | T1 |
| 5 | T5 | T1, T2, T3, T4 |

## Task Index

| ID | Title | Status | Covers | Acceptance |
|----|-------|--------|--------|------------|
| T1 | Carry an `unmetered` flag on the user, read and written with the user row | pending | — | `go test ./internal/store/ -run 'Unmetered\|Migrat' -count=1 …` |
| T2 | Admit an unmetered customer whose balance is not positive | pending | — | `go test ./internal/router/ -run 'Unmetered' -count=1 …` |
| T3 | Waive the charge on an unmetered delivery, and count what was waived | pending | — | `go test ./internal/router/ -run 'Unmetered\|Waiv' -count=1 …` |
| T4 | Give an administrator the toggle, and make the state visible on the table | pending | — | `templ generate && go test ./internal/web/... -run 'Unmeter' -count=1 …` |
| T5 | Say what unmetered means where an operator will read it, and close the backlog entry | pending | — | `grep -q 'unmetered' README.md && … adr-debt docs/adr` |

Status: `pending` | `partial` | `blocked` | `done`.

## Contract Coupling

| Producer | Contract | Consumer(s) | Ordering note |
|----------|----------|-------------|---------------|
| T1 | `core.User.Unmetered` and its persistence | T2, T3, T4 | T1 before all three; none of them compiles without the field |
| T3 | `ocrr_credits_waived_total` | T5 | T3 before T5 — T5 documents the series |
| T4 | `POST /admin/users/{id}/unmetered` | T5 | T4 before T5 — T5 documents the control |

## Notes

- **Run the whole project check before calling any task done:** `templ generate && go build ./... &&
  go vet ./... && test -z "$(gofmt -l .)" && go test ./... -count=1`. The per-task fences are scoped to
  the packages each task touches, which is what makes them fast; the project check is what catches a
  user-row change breaking a package nobody thought about.
- **T1 changes a column list and a scan list in two files.** Getting one without the other is a
  runtime error on every authenticated request, not a compile error. Its test table covers both scan
  sites for that reason.
- **T4's stop condition is a test it must NOT need to edit.** `cmd/router/monitoring_test.go` walks the
  real route table; needing to touch it means the new route escaped the authenticated group.
