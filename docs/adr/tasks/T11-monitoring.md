# Task ADR-0001-T11: Expose liveness on the public listener and Prometheus metrics on a private one

**Depends-on:** T8, T10
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `monitor.Registry`, `monitor.HealthHandler`, `monitor.MetricsHandler`, `monitor.Serve(addr)`
**Consumes:** `httpapi.New()` route table (T7), `store.Repo` read methods (T2), `results.Store.Len()` (T4), `bus.Bus.Subscribers()` (T5)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the real dependency probe`, `the label allow-list`, `the separate listener bind`, `the counter attached to the writer`, `the unauthenticated mount`, `the loopback default`

## Goal

Make the router's health machine-readable: a narrow unauthenticated `/healthz` that actually
probes its dependencies, and a `/metrics` endpoint on a loopback-bound listener carrying the
numbers that answer "is this sick", above all whether any worker is serving a given label.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/monitor/registry.go` | add | counters, the metric names, and the allow-listed label set |
| `internal/monitor/health.go` | add | `/healthz` — the real dependency probes |
| `internal/monitor/monitor.go` | add | scrape-time gauges, Prometheus text-format rendering, and `Serve(ctx, addr)` |
| `internal/monitor/monitor_test.go` | add | the failing tests for the package |
| `internal/router/metrics.go` | add | the `Counter` interface **at the consumer**, its nop default, and `SetCounter` |
| `internal/router/service.go` | edit | increment counters at each state transition |
| `internal/router/reaper.go` | edit | increment `ocrr_reaper_actions_total{action}` per sweep |
| `internal/router/metrics_test.go` | add | the transition, ledger-agreement and reaper counter tests |
| `cmd/router/wire.go` | edit | **construct the registry, attach it to the writer, and mount `GET /healthz` unauthenticated** |
| `cmd/router/main.go` | edit | **`--metrics-addr` and starting the second listener** |
| `cmd/router/monitoring_test.go` | add | the composition-root tests |
| `README.md` | edit | the two endpoints, the loopback default, and why it is loopback |

⚠ **Deviation from the plan, recorded rather than quietly absorbed:** `/healthz` is mounted in
`cmd/router/wire.go`, not in `internal/httpapi/api.go` as this task first said. `httpapi.New`
returns a router whose every route is inside its authenticated group; mounting an exception
inside it would have meant carving a hole in the authenticator itself, where the next route
added inherits the hole. The composition root already owns the chi mux, so the unauthenticated
route lives beside the authenticated subtree instead of inside it. `httpapi` is therefore
unchanged by this task, and the proof moved with the code — to `cmd/router/monitoring_test.go`.

Three of these are the *selecting* lines: `wire.go` mounts `/healthz` and calls
`rt.SetCounter(reg)`, `main.go` starts the second listener, and `service.go` is where a counter
that nobody increments would be a metric that is always zero. All three carry tests below that
go red when the line is removed.

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write
   `health_test.go` asserting `/healthz` returns 503 when the database is unreachable,
   before any implementation (TDD red). [proof: acceptance]
2. [S2] `monitor.Registry` holds counters as `atomic.Int64` and computes gauges on demand
   from the read handle. **Gauges are computed at scrape time, not maintained on every
   write** — the scrape pays the cost, not the upload path.
3. [S3] `/healthz` performs **real probes**: `SELECT 1` through the read handle, and a
   create-write-remove of a temp file in the blob directory. `200 {"ok":true,"version":…}`,
   or `503 {"ok":false,"failed":["db"|"blobs"]}`. It is mounted on the **main** listener and
   is the **only** unauthenticated route in the API.
4. [S4] ⚠ `/healthz` does **not** consider worker availability. Zero workers for a label is
   degraded, not dead; reporting it as unhealthy would make an orchestrator restart the one
   component still working. Worker availability is `ocrr_workers_live{label}` and an alert.
5. [S5] Export, in Prometheus text format: `ocrr_workers_live{label}` (gauge, from
   `bus.Subscribers("workers:"+label)`), `ocrr_queue_oldest_age_seconds{label}` (gauge),
   `ocrr_queue_depth{label}` (gauge), `ocrr_jobs_total{state}` (counter),
   `ocrr_stage_advances_total`, `ocrr_reaper_actions_total{action}`,
   `ocrr_results_in_memory` (gauge), `ocrr_credits_debited_total` (counter).
6. [S6] **The label allow-list.** `Registry` accepts only the metric label *names* in a
   compile-time set (`label`, `state`, `action`). Anything else is a programming error and
   panics in tests. This is what stops a later change adding `user_id` and taking down the
   scraper — the constraint has to live in the code, because a prose rule about cardinality
   is invisible to the person adding one field.
7. [S7] `monitor.Serve(addr)` runs a **second** `http.Server` on `--metrics-addr`, default
   `127.0.0.1:9090`, serving `/metrics` only. It shuts down with the main server's context.
8. [S8] `router.Service` increments `ocrr_jobs_total{state}` on every terminal transition,
   `ocrr_stage_advances_total` on each advance, `ocrr_reaper_actions_total{action}` in
   `Reap`, and `ocrr_credits_debited_total` in the delivery transaction. The registry is
   injected, so a service constructed without one is a compile error rather than a silent
   no-op.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/monitor/... -count=1 -race 2>&1 | tee /tmp/adr1-t11.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t11.out \
  && go test ./internal/httpapi/... ./internal/router/... ./cmd/router/... -count=1 2>&1 | tee /tmp/adr1-t11r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t11r.out
```

The new package runs first and alone so it can carry the verdict; the three packages this
task edits run second as regression. Red at authoring: `internal/monitor` does not exist.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestHealthzOKWhenDependenciesWork` | `internal/monitor/monitor_test.go` | 200 with `ok:true` and the version against a working store and blob dir | — | S3 |
| `TestHealthzFailsWhenDatabaseIsUnreachable` | `internal/monitor/monitor_test.go` | with the database probe failing, 503 naming `db` — **the probe actually probes**, which is what separates this from a vacuous gate | — | S3 |
| `TestHealthzFailsWhenBlobDirUnwritable` | `internal/monitor/monitor_test.go` | with the blob dir made read-only, 503 naming `blobs`; skipped with a stated reason under `os.Geteuid() == 0`, where mode bits cannot make it fail | — | S3 |
| `TestHealthzIgnoresWorkerAvailability` | `internal/monitor/monitor_test.go` | with **zero** workers for every label and 500 jobs queued, `/healthz` is still 200 — the assertion that fails if someone later "improves" the check into a restart loop | — | S4 |
| `TestHealthzIsUnauthenticatedInTheBinary` | `cmd/router/monitoring_test.go` | `/healthz` resolves through the binary's own graph with no `Authorization` header — red if the mount line is deleted from `wire.go` | — | S3 |
| `TestHealthzIsTheOnlyUnauthenticatedRoute` | `cmd/router/monitoring_test.go` | every other route, probed with its own method so chi's 405 cannot answer for it, still 401s — asserted alongside the exception so making `/healthz` public cannot quietly make a sibling public | — | S3 |
| `TestHealthzReportsTheBuildVersion` | `cmd/router/monitoring_test.go` | the configured version reaches the response body — the field an operator reads mid-rollout | — | S3 |
| `TestHealthzStaysHealthyWithNoWorkers` | `cmd/router/monitoring_test.go` | the binary-level statement of S4: a queued job and nobody serving it is still 200 | — | S4 |
| `TestWorkersLiveReflectsSubscribers` | `internal/monitor/monitor_test.go` | the gauge is 2 for a served label and an explicit **0** for a known label with nobody serving it — an absent series cannot be alerted on, which is **the silent failure this whole task exists to surface** | — | S5 |
| `TestQueueOldestAgeReflectsStall` | `internal/monitor/monitor_test.go` | with one job queued 300s ago and depth 1, the age gauge is 300 — the stall signal that depth alone misses | — | S5 |
| `TestQueueDepthIsPerLabel` | `internal/monitor/monitor_test.go` | depth is reported per label and does not sum across labels | — | S5 |
| `TestCountersAppearInTheScrape` | `internal/monitor/monitor_test.go` | counters registered through `Registry` are rendered with their labels and values | — | S5 |
| `TestCountersIncrementOnTransitions` | `internal/router/metrics_test.go` | driving a job through claim, complete and deliver moves `ocrr_jobs_total{state="delivered"}` | — | S8 |
| `TestCreditsDebitedCounterMatchesLedger` | `internal/router/metrics_test.go` | after three deliveries of 3, 1 and 5 pages the counter equals the sum of the ledger's debits — a counter that disagrees with the books is worse than none | — | S8 |
| `TestReaperActionsCounted` | `internal/router/metrics_test.go` | a `Reap` pass that expires a lease moves an `ocrr_reaper_actions_total{action}` series, with the fixture asserting the lease really expired first | — | S8 |
| `TestServiceWithoutACounterStillWorks` | `internal/router/metrics_test.go` | a `Service` constructed with no counter runs the full transition path — the nop default, written down rather than assumed | — | S8 |
| `TestSetCounterNilIsSafe` | `internal/router/metrics_test.go` | `SetCounter(nil)` yields the nop rather than a panic on the first transition, which would crash in production and nowhere else | — | S8 |
| `TestCountersMoveThroughTheBinarysGraph` | `cmd/router/monitoring_test.go` | a real job driven through the binary over HTTP moves `ocrr_jobs_total{state="delivered"}` and charges exactly 2 to `ocrr_credits_debited_total` — red if `rt.SetCounter(reg)` is deleted from the composition root, which every `internal/monitor` test survives | — | S8 |
| `TestMetricLabelAllowListPanics` | `internal/monitor/monitor_test.go` | registering `user_id`, `job_id`, `email` or `customer` panics, and the permitted three are accepted — the guard is not simply refusing everything, which would pass every negative test | — | S6 |
| `TestExportedLabelNamesAreOnlyAllowListed` | `internal/monitor/monitor_test.go` | parses every label name back out of the **rendered** scrape and finds nothing outside the allow-list, failing if no labelled series was found at all — catches a hand-written exposition line that bypassed the registry | — | S5, S6 |
| `TestPrometheusTextFormatHasTypeLines` | `internal/monitor/monitor_test.go` | `# TYPE` lines present and no duplicate series, which a scraper rejects | — | S5 |
| `TestLabelValuesAreEscaped` | `internal/monitor/monitor_test.go` | a label value containing `"` and `\` is escaped, so one quote cannot corrupt the whole scrape | — | S5 |
| `TestMetricsServesOnItsOwnListener` | `internal/monitor/monitor_test.go` | a **real** bind answers `/metrics` and 404s `/healthz`, `/upload`, `/files/x` and `/` — the metrics listener serves one path and is not a second front door | — | S7 |
| `TestMetricsIsNotOnThePublicListener` | `cmd/router/monitoring_test.go` | the internet-facing handler serves no `ocrr_` metrics at `/metrics` or `/admin/metrics` — the half of the exposure decision that protects the data, invisible to `internal/monitor` | — | S7 |
| `TestMetricsDefaultsToLoopback` | `cmd/router/monitoring_test.go` | the `--metrics-addr` default read off the real command is bound, and the **listener's actual address** is loopback — the flag's default string proves nothing about the socket | — | S7 |
| `TestMetricsListenerBindsWhereItIsTold` | `internal/monitor/monitor_test.go` | `Serve` returns the RESOLVED address, not the requested one — without which no test using it can assert anything about the real socket | — | S7 |
| `TestMetricsListenerStopsWithContext` | `internal/monitor/monitor_test.go` | the listener answers before cancel and refuses after, polled to a deadline rather than slept at | — | S7 |
| `TestGaugesDoNotTouchTheWritePath` | `cmd/router/monitoring_test.go` | a scrape against the binary's `query_only` read handle succeeds and reports the queued job — it could not if it wrote, and the depth assertion stops it passing against tables it never read | — | S2 |
| `TestScrapeIsRepeatable` | `internal/monitor/monitor_test.go` | two scrapes with no state change are identical — a gauge that drifts on read is maintaining state it should be deriving | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the twenty-nine tests above |
| 2 — something selects it | `wire.go` mounts `/healthz` (`TestHealthzIsUnauthenticatedInTheBinary`) and calls `rt.SetCounter(reg)` (`TestCountersMoveThroughTheBinarysGraph`), `main.go` starts the metrics listener (`TestMetricsDefaultsToLoopback`), and `service.go` increments the counters (`TestCountersIncrementOnTransitions`). Each goes red when its one line is deleted — chosen because a metric nobody increments is always zero, and always-zero reads exactly like healthy. |
| 3 — the caller can discover it | `--help` lists `--metrics-addr`; the README documents both endpoints, the loopback default and why it is loopback |
| 4 — it is used | `ocrr_workers_live` is the intended alert signal. Nothing scrapes it inside this repository — recorded honestly rather than claimed; whether an alert exists is the operator's deployment, not this task's. |

## Mutation Log

- 2026-09-15 · 06afdbb* · mutant killed · exit 1 · `internal/monitor/health.go` · a dead database is not reported, so /healthz is a vacuous gate that stays green through a total outage · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · covers:the real dependency probe
- 2026-09-15 · 06afdbb* · mutant killed · exit 1 · `internal/monitor/registry.go` · the cardinality guard accepts any label name, so a later user_id label reaches the scraper · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · covers:the label allow-list
- 2026-09-15 · 06afdbb* · mutant killed · exit 1 · `cmd/router/wire.go` · the registry is never attached to the single writer, so every counter stays zero and reads exactly like a healthy quiet system · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · covers:the counter attached to the writer
- 2026-09-15 · 06afdbb* · mutant killed · exit 1 · `cmd/router/wire.go` · the liveness route is never mounted, so every probe gets 404 and the load balancer takes a healthy process out of rotation · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · covers:the unauthenticated mount
- 2026-09-15 · 06afdbb* · mutant killed · exit 1 · `cmd/router/main.go` · the metrics listener defaults to every interface, publishing per-customer queue depth and throughput to the internet for any operator who never read the flag list · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · covers:the loopback default
- 2026-09-15 · 06afdbb* · mutant killed · exit 1 · `internal/monitor/monitor.go` · the metrics listener answers every path, so it is a second unauthenticated front door rather than one endpoint · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · covers:the separate listener bind

## Invariants

- `/healthz` is the only unauthenticated route in the API.
- `/healthz` probes real dependencies and can return 503.
- Worker availability is never an input to liveness.
- `/metrics` is served **only** on the metrics listener, never on the main one.
- No metric label name outside `{label, state, action}`.
- A scrape performs no writes.

## Risks

- **`TestHealthzFailsWhenBlobDirUnwritable` will not fail when the tests run as root**, which
  is the case in many CI containers: root ignores the mode bits. Skip it with a stated reason
  when `os.Geteuid() == 0` rather than letting it pass vacuously — a green test that cannot
  fail is the exact defect this task is guarding against elsewhere.
- **`TestMetricsDefaultsToLoopback` is the one that protects the leak**, and the tempting
  version — asserting the flag's default *string* — proves nothing about what the socket
  bound to. Assert the listener's actual address.
- **The label allow-list is enforced at registration, not at render.** A hand-written
  exposition line would bypass it, which is why `TestExportedLabelNamesAreOnlyAllowListed`
  parses the rendered output rather than inspecting the registry.
- **`ocrr_queue_oldest_age_seconds` is a `MIN(queued_at)` per label** and will scan without
  the `jobs(state, label, queued_at)` index from T2. With the index it is an index seek.

## Stop Condition

Stop and ask if the operator wants alerting rules or a scrape config committed here — this
task exposes the numbers and deliberately ships no opinion about thresholds, because a
threshold is valid for a deployment and this repository does not know the deployment.

## Out of Scope

- OpenTelemetry / OTLP export (deferred: docs/adr/BACKLOG.md).
- Alert rules, scrape configs, dashboards for Grafana (permanent: boundary: a threshold is valid for a deployment, and this repository does not know the deployment).
- Per-customer metrics (permanent: boundary: unbounded label cardinality; the admin dashboard answers the per-customer question).
- Log aggregation and structured request logging (deferred: docs/adr/BACKLOG.md).

## Verification Log
- 2026-09-15 · 06afdbb* · exit 0 · `set -o pipefail …` · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · ms:7031
- 2026-09-15 · 06afdbb* · exit 0 · `set -o pipefail …` · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · ms:5715
- 2026-09-15 · 06afdbb* · exit 0 · `set -o pipefail …` · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · ms:4784
- 2026-09-15 · 06afdbb* · exit 0 · `set -o pipefail …` · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · ms:6521
- 2026-09-15 · 06afdbb* · exit 0 · `set -o pipefail …` · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · ms:5211
- 2026-09-15 · 06afdbb* · exit 0 · `set -o pipefail …` · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · ms:4934
- 2026-09-15 · 06afdbb* · exit 0 · `set -o pipefail …` · acceptance-sha256:651916e58d1f57d804f5631e667aed81fd925fd4e1a3b08453ac27d14663916c · ms:4418
