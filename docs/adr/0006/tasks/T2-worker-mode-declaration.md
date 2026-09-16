# Task ADR-0006-T2: Make a worker declare its mode, and refuse one that disagrees with the admin record

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `Service.CheckWorkerMode(ctx context.Context, label string, raw bool) error`, `core.ErrModeMismatch`, `?raw=` on `/sse` and `/claim`
**Consumes:** `Repo.ServiceMode(ctx, label)` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the comparison against the admin record`, `the refusal reaching the worker as 409`

## Goal

A worker declares `raw=0|1` when it declares its label, and the router refuses it when that
disagrees with `service_rates.raw`.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/router/service.go` | edit | `CheckWorkerMode` — refuses a declaration that disagrees with `service_rates`, and records nothing |
| `internal/core/errors.go` | edit | `ErrModeMismatch` sentinel |
| `internal/httpapi/sse.go` | edit | Reads `?raw=` on a worker's subscribe — the primary declaration point, because the bus topic is where the label registry is derived from |
| `internal/httpapi/claim.go` | edit | Reads `?raw=` on claim; a worker that claims without ever subscribing still declares |
| `internal/httpapi/errors.go` | edit | `ErrModeMismatch` → 409, so `TestErrorStatusMapping` covers it rather than defaulting to 500 |
| `internal/httpapi/raw_test.go` | add | The failing tests below |
| `internal/httpapi/rawparam.go` | add | `rawParam` — the one place `?raw=` is parsed, so "absent means units" is stated once rather than in each handler |

## Ordered Steps

1. [S1] Write the failing tests: `TestRawModeMismatchIsRefused` (both directions — a raw worker on
   a units label and a units worker on a raw label) and `TestMatchingModeIsAccepted`. Confirm red. [proof: acceptance]
2. [S2] ⚠ **AMENDED DURING EXECUTION — `labelSeen` is NOT restructured.** The task planned a map to
   a struct carrying `seen` and `raw`. That was unnecessary and slightly wrong: the admin record is
   the single authority on a service's mode, so `CheckWorkerMode` re-reads `service_rates` on every
   declaration and stores nothing. A copy of the declaration in the registry would be a second value
   that can disagree with the one that decides the bill, and the invariant below is that only one of
   them may. `labelSeen`, `ObserveLabels` and the grace window are untouched. [proof: human: this step REMOVES planned work, so no test can witness it; the reviewer checks that `git diff internal/router/service.go` adds `CheckWorkerMode` and changes neither the `labelSeen` declaration nor `ObserveLabels`]
3. [S3] Add `Service.CheckWorkerMode(ctx, label, raw) error`: read `Repo.ServiceMode`, compare,
   return `ErrModeMismatch` on disagreement. ⚠ **AMENDED: it records nothing on agreement.** The
   draft also stamped `labelSeen`; a mutation graded that line SURVIVED — `Claim` already stamps and
   `ObserveLabels` derives the registry from live bus topics, so it was dead code. It was deleted
   rather than given a test, and the function renamed from `NoteWorker` to say what it does.
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
go test ./internal/httpapi/... -run 'TestRawModeMismatchIsRefused|TestMatchingModeIsAccepted|TestUndeclaredModeIsUnits|TestUnconfiguredLabelIsUnits' -count=1 -v 2>&1 | tee /tmp/acc-0006-T2.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T2.out \
  && go test ./internal/httpapi/... ./internal/router/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestRawModeMismatchIsRefused` | `internal/httpapi/raw_test.go` | A worker declaring `raw=1` on a label whose `service_rates.raw` is 0 is refused 409 — and the reverse. Both directions, because one-way is how a leaked token gets a free downgrade | — | S3, S4, S5 |
| `TestMatchingModeIsAccepted` | `internal/httpapi/raw_test.go` | A worker whose declaration agrees is admitted and its label becomes available | — | S3, S4 |
| `TestUndeclaredModeIsUnits` | `internal/httpapi/raw_test.go` | A worker that sends no `raw` param at all is treated as units — an un-upgraded worker degrades to a refusal on a raw label, never to silent acceptance | — | S3 |
| `TestUnconfiguredLabelIsUnits` | `internal/httpapi/raw_test.go` | A label with no `service_rates` row at all is units: an undeclared worker is admitted and a raw worker is refused — raw is admin-owned, so a service nobody configured is not one | — | S3 |
<!-- TestErrorStatusMapping (internal/httpapi/api_test.go) is deliberately NOT a row here. It is a
pre-existing table-driven test over every sentinel in core, it runs in this fence's REGRESSION
segment, and it is not a test this task adds — listing it would claim ownership of coverage that
already existed. What T2 owns is that ErrModeMismatch reaches the worker as 409, and that is proved
by TestRawModeMismatchIsRefused plus the killed mutant on internal/httpapi/errors.go. -->

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestRawModeMismatchIsRefused` |
| 2 — something selects it | The SSE subscribe and claim handlers call `CheckWorkerMode`; deleting either call makes `TestRawModeMismatchIsRefused` red on that path — the mutation entry proves the comparison itself, and the two-path coverage is why the test exercises `/claim` AND `/sse` |
| 3 — the caller can discover it | `?raw=` is documented in the worker README and is the wire contract; `TestUndeclaredModeIsUnits` pins what omitting it means |
| 4 — it is used | Nothing measures this yet. A refusal counter is deliberately not added here — T6 owns metrics |

## Mutation Log

- 2026-09-16 · 7ccbfa8* · mutant killed · exit 1 · `internal/router/service.go` · the mode comparison is removed, so a worker may declare any mode for any service — the pricing escalation ADR-0001 drew the boundary against · acceptance-sha256:e948846982950d1162110f50f00866c8be7e431c2616c17a3cfe7d09e6dc28fe · covers:the comparison against the admin record
- 2026-09-16 · 7ccbfa8* · mutant killed · exit 1 · `internal/httpapi/errors.go` · ErrModeMismatch falls through to 500, so the refusal reads as a router fault rather than a configuration disagreement the operator can fix · acceptance-sha256:e948846982950d1162110f50f00866c8be7e431c2616c17a3cfe7d09e6dc28fe · covers:the refusal reaching the worker as 409
- 2026-09-16 · 7ccbfa8* · mutant survived · exit 0 · `internal/router/service.go` · an agreeing worker never registers its label, so the service it serves never becomes available to clients · acceptance-sha256:e948846982950d1162110f50f00866c8be7e431c2616c17a3cfe7d09e6dc28fe · covers:the registry recording the mode on agreement
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```
- 2026-09-16 · 7ccbfa8* · mutant killed · exit 1 · `internal/router/service.go` · the mode comparison is removed, so a worker may declare any mode for any service — the pricing escalation ADR-0001 drew the boundary against · acceptance-sha256:e948846982950d1162110f50f00866c8be7e431c2616c17a3cfe7d09e6dc28fe · covers:the comparison against the admin record
- 2026-09-16 · 7ccbfa8* · mutant killed · exit 1 · `internal/httpapi/errors.go` · ErrModeMismatch falls through to 500, so a configuration disagreement the operator can fix reads as a router fault · acceptance-sha256:e948846982950d1162110f50f00866c8be7e431c2616c17a3cfe7d09e6dc28fe · covers:the refusal reaching the worker as 409

## Invariants

- A worker can never *set* a label's mode, only agree with it. `CheckWorkerMode` reads `service_rates`
  and never writes it. This is the ADR's security boundary and the reason the task exists.
- An omitted `raw` param means units, never "whatever the label says". Inferring it from the record
  would make an un-upgraded worker silently correct on a raw label and reintroduce the escalation.
- `AvailableLabels` and the grace window behave exactly as before for units labels.

## Risks

- Two declaration points (SSE and claim) can disagree within one worker. Mitigated by both calling
  the same `CheckWorkerMode`, so the second refuses if it differs from the first.
- `ObserveLabels` reconstructs the registry from bus topic names and has no mode to offer. With S2
  amended this is a non-issue rather than a mitigation: the registry never held a mode, so there is
  nothing for `ObserveLabels` to resurrect wrongly. `TestLabelGraceUnchangedByModeRegistry` was
  dropped for the same reason — with `labelSeen` untouched it would have guarded nothing this task
  changed, and a test that cannot fail for the reason its name gives is worse than no test.

## Stop Condition

Stop if the SSE subscribe path turns out not to be reachable with query params in a way the worker
can set (e.g. the topic is derived before the handler sees the URL). The ADR assumes the declaration
can ride the subscribe; if it cannot, the declaration point is a design question for the owner, not
a workaround to invent here.

## Out of Scope

- The client half of the match — T3.
- The `--raw` flag on `cmd/worker` — T4. This task defines the wire contract the flag will use.

## Verification Log
- 2026-09-16 · 7ccbfa8* · exit 1 · `set -o pipefail …` · acceptance-sha256:5d46c518f7d032adbac28f6c9a5570460cfb5dee5b22bda34a01d35c1bca13f9 · ms:1394
  ```
  --- last 10 line(s) of stdout (of 15 after folding 15 raw)
  === NAME  TestRawModeMismatchIsRefused
      raw_test.go:63: SSE subscribe with a mismatched mode = 200, want 409
  --- FAIL: TestRawModeMismatchIsRefused (0.02s)
      --- FAIL: TestRawModeMismatchIsRefused/a_raw_worker_on_a_units_label (0.00s)
      --- FAIL: TestRawModeMismatchIsRefused/a_units_worker_on_a_raw_label (0.00s)
  === RUN   TestMatchingModeIsAccepted
  --- PASS: TestMatchingModeIsAccepted (0.01s)
  FAIL
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/httpapi	0.442s
  FAIL
  ```
- 2026-09-16 · 7ccbfa8* · exit 0 · `set -o pipefail …` · acceptance-sha256:e948846982950d1162110f50f00866c8be7e431c2616c17a3cfe7d09e6dc28fe · ms:5728
- 2026-09-16 · 7ccbfa8* · exit 0 · `set -o pipefail …` · acceptance-sha256:e948846982950d1162110f50f00866c8be7e431c2616c17a3cfe7d09e6dc28fe · ms:4718
- 2026-09-16 · 7ccbfa8* · exit 0 · `set -o pipefail …` · acceptance-sha256:e948846982950d1162110f50f00866c8be7e431c2616c17a3cfe7d09e6dc28fe · ms:4847
- 2026-09-16 · 7ccbfa8* · exit 0 · `set -o pipefail …` · acceptance-sha256:e948846982950d1162110f50f00866c8be7e431c2616c17a3cfe7d09e6dc28fe · ms:4166
- 2026-09-16 · 7ccbfa8* · exit 0 · `set -o pipefail …` · acceptance-sha256:e948846982950d1162110f50f00866c8be7e431c2616c17a3cfe7d09e6dc28fe · ms:4089
