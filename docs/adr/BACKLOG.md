# ADR Backlog

Deferred work, each entry naming the record that deferred it. `adr-debt docs/adr` sweeps
this file so entries resurface at the next `/quality-harness:adr-write`.

## Open

- **Per-token rate limiting beyond the per-customer buffer limit.**
  Deferred by ADR-0001 (`docs/adr/0001-ocr-router-architecture.md`, §Out of Scope).
  ADR-0001 meters concurrency per *customer* via `users.buffer_limit`, which stops one
  customer starving the worker pool. It does nothing about request *rate* — a customer
  inside their buffer limit can still hammer `POST /upload` with rejected requests, and a
  compromised worker token can poll `POST /claim` without bound. Needs a decision on where
  the limit lives (middleware vs. service) and what the limit is per role.

- **OpenTelemetry / OTLP export for the router.**
  Deferred by ADR-0001 (`docs/adr/0001-ocr-router-architecture.md`, §Alternatives) and by
  task T11 (`docs/adr/tasks/T11-monitoring.md`, §Out of Scope).
  T11 ships Prometheus text format on a loopback listener. OTEL was rejected **for now**
  because it adds a collector as a runtime dependency for a single-process system; the
  sibling project `wgmesh`/chimney chose OTEL in a context where Coroot was already running,
  which is that project's call and not precedent here. The metric names T11 defines are
  exportable over OTLP without changing what is measured, so this is a transport decision
  rather than a re-measurement.

- **Structured request and transition logging (`log/slog` JSON).**
  Deferred by ADR-0001 task T11 (`docs/adr/tasks/T11-monitoring.md`, §Out of Scope).
  T11 makes the system's health *countable*; it does nothing to make a single failure
  *readable*. When a job dies at attempt 3, nothing currently records which worker took it,
  what the subprocess printed to stderr, or how long each stage ran. Needs a decision on
  destination (stdout vs. a ring buffer exposed over SSE, as chimney did) and on what is
  redacted, since job params are customer-supplied and may carry URLs or credentials.
