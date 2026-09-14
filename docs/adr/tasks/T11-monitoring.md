# Task ADR-0001-T11: Expose liveness on the public listener and Prometheus metrics on a private one

**Depends-on:** T8, T10
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `monitor.Registry`, `monitor.HealthHandler`, `monitor.MetricsHandler`, `monitor.Serve(addr)`
**Consumes:** `httpapi.New()` route table (T7), `store.Repo` read methods (T2), `results.Store.Len()` (T4), `bus.Bus.Subscribers()` (T5)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the real dependency probe`, `the label allow-list`, `the separate listener bind`

## Goal

Make the router's health machine-readable: a narrow unauthenticated `/healthz` that actually
probes its dependencies, and a `/metrics` endpoint on a loopback-bound listener carrying the
numbers that answer "is this sick", above all whether any worker is serving a given label.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/monitor/registry.go` | add | counters and gauges, and the allow-listed label set |
| `internal/monitor/health.go` | add | `/healthz` — the real dependency probes |
| `internal/monitor/metrics.go` | add | Prometheus text-format rendering |
| `internal/monitor/server.go` | add | the second `http.Server` on `--metrics-addr` |
| `internal/monitor/*_test.go` | add | the failing tests |
| `internal/httpapi/api.go` | edit | **mount `GET /healthz` on the main router, unauthenticated** |
| `internal/router/service.go` | edit | increment counters at each state transition |
| `cmd/router/main.go` | edit | **construct the registry and start the metrics listener** |
| `cmd/router/wire.go` | edit | pass the registry into `httpapi.New` deps |
| `README.md` | edit | the two endpoints, the loopback default, and why it is loopback |

Three of these are the *selecting* lines: `api.go` mounts `/healthz`, `main.go` starts the
second listener, and `service.go` is where a counter that nobody increments would be a
metric that is always zero. All three carry tests below that go red when the line is removed.

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
| `TestHealthzOKWhenDependenciesWork` | `internal/monitor/health_test.go` | 200 with `ok:true` and the version against a working store and blob dir | — | S3 |
| `TestHealthzFailsWhenDatabaseIsUnreachable` | `internal/monitor/health_test.go` | with the store closed, 503 naming `db` — **the probe actually probes**, which is what separates this from a vacuous gate | — | S3 |
| `TestHealthzFailsWhenBlobDirUnwritable` | `internal/monitor/health_test.go` | with the blob dir made read-only, 503 naming `blobs` | — | S3 |
| `TestHealthzIgnoresWorkerAvailability` | `internal/monitor/health_test.go` | with **zero** workers connected for every label, `/healthz` is still 200 — the assertion that fails if someone later "improves" the check into a restart loop | — | S4 |
| `TestHealthzIsUnauthenticated` | `internal/httpapi/api_test.go` | `/healthz` answers with no `Authorization` header, while every other route still 401s — asserted together so making it public cannot quietly make a sibling public | — | S3 |
| `TestHealthzIsMountedOnMainListener` | `cmd/router/main_test.go` | `/healthz` resolves through the binary's own `buildHandler` — red if the mount line is deleted | — | S3 |
| `TestWorkersLiveReflectsSubscribers` | `internal/monitor/metrics_test.go` | the gauge is 0 for a label with no subscriber, 2 with two worker streams open, and back to 0 after both disconnect — **the silent failure this whole task exists to surface** | — | S5 |
| `TestQueueOldestAgeReflectsStall` | `internal/monitor/metrics_test.go` | with one job queued 300s ago and depth 1, the age gauge is ~300 — the stall signal that depth alone misses | — | S5 |
| `TestQueueDepthPerLabel` | `internal/monitor/metrics_test.go` | depth is reported per label and does not sum across labels | — | S5 |
| `TestCountersIncrementOnTransitions` | `internal/monitor/metrics_test.go` | driving a job to `delivered`, one to `expired` and one to `dead` moves exactly the matching `ocrr_jobs_total{state}` series | — | S8 |
| `TestCreditsDebitedCounterMatchesLedger` | `internal/monitor/metrics_test.go` | after N deliveries the counter equals the sum of the ledger rows — a counter that disagrees with the books is worse than none | — | S8 |
| `TestReaperActionsCounted` | `internal/monitor/metrics_test.go` | a `Reap` pass that requeues, expires and sweeps moves all three `action` series | — | S8 |
| `TestMetricLabelAllowList` | `internal/monitor/registry_test.go` | registering a metric with a label name outside the allow-list panics; the permitted three are accepted — the cardinality guard, enforced by the compiler's data rather than by a comment | — | S6 |
| `TestExportedLabelNamesAreOnlyAllowListed` | `internal/monitor/registry_test.go` | scraping the **rendered** output and parsing every label name back out yields nothing outside the allow-list — catches a hand-written exposition line that bypassed the registry | — | S5, S6 |
| `TestPrometheusTextFormatParses` | `internal/monitor/metrics_test.go` | the rendered body round-trips through a strict text-format parser, with `# TYPE` lines present and no duplicate series | — | S5 |
| `TestMetricsServesOnSeparateListener` | `internal/monitor/server_test.go` | `/metrics` answers on the metrics address and **404s on the main listener** — both halves, because serving it in both places is the leak this design avoids | — | S7 |
| `TestMetricsDefaultsToLoopback` | `cmd/router/main_test.go` | with no `--metrics-addr`, the metrics listener binds `127.0.0.1` and not `0.0.0.0` — the default is the protection, so the default is what is asserted | — | S7 |
| `TestMetricsListenerStopsWithServer` | `internal/monitor/server_test.go` | cancelling the context closes the metrics listener, leaving no goroutine or held port | — | S7 |
| `TestGaugesDoNotTouchTheWritePath` | `internal/monitor/metrics_test.go` | a scrape issues only reads — pointed at the `query_only` handle it succeeds, which it could not if it wrote | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the nineteen tests above |
| 2 — something selects it | `api.go` mounts `/healthz` (`TestHealthzIsMountedOnMainListener`), `main.go` starts the metrics listener (`TestMetricsDefaultsToLoopback`), and `service.go` increments the counters (`TestCountersIncrementOnTransitions`). Each goes red when its one line is deleted — chosen because a metric nobody increments is always zero, and always-zero reads exactly like healthy. |
| 3 — the caller can discover it | `--help` lists `--metrics-addr`; the README documents both endpoints, the loopback default and why it is loopback |
| 4 — it is used | `ocrr_workers_live` is the intended alert signal. Nothing scrapes it inside this repository — recorded honestly rather than claimed; whether an alert exists is the operator's deployment, not this task's. |

## Mutation Log

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
