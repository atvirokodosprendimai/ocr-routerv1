# ADR-0001 Tasks

Implementation tasks for ADR-0001: Route OCR work to internet-resident workers over an SSE
command bus with page-metered credits. See the parent ADR for the decision.

**Source of truth:** the task files' `Depends-on` / `Produces` / `Consumes` / `Covers`
headers. This README is a derived index — when it disagrees with a task file, the task file
wins and the README must be regenerated. Regenerate rather than hand-edit.

## Execution Order

| Wave | Tasks | Depends-on |
|------|-------|------------|
| 1 | T1 | none |
| 2 | T2, T4, T5 | T1 |
| 3 | T3, T6 | T2 (T3); T2, T4, T5 (T6) |
| 4 | T7 | T3, T6 |
| 5 | T8, T9 | T7 |
| 6 | T10 | T8 |
| 7 | T11 | T8, T10 |

```
T1 ──┬── T2 ──┬── T3 ──┐
     │        │        ├── T7 ──┬── T8 ── T10
     ├── T4 ──┼── T6 ──┘        └── T9
     └── T5 ──┘
```

## Task Index

| ID | Title | Status | Covers | Acceptance |
|----|-------|--------|--------|------------|
| T1 | Module skeleton and domain kernel | done | — | `go build ./... && go test ./internal/core/...` |
| T2 | SQLite two-handle store and schema migration | done | — | `go test ./internal/store/...` |
| T3 | Users, hashed bearer tokens, authentication | done | — | `go test ./internal/identity/...` |
| T4 | Blob store on disk and TTL result store | done | — | `go test ./internal/blob/... ./internal/results/...` |
| T5 | In-process per-label event bus | done | — | `go test ./internal/bus/... -race` |
| T6 | Router service: the single writer | done | — | `go test ./internal/router/... -race` |
| T7 | HTTP boundary: upload, claim, files, SSE, services | done | — | `go test ./internal/httpapi/... -race` |
| T8 | cmd/router binary and end-to-end proof | done | — | `go test ./cmd/router/... -race` |
| T9 | Generic subprocess runner, worker agent, cmd/worker | done | — | `go test ./internal/runner/... ./internal/agent/... -race` |
| T10 | Admin dashboard over templ and datastar | done | — | `go test ./internal/web/...` |
| T11 | Liveness endpoint and Prometheus metrics | done | — | `go test ./internal/monitor/... -race` |

Status: `pending` | `partial` | `blocked` | `done`.

- `pending` — not started, or started and carrying no evidence yet.
- `partial` — genuinely part-done; its landed evidence is checked exactly as hard as a
  `done` task's, so a passing Acceptance fence still owes a killed mutant.
- `blocked` — waiting on something outside this repository, named in `**Blocked-on:**`.
- `done` — finished, with tool-written acceptance and mutation evidence to match.

## Contract Coupling

| Producer | Contract | Consumer(s) | Ordering note |
|----------|----------|-------------|---------------|
| T1 | `core.Job`, `core.User`, `core.Token`, `core.Err*` | T2, T3, T4, T5, T6, T7, T9 | T1 before all |
| T2 | `store.Open()`, `store.Repo` | T3, T6 | T2 before T3, T6 |
| T3 | `identity.Service.Authenticate()` | T7 | T3 before T7 |
| T4 | `blob.Store`, `results.Store` | T6, T7 | T4 before T6 |
| T5 | `bus.Bus` | T6, T7 | T5 before T6 |
| T6 | `router.Service` write API | T7, T10 | T6 before T7 |
| T7 | `httpapi.New()` mounted handler | T8, T9, T10 | T7 before T8, T9 |
| T8 | `cmd/router` binary | T10 | T8 before T10 |
| T11 | `monitor.Registry`, `/healthz`, `/metrics` | none | T11 is last: it EDITS `cmd/router/{main,wire}.go`, which T8 creates and T10 also edits, so it is sequenced after both rather than sharing a wave with either |

## Notes

- **Pre-flight, once, before wave 2:** `go get` every dependency the fan-out needs, so no
  later task edits `go.mod`. `go.mod` is a shared aggregate and concurrent writers to it are
  the two-writer bug (`cqrs` §2c.3). T1 owns it; no other task may run `go get` or
  `go mod tidy`.
- Every acceptance fence is **scoped to the packages its task touches**, and chains the new
  unit ahead of the regression suites so the new unit can carry the verdict alone.
- Exit codes are read directly, never through a pipe. Every fence sets `-o pipefail` and
  greps a `tee`'d file, because a pipeline's status is the last command's, not the suite's.
- T10's dashboard tests are Go tests over rendered `templ` output; browser verification of
  the live datastar round trip is a human sign-off recorded on T10.
- **T9 is `internal/runner`, not `internal/ocr`.** The operator generalised the worker on
  2026-09-15: it forks whatever `--cmd` names for whatever `--label` it serves, so OCR is
  the default instance of the pattern rather than the pattern itself.
- **Labels, params and pipelines cut across T1, T2, T5, T6, T7 and T9.** They were added
  mid-authoring and every one of those task files was amended for them; if a task file still
  reads as if `ocr` were the only service, it is stale and the ADR wins.
