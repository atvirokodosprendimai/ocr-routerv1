# ADR-0008 Tasks

Implementation tasks for ADR-0008: Stop charging a worker restart against a job's
retry budget. See the parent ADR for the decision.

**Source of truth:** the task files' `Depends-on` / `Produces` / `Consumes` /
`Covers` headers. This README is a derived index — when it disagrees with a task
file, the task file wins and the README must be regenerated.

## Execution Order

Three tasks, strictly sequential — each consumes what the previous produces.

| Order | Task | Depends-on |
|-------|------|------------|
| 1 | T1 | none |
| 2 | T2 | T1 |
| 3 | T3 | T2 |

T1 adds the counter and the writer; T2 makes the reaper spend it instead of the
failure budget, which is the fix for "three restarts killed my job"; T3 adds the
cooperative release, which is the fix for "the client waited five minutes".

**T2 is the one that matters most.** If only one lands, it should be that one:
it stops a deploy destroying customer work. T3 removes the wait, which is
irritating rather than destructive.

## Task Index

| ID | Title | Status | Covers | Acceptance |
|----|-------|--------|--------|------------|
| T1 | Count abandonment separately from failure | done | — | `go test ./internal/store -run 'TestReclaimIncrementsReclaimsNotAttempts\|…'` |
| T2 | Make the reaper spend the abandonment budget, not the failure one | done | — | `go test ./internal/router -run 'TestReclaimDoesNotSpendAnAttempt\|…'` |
| T3 | Let a worker hand its leases back when it is told to stop | pending | — | `go test ./internal/httpapi ./internal/agent -run 'TestReleaseRequeuesWithoutSpendingEitherBudget\|…'` |

Status: `pending` | `partial` | `blocked` | `done`.

## Contract Coupling

| Producer | Contract | Consumer(s) | Ordering note |
|----------|----------|-------------|---------------|
| T1 | `jobs.reclaims`, `core.Job.Reclaims`, `Repo.ReclaimJob` | T2, T3 | T1 before T2 — T2 is what first calls the writer |
| T2 | `Service.reclaimJob` + `Config.MaxReclaims` | T3 | T2 before T3 — T3's release must be distinguishable from T2's reclaim, and it is defined by contrast |

## Notes

- **T1 deliberately ships a writer with no caller.** That is normally this
  corpus's most-shipped defect, and it is bounded here by T2 landing immediately
  after. T1's Reachability rung 2 says so explicitly rather than claiming
  coverage it does not have.
- **The lease guard appears in three places after T3** — `Complete`, `Fail` and
  `ReleaseLease`. That is the thing to review hardest: a release without it lets
  any worker requeue a job another is running.
- **Two budgets is the decision, and merging them is the natural
  simplification.** T2's tests fail in opposite directions on purpose, so one
  counter with a flag cannot pass both.
- **Every task's first step is the TDD red run**, and `adr-verify`'s first
  Verification Log entry for each should be that failing run.
- **The immediate workaround, needing none of this:** run the router with
  `--lease 30s` in development. It trades the five-minute stall for a shorter
  one, and carries its own risk — a lease shorter than the longest legitimate
  command lets the reaper requeue work that is still running.
