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
