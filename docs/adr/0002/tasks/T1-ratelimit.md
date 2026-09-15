# Task ADR-0002-T1: A per-token bucket limiter with an injected clock and idle eviction

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** S (single file plus its test)
**Owner:** unassigned
**Produces:** `ratelimit.Limiter`, `ratelimit.Limit`, `Limiter.Allow()`, `Limiter.RetryAfter()`, `Limiter.EvictIdle()`, `Limiter.Len()`
**Consumes:** none — `golang.org/x/time/rate` is the only dependency
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the injected clock`, `the idle eviction`, `the zero-limit escape hatch`

## Goal

A `Limiter` that holds one token bucket per key, decides with a caller-supplied instant so every
test is deterministic, and drops entries that have been idle — so the limiter itself cannot become
the memory leak.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/ratelimit/ratelimit.go` | add | the limiter, its per-role `Limit` type, and eviction |
| `internal/ratelimit/ratelimit_test.go` | add | the failing tests |
| `go.mod` | edit | `require golang.org/x/time` |
| `go.sum` | edit | the module checksum |

Nothing selects this package yet — T3 and T4 do. Rung 2 is therefore honestly `nothing selects it
yet` in this task and is discharged by T3's and T4's mutants, rather than claimed here where no
call site exists.

## Ordered Steps

1. [S1] Write the failing tests first: a limiter that permits `burst` requests at one instant and
   refuses the next, before any implementation exists (TDD red). [proof: acceptance]
2. [S2] `Limit{RPS float64, Burst int}`, and `New(now func() time.Time, idle time.Duration)`
   returning a `*Limiter`. The clock is a field, never `time.Now` inline: every decision is made
   with an instant the caller supplies, which is what makes the tests advance time instead of
   sleeping.
3. [S3] `Allow(key string, l Limit) bool` looks up or creates the bucket for `key` and calls
   `AllowN(now, 1)`. It records `lastSeen` on every call, which is what eviction reads.
4. [S4] ⚠ **`Limit{}` zero, or `RPS <= 0`, means UNLIMITED** — `Allow` returns true without
   creating an entry. This is the operational rollback named in ADR-0002 §Rollback: an operator
   sets `--rate-client 0` and the limit is gone without a redeploy. It must not create a map entry
   either, or "disabled" would still leak.
5. [S5] `RetryAfter(key string, l Limit) time.Duration` reports how long until the next token, so
   the middleware can send a truthful `Retry-After` rather than a guess. It must not consume a
   token — asking when you may retry is not a retry.
6. [S6] `EvictIdle()` drops every entry whose `lastSeen` is older than `idle` and returns how many
   it dropped. `Len()` reports the live entry count, so a test can assert the map SHRANK rather
   than merely that eviction was called.
7. [S7] One mutex around the map. The buckets themselves are `*rate.Limiter`, which is internally
   synchronised, but creation is check-then-act and needs the lock. The race detector is in the
   Acceptance fence because this is the only shared mutable state the request path adds.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/ratelimit/... -count=1 -race 2>&1 | tee /tmp/adr2-t1.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr2-t1.out
```

Red at authoring: `internal/ratelimit` does not exist, so `go build ./...` succeeds and the test
command reports `no test files`, which the grep turns into a non-zero exit. The package runs alone
because nothing else consumes it yet — there is no regression surface to add.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestAllowsBurstThenRefuses` | `internal/ratelimit/ratelimit_test.go` | at one frozen instant, `Burst` calls are allowed and the next is refused — the core claim | — | S2, S3 |
| `TestRefillsOverTime` | `internal/ratelimit/ratelimit_test.go` | after the clock advances by `1/RPS`, exactly one more call is allowed — asserting the refill RATE, not merely that something eventually succeeds | — | S2, S3 |
| `TestClockIsInjectedNotWallClock` | `internal/ratelimit/ratelimit_test.go` | a limiter whose clock never advances refuses forever, and the same limiter with an advancing clock allows — red if any call site reaches for `time.Now()` directly | — | S2 |
| `TestKeysAreIndependent` | `internal/ratelimit/ratelimit_test.go` | exhausting one key's bucket leaves another key's untouched — the whole point of per-token rather than global | — | S3 |
| `TestZeroLimitIsUnlimited` | `internal/ratelimit/ratelimit_test.go` | `Limit{}` and `Limit{RPS: 0}` allow far past any burst — the documented rollback switch | — | S4 |
| `TestZeroLimitCreatesNoEntry` | `internal/ratelimit/ratelimit_test.go` | after 1000 allowed calls under a zero limit, `Len()` is 0 — "disabled" must not still leak, which the obvious implementation gets wrong | — | S4 |
| `TestRetryAfterIsPositiveWhenRefused` | `internal/ratelimit/ratelimit_test.go` | after a refusal, `RetryAfter` is greater than zero and no larger than `burst/rps` | — | S5 |
| `TestRetryAfterConsumesNothing` | `internal/ratelimit/ratelimit_test.go` | calling `RetryAfter` ten times does not reduce the tokens a subsequent `Allow` sees — asking when to retry is not a retry | — | S5 |
| `TestEvictIdleShrinksTheMap` | `internal/ratelimit/ratelimit_test.go` | with 100 keys used and the clock advanced past `idle`, `EvictIdle` returns 100 and `Len()` drops to 0 — **asserts the map SHRANK**, not that eviction was called | — | S6 |
| `TestEvictIdleKeepsActiveKeys` | `internal/ratelimit/ratelimit_test.go` | a key touched after the cutoff survives while a stale sibling is dropped — red if eviction clears the whole map, which passes the shrink test above on its own | — | S6 |
| `TestEvictedKeyStartsFresh` | `internal/ratelimit/ratelimit_test.go` | a key evicted and used again gets a full burst — the forgiveness ADR-0002 Risk 2 calls deliberate, written down so it is not later read as a bug | — | S6 |
| `TestConcurrentAllowIsRaceFree` | `internal/ratelimit/ratelimit_test.go` | 50 goroutines × 100 calls across 10 keys with `EvictIdle` running concurrently, under `-race` | — | S7 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the twelve tests above |
| 2 — something selects it | **nothing selects it yet** — this package has no caller until T3 mounts the middleware. Recorded honestly rather than claimed: T3's mutant deletes the middleware line and T4's deletes the `EvictIdle` call, and those are where rung 2 is actually discharged. |
| 3 — the caller can discover it | doc comments on every exported identifier; the flags that configure it arrive in T4 |
| 4 — it is used | `ocrr_requests_throttled_total{role}` in T3 is how throttling becomes observable; nothing measures it inside this task |

## Mutation Log

## Invariants

- A decision is never made from the wall clock; the caller's instant decides.
- A zero or negative `RPS` allows everything and stores nothing.
- `RetryAfter` never consumes a token.
- `EvictIdle` removes only entries older than `idle`.
- The map is the only shared mutable state, and it is always held under the mutex.

## Risks

- **`x/time/rate` has argument-less `Allow()` and `Wait()` forms that use the wall clock.** One
  call to either makes that path untestable and inconsistent with the rest. Mitigated by
  `TestClockIsInjectedNotWallClock`, which pins a non-advancing clock — a wall-clock call would
  make it pass spuriously as time really passes, so the test asserts REFUSAL under a frozen clock,
  where a wall-clock implementation eventually allows and goes red.
- **`TestEvictIdleShrinksTheMap` passes against an implementation that clears the entire map.**
  That is why `TestEvictIdleKeepsActiveKeys` exists beside it; neither is sufficient alone.
- **A burst test with `Burst` equal to 1 cannot distinguish burst from rate.** Every burst test
  here uses a burst of at least 3 so the two are separable.
- **`golang.org/x/time` is a new module.** It is pure Go, needs no cgo — which ADR-0001 forbids —
  and adds no transitive dependencies. Verified with `go mod graph` in S2.

## Stop Condition

Stop and ask if the operator would rather have no new module at all. The hand-rolled alternative is
argued in ADR-0002 §Alternatives; it is a real option and the answer changes this task entirely
rather than adjusting it.

## Out of Scope

- The HTTP middleware and the 429 response — T3.
- Flags, wiring, and the eviction tick — T4.
- Limits shared across processes (permanent: fact: ADR-0001 commits the router to a single
  process; citation: file `docs/adr/0001-ocr-router-architecture.md:521`).

## Verification Log
