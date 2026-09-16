# Task ADR-0006-T8: Give the administrator the control that marks a service raw

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** the admin control for `service_rates.raw`
**Consumes:** `Repo.SetRate(ctx, label, creditsPerUnit, raw, now)` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the control reaching SetRate`, `the per-row signal not being shared across rows`, `templ generate having been run`, `the rate write not resetting the mode`

## Goal

An administrator can mark a service raw from the dashboard, beside the rate control that already
exists.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/web/views/*.templ` | edit | The control on the service row; `templ generate` after, never editing `*_templ.go` |
| `internal/web/web.go` | edit | The rate handler at `:510` reads the new signal and passes it to `SetRate` — the line that SELECTS this control; without it the checkbox renders and does nothing |
| `internal/web/handlers_test.go` | edit | The failing tests below |

## Ordered Steps

1. [S1] Write the failing tests: `TestAdminCanMarkServiceRaw` and `TestRawSignalIsPerServiceRow`. Confirm red. [proof: acceptance]
2. [S2] Add the control to the service row as a datastar `data-bind:` input — **no `<form>` tag**,
   per the team's standing datastar decision.
3. [S3] ⚠ Give the signal a PER-ROW name (`raw-<label>` or equivalent). A shared signal name across
   rows is the exact bug fixed on 2026-09-15 in commit `89cdd62`, where every customer row bound the
   same signals and editing one row changed them all. This task is the same markup shape, so it is
   the same trap.
4. [S4] Read the signal in the rate handler and pass it through to `SetRate`, replacing T1's
   pass-the-existing-value placeholder.
5. [S5] Render the current mode on the row so an administrator can see which services are raw
   without editing them. [proof: human: an admin loads the services page and can tell raw from
   units services at a glance, without opening a control]
6. [S6] Run `templ generate` and confirm `*_templ.go` was regenerated rather than hand-edited.
   [proof: acceptance]

## Acceptance

```bash
set -o pipefail
templ generate \
  && go test ./internal/web/... -run 'TestAdminCanMarkServiceRaw|TestRawSignalIsPerServiceRow' -count=1 -v 2>&1 | tee /tmp/acc-0006-T8.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T8.out \
  && go test ./internal/web/... ./internal/store/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestAdminCanMarkServiceRaw` | `internal/web/handlers_test.go` | Posting the control writes `service_rates.raw` and the change reads back through `ServiceMode` | — | S2, S4 |
| `TestRawSignalIsPerServiceRow` | `internal/web/handlers_test.go` | With TWO services rendered, the signals differ per row — the `89cdd62` regression, which is invisible to any single-row markup test | — | S3 |
| `TestServicesPageShowsMode` | `internal/web/handlers_test.go` | The rendered page distinguishes a raw service from a units one | — | S5 |
| `TestRateStillSettableWithoutTouchingMode` | `internal/web/handlers_test.go` | Changing only the rate leaves `raw` as it was — the placeholder T1 installed must not have become a silent reset | — | S4 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestAdminCanMarkServiceRaw` |
| 2 — something selects it | The rate handler in `internal/web/web.go` reads the signal; deleting that read leaves the control rendering and doing nothing, and `TestAdminCanMarkServiceRaw` goes red — the mutation to record, and this ADR's most likely finished-and-unreachable defect |
| 3 — the caller can discover it | The control is visible on the services page (S5); an admin-owned flag nobody can see is one nobody will set, and every worker on that label is then refused |
| 4 — it is used | Nothing measures this yet |

## Mutation Log

## Invariants

- Signals are per-row, never shared across services.
- No `<form>` tag; `data-bind:` per input, per the team's datastar decision.
- `*_templ.go` is generated, never edited.
- Setting a rate without touching the mode leaves the mode unchanged.

## Risks

- This is the task most likely to be dropped as "just a checkbox". If it is, `service_rates.raw` is
  only settable by SQL, and the three-party agreement has an administrator who cannot administer —
  every raw worker is refused and the feature appears broken. The ADR's risk table names the
  worker-refusal symptom; this is its most likely cause.

## Stop Condition

Stop if the services admin page does not currently render a per-service row at all (the rate is set
somewhere else). The task assumes a row to add a control to; if there is none, the UI work is larger
than this task and is the owner's call.

## Out of Scope

- Any change to how rates themselves are set or validated.
- Bulk marking of several services at once (deferred: `docs/adr/BACKLOG.md`).

## Verification Log
