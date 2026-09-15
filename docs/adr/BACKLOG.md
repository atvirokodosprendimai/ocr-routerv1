# ADR Backlog

Deferred work, each entry naming the record that deferred it. `adr-debt docs/adr` sweeps the ADR
corpus for `(deferred: …)` dispositions and reports them; this file is where they land, so an entry
reported by `adr-debt` and missing here is a pointer to nothing.

## Taken up

- **Per-token rate limiting beyond the per-customer buffer limit.**
  Deferred by ADR-0001; **taken up by ADR-0002**
  (`docs/adr/0002/0002-rate-limiting-and-structured-logging.md`, §Decision 1).
- **Structured request and transition logging (`log/slog` JSON).**
  Deferred by ADR-0001 task T11; **taken up by ADR-0002** (§Decision 2).

## Open

- **OpenTelemetry / OTLP export for the router.**
  Deferred by ADR-0001 (§Alternatives), by task T11 (§Out of Scope), and **re-deferred by ADR-0002**
  (§Decision 3) on 2026-09-15.
  T11 ships Prometheus text format on a loopback listener. OTEL was rejected **for now** because it
  adds a collector as a runtime dependency for a single-process system; the sibling project
  `wgmesh`/chimney chose OTEL in a context where Coroot was already running, which is that
  project's call and not precedent here. ADR-0002 reviewed this and changed nothing: no new
  evidence arrived, and implementing it would have reversed an accepted decision on no grounds. The
  metric names T11 defines remain exportable over OTLP without re-measuring anything, so this stays
  a transport decision.

- **Logs delivered over SSE to the admin dashboard.**
  Deferred by ADR-0002 (§Alternatives, §Out of Scope).
  The system already has an SSE bus and a dashboard to render into, which is why this was
  considered seriously. It needs a decision about which principals may read which logs — a customer
  must never see another's params, and the transition log is full of them — and that authorization
  design is larger than the logging it would serve.

- **Audit logging of administrative actions as a separate tamper-evident stream.**
  Deferred by ADR-0002 task T2 (§Out of Scope).
  ADR-0002's request log records that an admin called an endpoint. It is not tamper-evident and it
  is not separable from operational noise, which is what an audit trail has to be.

- **Tracing spans across the router → worker → router round trip.**
  Deferred by ADR-0002 (§Out of Scope) and task T4.
  The transition log makes a job's path readable in one process. It does not correlate with what
  the worker did, because the worker emits nothing structured.

- **Per-route rate limits, as opposed to per-role.**
  Deferred by ADR-0002 task T3 (§Out of Scope).
  One limit covers every route a role can reach. `POST /upload` and `POST /claim` have very
  different costs, and a limit tuned for one is loose or tight for the other.

- **Structured logging inside `cmd/worker`.**
  Deferred by ADR-0002 task T4 (§Out of Scope).
  ADR-0002 covers the router. The worker still writes nothing structured, so the subprocess's
  stderr — the single most useful artefact when a job fails — is not captured anywhere.

- **Charts and historical analytics on the admin dashboard.**
  Deferred by ADR-0001 task T10 (§Out of Scope).
  T10 ships current state. Nothing retains a time series, so "was it like this yesterday" has no
  answer; T11's metrics are the intended source if this is ever built.

- **Encryption at rest for source files and results.**
  Deferred by ADR-0001 task T4 (§Out of Scope).
  Source blobs sit unencrypted on disk and results sit unencrypted in memory. Needs a decision on
  key custody before anything else.

- **Packaging: systemd units, containers, release artefacts.**
  Deferred by ADR-0001 task T8 (§Out of Scope).
  The binaries build and run; nothing ships them.

- **Sandboxing the worker subprocess with containers or seccomp.**
  Deferred by ADR-0001 task T9 (§Out of Scope).
  The worker forks an operator-configured command with customer-supplied arguments. ADR-0001
  constrains the argv construction — no shell, validated keys, values as their own argv elements —
  and does not confine the process that results.

- **Windows support for the worker.**
  Deferred by ADR-0001 task T9 (§Out of Scope).
  Process-group handling and the tmpdir lifecycle are POSIX-shaped.
