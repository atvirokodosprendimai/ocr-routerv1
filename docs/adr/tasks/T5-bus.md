# Task ADR-0001-T5: Fan out per-user events in process, with no blocking and no leaked goroutines

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** S (single file)
**Owner:** unassigned
**Produces:** `bus.Bus` — `Subscribe(topic) (<-chan Event, func())`, `Publish(topic, Event)`, `Subscribers(topic) int`
**Consumes:** `core.Err*` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the drop-oldest send`, `the unsubscribe under lock`

## Goal

Deliver an event to every open SSE stream for one topic, without a slow reader ever
blocking the writer and without an abandoned subscription leaking a goroutine or a channel.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/bus/bus.go` | add | the in-process fan-out |
| `internal/bus/bus_test.go` | add | the failing tests |

`bus.Publish` is selected by `router.Service` on every write path (T6) and `bus.Subscribe`
by the SSE handler (T7); both tasks carry the call site and the mutation.

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write
   `bus_test.go` asserting that two subscribers both receive one publish and that a slow
   subscriber does not block the publisher, before any implementation (TDD red).
   [proof: acceptance]
2. [S2] Define `Event{Kind, JobID, Pages, Reason}` — a **notification**, carrying ids and
   small scalars only. It never carries rendered state or the OCR text: the reader
   re-queries, which is what makes two racing events for one job resolve correctly (ADR §7,
   `cqrs` §3).
3. [S3] Topics are strings. A client subscribes to its own `user:<id>`; a worker subscribes
   to **`workers:<label>`**, the label it serves, so a `strip-html` worker is never woken by
   `ocr` work. The admin dashboard subscribes to `admin`. Per-label worker topics are what
   keep a fleet of many service types from waking every process on every upload.
4. [S4] `Subscribe` returns a **buffered channel of 1** and an `unsubscribe` closure. The
   subscriber set lives in a `map[string]map[*subscriber]struct{}` behind one `sync.Mutex`
   — a mutex, not a channel handshake, because a handshake deadlocks against a loop that
   wants the same lock (`cqrs` §9).
5. [S5] `Publish` does a **non-blocking send with drop-oldest**: `select { case ch <- e:
   default: }` after draining one. Dropping a superseded notification is free — the next
   event makes the reader re-query anyway — and a blocking send would let one stalled
   browser stop every write in the process.
6. [S6] `unsubscribe` removes the subscriber **under the lock** and closes its channel
   exactly once (`sync.Once`), so a double call from a deferred cleanup plus an error path
   cannot panic on a closed channel.
7. [S7] An empty topic map entry is deleted when its last subscriber leaves, so a router
   serving a million users over its lifetime does not accumulate a million empty maps.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/bus/... -count=1 -race 2>&1 | tee /tmp/adr1-t5.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t5.out
```

Red at authoring: `internal/bus` does not exist. `-race` is not optional here — this is the
one package whose whole subject is concurrency.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestPublishReachesAllSubscribers` | `internal/bus/bus_test.go` | two subscribers on one topic both receive one publish, with every `Event` field intact | — | S2, S4, S5 |
| `TestPublishIsTopicScoped` | `internal/bus/bus_test.go` | a subscriber on `user:a` receives nothing published to `user:b` — the isolation that stops one customer seeing another's job ids | — | S3 |
| `TestWorkerTopicsAreLabelScoped` | `internal/bus/bus_test.go` | a subscriber on `workers:ocr` receives nothing published to `workers:strip-html` — the check that fails if labels are collapsed back to one `workers` topic | — | S3 |
| `TestSlowSubscriberDoesNotBlockPublisher` | `internal/bus/bus_test.go` | with a subscriber that never reads, N publishes all return promptly | — | S5 |
| `TestDropOldestKeepsLatest` | `internal/bus/bus_test.go` | after two publishes to a full buffer, the reader gets the **second** event, not the first | — | S5 |
| `TestUnsubscribeStopsDelivery` | `internal/bus/bus_test.go` | after `unsubscribe`, a publish is not delivered and the channel is closed | — | S6 |
| `TestUnsubscribeTwiceDoesNotPanic` | `internal/bus/bus_test.go` | calling the closure twice is safe | — | S6 |
| `TestEmptyTopicIsReaped` | `internal/bus/bus_test.go` | `Subscribers(topic)` is 0 and the internal map has no entry after the last unsubscribe | — | S7 |
| `TestConcurrentSubscribeUnsubscribePublish` | `internal/bus/bus_test.go` | N goroutines subscribing, publishing and unsubscribing concurrently trip neither the race detector nor a panic | — | S4, S6, S7 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the eight tests above |
| 2 — something selects it | `router.Service` publishes on every write path (T6); the SSE handler subscribes (T7). T7's mutation deletes the `Subscribe` call and its stream test goes red. |
| 3 — the caller can discover it | n/a: no declared interface — an internal Go package |
| 4 — it is used | T8's end-to-end test observes a `ready` event arriving on a live SSE stream |

## Mutation Log

## Invariants

- `Publish` never blocks.
- An `Event` carries ids and scalars, never rendered state or OCR text.
- Every `Subscribe` is matched by exactly one effective `unsubscribe`.
- The subscriber set is only ever mutated under the mutex.

## Risks

- **A blocking send** turns one stalled browser into a stalled router. S5 and
  `TestSlowSubscriberDoesNotBlockPublisher` are the guard.
- **`TestDropOldestKeepsLatest` is the test that is easy to get backwards.** Dropping the
  *newest* on a full buffer is equally easy to write and leaves the client looking at stale
  state forever; asserting *which* event survives is what separates the two.
- **This is the seam where NATS would be added** if the single-router decision ever
  reverses (ADR §1). Keeping `Event` free of rendered state is what keeps that cheap.

## Stop Condition

Stop and report if a design need appears for cross-process fan-out — that reverses an
explicit operator decision (ADR §Out of Scope) and is not an implementation detail.

## Out of Scope

- Cross-process fan-out / NATS (permanent: boundary: the operator chose one router).
- Deciding *what* to publish — T6 owns that; this task only delivers.
- SSE framing — T7's.

## Verification Log
