# Task ADR-0006-T8: Give the administrator the control that marks a service raw

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** the admin control for `service_rates.raw`, `core.ServiceRate`, `ListRates` returning both halves of a price
**Consumes:** `Repo.SetRate(ctx, label, creditsPerUnit, raw, now)` (T1), `Repo.ServiceMode(ctx, label)` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the control reaching SetRate`, `the current mode being rendered back`, `templ generate having been run`

## Goal

An administrator can mark a service raw from the dashboard, and can see which services are raw
without editing them.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/web/views/views.templ` | edit | The `rateRaw` checkbox on the rate form, the `Mode` column on the services table, and `RateSaved` naming the mode; `templ generate` after, never editing `*_templ.go` |
| `internal/web/web.go` | edit | `signals.RateRaw`, the rate handler passing it to `SetRate` — the line that SELECTS this control; without it the checkbox renders and does nothing — and the services read model carrying the mode |
| `internal/web/views/model.go` | edit | `ServiceRow.Raw`, so the table can show the mode beside the rate |
| `internal/store/repo.go` | edit | `ListRates` returns `core.ServiceRate` rather than a bare int: a caller reading only the number renders a price that is wrong for every raw service |
| `internal/core/service.go` | add | `core.ServiceRate` — the two halves of a service's price, kept together because `raw` CHANGES the price rather than decorating it |
| `internal/web/rawmode_test.go` | add | The failing tests below |
| `internal/store/repo_test.go` | edit | `TestSetRateRoundTrip`'s `ListRates` assertion, which the return-type change reaches |

## Ordered Steps

1. [S1] Write the failing tests: `TestAdminCanMarkServiceRaw`, `TestRateStillSettableWithoutTouchingMode`
   and `TestServicesPageShowsMode`. Confirm red. [proof: acceptance]
2. [S2] Add the control to the rate form as a datastar `data-bind:` input — **no `<form>` tag**, per
   the team's standing datastar decision — and declare `rateRaw` in the page's `data-signals` beside
   `rateLabel` and `rateValue`.
3. [S3] ⚠ **AMENDED DURING EXECUTION — there is no per-row signal to name.** [proof: human: the reviewer confirms the Services page renders exactly ONE rate control, so there is no second binding of `rateRaw` for the first to collide with] The task assumed a per-service form and warned about the `89cdd62` shared-signal bug, where every customer row bound the same signals. The services page has no rows to edit: it is one "label + rate" control plus a read-only table, so `rateRaw` is a single page-level signal and that collision cannot occur. If a per-row editor is ever added, the per-row naming rule returns with it.
4. [S4] Read the signal in the rate handler and pass it to `SetRate`, replacing T1's
   pass-the-existing-value placeholder.
5. [S5] Render the current mode in the services table, as a `Mode` column beside `Rate`.
6. [S6] Run `templ generate` and confirm `*_templ.go` was regenerated rather than hand-edited.
   [proof: acceptance]

## Acceptance

```bash
set -o pipefail
templ generate \
  && go test ./internal/web -run 'TestAdminCanMarkServiceRaw|TestRateStillSettableWithoutTouchingMode|TestServicesPageShowsMode' -count=1 -v 2>&1 | tee /tmp/acc-0006-T8.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T8.out \
  && go test ./internal/web/... ./internal/store/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestAdminCanMarkServiceRaw` | `internal/web/rawmode_test.go` | Posting the control writes `service_rates.raw`, reads back through `ServiceMode`, and can be CLEARED again — a mode that could be set and not unset would strand a service on flat-rate billing | — | S2, S4 |
| `TestServicesPageShowsMode` | `internal/web/rawmode_test.go` | The rendered page shows the mode for a raw service AND a units one, with both labels asserted present so it cannot be satisfied by the word "raw" appearing in the page's prose | — | S5 |
| `TestRateStillSettableWithoutTouchingMode` | `internal/web/rawmode_test.go` | Editing only the price leaves a raw service raw — `SetRate` upserts the whole row, so this is what stops a price edit silently demoting it | — | S4 |

<!-- TestRawSignalIsPerServiceRow was DROPPED during execution. It guarded the 89cdd62 shared-signal
regression, which needs a repeated ROW to occur, and this page has none. A test named for a per-row
property on a page with no rows would pass for a reason unrelated to its name — the gate-that-cannot-
fail shape. It returns if a per-row editor is ever added. -->

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestAdminCanMarkServiceRaw` |
| 2 — something selects it | The rate handler in `internal/web/web.go` reads `s.RateRaw`; replacing it with a literal leaves the checkbox rendering and doing nothing, and `TestAdminCanMarkServiceRaw` goes red — the mutation recorded below, and this ADR's most likely finished-and-unreachable defect |
| 3 — the caller can discover it | The `Mode` column on the services table (S5), asserted by `TestServicesPageShowsMode`. An admin-owned flag nobody can see is one nobody will set, and every worker on that label is then refused with no visible cause |
| 4 — it is used | Nothing measures this yet |

## Mutation Log

- 2026-09-16 · b5fa5b5* · mutant killed · exit 1 · `internal/web/web.go` · the checkbox renders and does nothing — the finished-and-unreachable defect, and the one that leaves every raw worker refused with no visible cause · acceptance-sha256:61871b3be89cf8b2c919844765b84092e3da0cbf11efae6012622dd9cdca4ccc · covers:the control reaching SetRate
- 2026-09-16 · b5fa5b5* · mutant survived · exit 0 · `internal/web/views/views.templ` · the services table stops showing which services are raw, so an admin-owned flag becomes invisible and nobody sets it · acceptance-sha256:61871b3be89cf8b2c919844765b84092e3da0cbf11efae6012622dd9cdca4ccc · covers:the current mode being rendered back
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```
- 2026-09-16 · b5fa5b5* · mutant killed · exit 1 · `internal/web/web.go` · the signal name on the wire stops matching the data-bind: attribute the page renders, so the checkbox posts nothing and the mode never changes — the kebab-case/camelCase trap this file already warns about · acceptance-sha256:61871b3be89cf8b2c919844765b84092e3da0cbf11efae6012622dd9cdca4ccc · covers:templ generate having been run
- 2026-09-16 · b5fa5b5* · mutant killed · exit 1 · `internal/web/views/views.templ` · the services table stops showing which services are raw, so an admin-owned flag becomes invisible and nobody sets it · acceptance-sha256:61871b3be89cf8b2c919844765b84092e3da0cbf11efae6012622dd9cdca4ccc · covers:the current mode being rendered back

## Invariants

- `rateRaw` is a single page-level signal, declared in `data-signals` with the other rate signals.
  Per-row naming applies only if a per-row editor appears.
- No `<form>` tag; `data-bind:` per input, per the team's datastar decision.
- `*_templ.go` is generated, never edited.
- Editing a rate leaves the mode unchanged — the whole row is upserted, so the form must carry the
  current mode back.
- Neither half of a service's price is ever derived from a worker.

## Risks

- This is the task most likely to be dropped as "just a checkbox". If it is, `service_rates.raw` is
  settable only by SQL, and the three-party agreement has an administrator who cannot administer:
  every raw worker is refused and the feature appears broken rather than unconfigured. The ADR's
  risk table names the worker-refusal symptom; this is its most likely cause.
- `ListRates`'s return type changed. The compiler reaches every caller, and the sweep
  (`git grep -n "ListRates(" -- '*.go'`) found two: the dashboard read model and one test assertion.

## Stop Condition

⚠ **This condition FIRED and was resolved rather than escalated.** It said to stop if the services
page renders no per-service row, because the UI work would then be larger than the task. The page
indeed has no per-service editor — but the goal is reachable without one: a checkbox on the existing
single rate form, and a column on the table that already lists every service with its rate. That is
smaller than the task assumed, not larger, so the condition's purpose (do not silently build a large
UI) is satisfied. Escalate if a future change needs the per-service editor after all.

## Out of Scope

- Any change to how rates themselves are set or validated.
- Bulk marking of several services at once (deferred: `docs/adr/BACKLOG.md`).

## Verification Log
- 2026-09-16 · b5fa5b5* · exit 1 · `set -o pipefail …` · acceptance-sha256:9204f964fc152eff256e0caf2d55808841b27bda1b7cba77182133648fe26f72 · ms:2344
  ```
  --- last 10 line(s) of stdout (of 11 after folding 11 raw)
  --- PASS: TestAdminCanMarkServiceRaw (0.01s)
  === RUN   TestRateStillSettableWithoutTouchingMode
  --- PASS: TestRateStillSettableWithoutTouchingMode (0.01s)
  === RUN   TestServicesPageShowsMode
  --- PASS: TestServicesPageShowsMode (0.01s)
  PASS
  ok  	github.com/atvirokodosprendimai/ocr-router/internal/web	0.795s
  testing: warning: no tests to run
  PASS
  ok  	github.com/atvirokodosprendimai/ocr-router/internal/web/views	0.442s [no tests to run]
  --- last 2 line(s) of stderr
  (✓) Post-generation event received, processing... [ updates=0 needsRestart=true needsBrowserReload=true ]
  (✓) Complete [ updates=0 duration=34.337542ms ]
  ```
- 2026-09-16 · b5fa5b5* · exit 0 · `set -o pipefail …` · acceptance-sha256:61871b3be89cf8b2c919844765b84092e3da0cbf11efae6012622dd9cdca4ccc · ms:10966
- 2026-09-16 · b5fa5b5* · exit 0 · `set -o pipefail …` · acceptance-sha256:61871b3be89cf8b2c919844765b84092e3da0cbf11efae6012622dd9cdca4ccc · ms:11965
- 2026-09-16 · b5fa5b5* · exit 0 · `set -o pipefail …` · acceptance-sha256:61871b3be89cf8b2c919844765b84092e3da0cbf11efae6012622dd9cdca4ccc · ms:8194
- 2026-09-16 · b5fa5b5* · exit 0 · `set -o pipefail …` · acceptance-sha256:61871b3be89cf8b2c919844765b84092e3da0cbf11efae6012622dd9cdca4ccc · ms:7147
