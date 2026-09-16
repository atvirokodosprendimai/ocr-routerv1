# Task ADR-0006-T2: Make a worker declare its mode, and refuse one that disagrees with the admin record

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `Service.NoteWorker(label string, raw bool, now) error`, `core.ErrModeMismatch`
**Consumes:** `Repo.ServiceMode(ctx, label)` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the comparison against the admin record`, `the refusal reaching the worker as 409`, `the registry recording the mode on agreement`

## Goal

A worker declares `raw=0|1` when it declares its label, and the router refuses it when that
disagrees with `service_rates.raw`.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/router/service.go` | edit | `labelSeen` becomes a struct carrying the declared mode; `noteLabel` becomes `NoteWorker` and returns the refusal |
| `internal/core/errors.go` | edit | `ErrModeMismatch` sentinel |
| `internal/httpapi/sse.go` | edit | Reads `?raw=` on a worker's subscribe — the primary declaration point, because the bus topic is where the label registry is derived from |
| `internal/httpapi/claim.go` | edit | Reads `?raw=` on claim; a worker that claims without ever subscribing still declares |
| `internal/httpapi/errors.go` | edit | `ErrModeMismatch` → 409, so `TestErrorStatusMapping` covers it rather than defaulting to 500 |
| `internal/httpapi/raw_test.go` | add | The failing tests below |
| `internal/router/service_test.go` | edit | Registry-level tests |

## Ordered Steps

1. [S1] Write the failing tests: `TestRawModeMismatchIsRefused` (both directions — a raw worker on
   a units label and a units worker on a raw label) and `TestMatchingModeIsAccepted`. Confirm red. [proof: acceptance]
2. [S2] Replace `labelSeen map[string]time.Time` with a map to a struct carrying `seen` and `raw`.
   Keep `AvailableLabels`'s grace-window behaviour byte-identical — it is not this task's subject.
3. [S3] Add `Service.NoteWorker(label, raw, now) error`: read `Repo.ServiceMode`, compare, return
   `ErrModeMismatch` on disagreement, record on agreement.
4. [S4] Call it from the SSE subscribe path and from `Claim`, returning the error to the handler
   rather than recording and continuing.
5. [S5] Map `ErrModeMismatch` to 409 and confirm the existing table-driven `TestErrorStatusMapping`
   picks it up — that test walks every sentinel in `core`, so a new one that is not mapped fails it.
   [proof: acceptance]
6. [S6] Log a refusal with the label, the declared mode and the recorded mode. A worker that is
   refused forever must be diagnosable from the router's log alone. [proof: human: an operator reads one log line carrying both the declared mode and the recorded mode, and knows which of the two to change]

## Acceptance

```bash
set -o pipefail
go test ./internal/httpapi/... -run 'TestRawModeMismatchIsRefused|TestMatchingModeIsAccepted' -count=1 -v 2>&1 | tee /tmp/acc-0006-T2.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T2.out \
  && go test ./internal/httpapi/... ./internal/router/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestRawModeMismatchIsRefused` | `internal/httpapi/raw_test.go` | A worker declaring `raw=1` on a label whose `service_rates.raw` is 0 is refused 409 — and the reverse. Both directions, because one-way is how a leaked token gets a free downgrade | — | S3, S4, S5 |
| `TestMatchingModeIsAccepted` | `internal/httpapi/raw_test.go` | A worker whose declaration agrees is admitted and its label becomes available | — | S3, S4 |
| `TestUndeclaredModeIsUnits` | `internal/httpapi/raw_test.go` | A worker that sends no `raw` param at all is treated as units — an un-upgraded worker degrades to a refusal on a raw label, never to silent acceptance | — | S3 |
| `TestLabelGraceUnchangedByModeRegistry` | `internal/router/service_test.go` | The grace window still keeps a label valid after its worker disconnects; the struct change did not alter it | — | S2 |
| `TestErrorStatusMapping` | `internal/httpapi/api_test.go` | Every `core` sentinel maps to a status; catches an unmapped `ErrModeMismatch` | — | S5 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestRawModeMismatchIsRefused` |
| 2 — something selects it | The SSE subscribe and claim handlers call `NoteWorker`; deleting either call makes `TestRawModeMismatchIsRefused` green on that path, which is what the mutation entry must prove |
| 3 — the caller can discover it | `?raw=` is documented in the worker README and is the wire contract; `TestUndeclaredModeIsUnits` pins what omitting it means |
| 4 — it is used | Nothing measures this yet. A refusal counter is deliberately not added here — T6 owns metrics |

## Mutation Log

## Invariants

- A worker can never *set* a label's mode, only agree with it. `NoteWorker` reads `service_rates`
  and never writes it. This is the ADR's security boundary and the reason the task exists.
- An omitted `raw` param means units, never "whatever the label says". Inferring it from the record
  would make an un-upgraded worker silently correct on a raw label and reintroduce the escalation.
- `AvailableLabels` and the grace window behave exactly as before for units labels.

## Risks

- Two declaration points (SSE and claim) can disagree within one worker. Mitigated by both calling
  the same `NoteWorker`, so the second refuses if it differs from the first.
- `ObserveLabels` reconstructs the registry from bus topic names and has no mode to offer. It must
  not resurrect a label as units-mode after a raw worker declared it; T2 keeps the recorded struct
  rather than overwriting it, and `TestLabelGraceUnchangedByModeRegistry` covers the window.

## Stop Condition

Stop if the SSE subscribe path turns out not to be reachable with query params in a way the worker
can set (e.g. the topic is derived before the handler sees the URL). The ADR assumes the declaration
can ride the subscribe; if it cannot, the declaration point is a design question for the owner, not
a workaround to invent here.

## Out of Scope

- The client half of the match — T3.
- The `--raw` flag on `cmd/worker` — T4. This task defines the wire contract the flag will use.

## Verification Log
