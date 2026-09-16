# ADR-0006 Tasks

Implementation tasks for ADR-0006: Carry binary worker output end to end as a raw passthrough mode.
See the parent ADR for the decision.

**Source of truth:** the task files' `Depends-on` / `Produces` / `Consumes` / `Covers` headers.
This README is a derived index — when it disagrees with a task file, the task file wins and the
README must be regenerated.

⚠ **Do not start T2, T3 or T8 until the owner has accepted or overruled the boxed note in the ADR's
Decision** — the three-party mode agreement. T2 and T3 are the worker and client halves of it, and
T8 is the administrator half; a two-party design deletes T8 entirely rather than shrinking it.

## Execution Order

| Wave | Tasks | Depends-on |
|------|-------|------------|
| 1 | T1 | none |
| 2 | T2, T3, T8 | T1 |
| 3 | T4 | T2 |
| 4 | T5 | T3, T4 |
| 5 | T6, T7 | T5 |

```
            T1
      ┌─────┼─────┐
     T2    T3    T8
      │     │
     T4     │
      └──┬──┘
        T5
      ┌──┴──┐
     T6    T7
```

T5 is the join: it is the first task where a byte written by a worker can be read by a client, and
nothing downstream of it can be proved before it lands.

## Task Index

| ID | Title | Status | Covers | Acceptance |
|----|-------|--------|--------|------------|
| T1 | Store a service's raw mode where the admin owns it, and stamp it on the job | pending | — | `go test ./internal/store/... -run 'TestSetRateCarriesRawMode\|TestServiceModeDefaultsToUnits'` |
| T2 | Make a worker declare its mode, and refuse one that disagrees with the admin record | pending | — | `go test ./internal/httpapi/... -run 'TestRawModeMismatchIsRefused\|TestMatchingModeIsAccepted'` |
| T3 | Match the client's requested mode against the admin record and stamp it on the job | pending | — | `go test ./internal/httpapi/... -run 'TestClientRawOnUnitsLabelIsRefused\|...'` |
| T4 | Let a worker emit raw bytes and post them without passing through a string | pending | — | `go test ./internal/runner/... ./internal/agent/... -run 'TestRawRunnerReturnsBytesVerbatim\|...'` |
| T5 | Store a raw result as a blob and stream it to the client unchanged | pending | — | `go test ./internal/httpapi/... -run 'TestRawResultRoundTripsByteForByte\|...'` |
| T6 | Charge a raw job a flat credit on delivery and bridge a raw stage without joinUnits | pending | — | `go test ./internal/router/... -run 'TestRawJobCostsOneCreditRegardlessOfSize\|...'` |
| T7 | Let the client request raw and write the bytes it gets back unchanged | pending | — | `go test ./internal/client/... ./cmd/client/... -run 'TestRawClientWritesBytesVerbatim\|...'` |
| T8 | Give the administrator the control that marks a service raw | pending | — | `templ generate && go test ./internal/web/... -run 'TestAdminCanMarkServiceRaw\|...'` |

Status: `pending` | `partial` | `blocked` | `done`.

## Contract Coupling

| Producer | Contract | Consumer(s) | Ordering note |
|----------|----------|-------------|---------------|
| T1 | `Repo.ServiceMode` / `Repo.SetRate(…, raw, …)` | T2, T3, T8 | T1 before all three — `SetRate`'s signature break reaches `internal/web/web.go:510` |
| T1 | `jobs.raw` + `core.Job.Raw` | T3, T5, T6 | T1 before T3 |
| T2 | `Service.NoteWorker` + `core.ErrModeMismatch` | T4 | T2 before T4 — the worker's flag needs the wire contract to declare into |
| T3 | `jobs.raw` stamped at admission | T5, T6 | T3 before T5 — the stamp is what T5's shape check reads |
| T4 | worker raw result wire shape (`?job_id=`, octet-stream) | T5 | T4 before T5 |
| T5 | raw result blob + octet-stream `GET /files/{id}` | T6, T7 | T5 before both |

## Notes

- **T8 carries the UX gate.** It is the only task that touches a user-facing surface, so the
  project's UI idioms load before any markup is written: datastar `data-bind:` per input, no
  `<form>` tag, per-row signal names, `templ generate` after every `.templ` edit and never a hand
  edit of `*_templ.go`.
- **Every task's first step is the TDD red run**, and `adr-verify`'s first Verification Log entry
  for each should be that failing run.
- **T1 installs a deliberate placeholder**: `internal/web/web.go:510` passes the row's existing
  `raw` value through `SetRate` so T1 changes no admin behaviour. T8 replaces it, and
  `TestRateStillSettableWithoutTouchingMode` is what stops the placeholder becoming a silent reset.
