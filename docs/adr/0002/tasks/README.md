# ADR-0002 tasks

Derived index — the task files are the source of truth. Execute in the order below.

| Task | Goal | Status | Depends-on | Acceptance (first command) |
|------|------|--------|------------|----------------------------|
| T1 | A per-token bucket limiter with an injected clock and idle eviction | pending | none | `go test ./internal/ratelimit/... -race` |
| T2 | A slog logger whose signature makes logging a param value impossible | pending | none | `go test ./internal/logging/...` |
| T3 | Enforce the rate limit and log every request, inside the authenticated group | pending | T1, T2 | `go test ./internal/httpapi/... -race` |
| T4 | Wire both into the binary and make a job's whole path readable | pending | T1, T2, T3 | `go test ./internal/router/... -race` |

## Order

T1 and T2 are independent of each other and of everything else — either may go first, and both are
leaf packages with no in-tree consumer until T3. T3 needs both. T4 is the composition root and
needs all three.

    T1 ──┐
         ├── T3 ── T4
    T2 ──┘

## What each task is actually for

- **T1** is the limiter as a data structure. Its one interesting property is that it can be made to
  forget: a per-token map that never evicts is a memory leak keyed on something an admin can mint
  freely, and `TestEvictIdleShrinksTheMap` is the test that matters.
- **T2** is the logger, and its whole design is one negative claim — there is no argument anywhere
  in the package that accepts a param value. ADR-0001 lets a crawler take `?url=…`, and a URL
  carries credentials often enough that treating it as safe is a decision to leak them eventually.
- **T3** applies both, and the two things it gets wrong by default are invisible in a diff: the
  limiter outside the authenticator gives every caller one shared bucket, and the request logger
  inside it loses exactly the `401`s and `429`s an operator needs. Two tests exist only to pin the
  order.
- **T4** is the wiring, and every unit test in T1–T3 passes with all of its lines deleted. That is
  the point of the task and the reason its mutants bind to `wire.go`.

## Convention

Status here is derived from each task's `## Verification Log`: a task may be marked `done` only
once `adr-verify` has recorded an exit-0 entry whose digest matches the task's current Acceptance
fence, plus at least one killed mutant bound to the same digest. Do not hand-edit either log.
