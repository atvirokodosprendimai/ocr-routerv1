# ADR-0012: Publish a Session handle so one SSE stream can carry many jobs across process restarts

**Status:** Proposed
**Date:** 2026-09-29
**Owner:** M
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0011/0011-public-client-package.md`, `docs/adr/0005/0005-cmd-client.md`, `docs/adr/0006/0006-raw-passthrough.md`, `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/BACKLOG.md`
**Governs:** `client/session.go`, `client/services.go`, `client/session_test.go`, `client/services_test.go`
**Enforced-by:** `client/session_test.go::TestOpenReturnsOnlyAfterHelloAndBacklog`
**Invalidates:** none — checked. No exported symbol is removed, renamed or re-signatured, so ADR-0005's seam and ADR-0006's raw contract are untouched; `RetryableError` gains a field, which is additive. ADR-0011 is EXTENDED rather than invalidated: its §Out of Scope deferred "Exported `Upload()` and `Collect()` entry points" to `docs/adr/BACKLOG.md:309`, and this record claims that entry. ⚠ It does retire one sentence of ADR-0011's §Consequences — *"A caller who needs to submit and collect from different processes has no supported path and must wait for the deferred work below"* — which is the deferred work, now arriving. That line becomes historical rather than wrong, so ADR-0011 is left unedited per its own §Out of Scope on rewriting prose bodies. ⚠ It also QUALIFIES, without editing, ADR-0011 §Consequences' claim that the SSE-before-upload ordering is mutation-verified: review on 2026-09-29 found `TestStreamOpensBeforeUpload` records the request at handler ENTRY (`client/client_test.go:60`), before the `hello` delay at `:65-68`, so it asserts dial-before-upload rather than hello-before-upload. T1 adds the missing check; the older claim is narrower than it reads and is corrected here rather than in that record.
**Served-path change:** A Go program can hold ONE `/sse` connection open across a whole batch — `s, _ := client.Open(ctx, cfg)` — then `s.Upload(...)` many documents concurrently from several goroutines, persist each returned job id, and `s.Collect(ctx, id)` in response to `s.Events()` or, after a crash, to `s.Backlog()`, instead of paying one SSE connection per document and losing any job whose result arrived while the process was down.

## Context

M, 2026-09-29: *"look, there is client, just use it ... instead of inventing wheel"*, pointing at
`https://github.com/atvirokodosprendimai/ocr-routerv1/tree/main/client`.

**The caller he means cannot use it, and has already built the missing half.**
`e-tar-crawlerv1` (`github.com/teisora/e-tar-crawlerv1`, `internal/ocr`, **659 lines plus 435 lines
of pipeline routing and 997 lines of test**, read 2026-09-29 at commit `0d79ccd`) is a
hand-written second implementation of this exact protocol. Its shape is not an accident and not a
stylistic preference — it is three requirements `Submit` does not meet:

1. **One stream, many jobs, uploaded concurrently.** `Submit` calls `openStream` internally
   (`client/client.go:136`), so N concurrent documents mean N `/sse` connections. The router's stream
   is keyed on the CLIENT TOKEN and already carries every job's events, so the crawler holds one
   (`internal/crawl/attachments.go:160`, *"ONE SSE STREAM SERVES EVERY WORKER"*) and uploads onto it
   from N worker goroutines (`internal/crawl/attachments.go:408-427`).
2. **Upload and collect are separate, and the id is persisted between them.** The crawler writes the
   job id to its store before waiting, so a run that dies mid-job finds its own work in the next
   stream's `backlog` instead of paying for it twice (`internal/ocr/ocr.go:355-360`, and
   `internal/crawl/attachments.go:336-337` for the persist, `:308` and `:516-523` for the resume).
3. **The events drive backpressure.** When the router answers `429 {"error":"buffer full"}`, what
   frees a slot is a COLLECTION, so the crawler waits on a stream event rather than a clock
   (`internal/crawl/attachments.go:186-200` for the broadcast, `~:480` for the wait). A
   `Submit`-shaped caller has no event to wait on.

**This is the deferred item, and the backlog already specified its acceptance criterion.**
`docs/adr/BACKLOG.md:309` defers exactly this, and at `:325-326` states the constraint: *"Whatever
shape this takes has to make the wrong order unrepresentable or survivable."* That sentence is what
this record answers — and the honest answer is **survivable**, not unrepresentable; see Decision.

★ **The duplicate knew two things this record's first draft did not, and both were found by reading
it rather than by reasoning.** A cold review on 2026-09-29 caught them:

- **Establishment is `hello` AND the first `backlog`, not `hello` alone.** The router sends them in
  that order, unconditionally (`internal/httpapi/sse.go:87-98`). An `Open` that returns on `hello`
  wins the race against the upload and **loses the `backlog` frame to the event pump**, so
  `Backlog()` is silently empty — and a silently empty backlog is a run that re-recognises and
  re-pays for every job a previous run had already finished. `internal/ocr/ocr.go:231-243` says
  exactly this, with `establishGrace = 2 * time.Second` (`ocr.go:289`) so a router that sends no
  backlog cannot hang the caller.
- **`client.Submit` is not exposed to this** and that is why the gap reads as absent: it also handles
  a later `backlog` frame inside its own wait loop (`client/client.go:191-202`), so
  `openStream` "already returning the backlog map" is sufficient for `Submit` and insufficient for
  anything that reads `Backlog()`.

**What went wrong on 2026-09-29, and why it is in this record.** M reported the OCR stage "fails
catastrophically" and diagnosed it as a client that polls instead of subscribing. That diagnosis was
falsified in all three implementations — `client.Submit` waits for `hello` before uploading,
`internal/agent.listen` drains on `work`/`backlog`/`ping`, and the crawler waits on its stream. The
actual cause was capacity arithmetic: `defaultBufferLimit = 4`
(`internal/identity/service.go:25`) against a 214,030-document corpus at 31.6-94.9 s/unit taken
2026-09-29 from the crawler's own progress output — a figure from a terminal run, with no artifact in
either repository, so treat it as reported rather than measured — with `cmd/worker`'s `slots`
defaulting to 2 (`cmd/worker/main.go:56`). **That is NOT what this ADR fixes** and it is listed in
Out of Scope; it is recorded here because the duplicate protocol package is what made three sessions
look for the bug in a wait loop, and de-duplicating it is the durable part of M's instruction.

## Existing Primitives Audit

- **`client.Config`, `client.Input`, `client.Result`, `client.Stage`, `client.Progress`,
  `client.FailedError`, `client.RetryableError`** — REUSE, unchanged in name and signature.
  `Session.Upload` takes the existing `Input` rather than a new parameter list, which is what makes
  the crawler's `Pipeline{Label, Raw}` collapse into fields that already exist (`Input.Label`,
  `Input.Raw`). `RetryableError` is RESHAPED by one additive field, `RetryAfter`.
- **`client/sse.go` — `frame`, `readFrames`, `frame.decode`, `backlogPayload`, `event`** — REUSE
  verbatim. Frame reassembly is already correct here (it reassembles rather than treating a read as a
  frame, `client/sse.go:37-47`) and it already parses the router's actual wire shape,
  `"event: %s\ndata: %s\n\n"` (`internal/httpapi/sse.go:80` against `client/sse.go:68-72`). No new
  parsing code is written by this record.
- **`client.Submit`'s unexported `openStream` / `upload` / `collect`** — RESHAPE into
  `Session`. They are already the three operations this record exports. ⚠ `openStream` is NOT
  sufficient as-is: it returns on `hello`, which is correct for `Submit` and wrong for a handle that
  exposes `Backlog()` (Context). The reshape is a relocation PLUS the establishment fix.
- **`internal/agent`'s reconnect loop (`internal/agent/agent.go:97-124`)** — REUSE as a PATTERN,
  not as code. It is this repository's existing answer to a dropped stream, and `Session` follows its
  shape for `Events()` closing rather than inventing one. ⚠ Reconnection itself is NOT in this
  record (Out of Scope): `Session` reports a dead stream by closing `Events()` and the caller
  re-`Open`s, which is what makes `Backlog()` the resume primitive.
- **`internal/httpapi`'s `GET /services`** — REUSE, unchanged. It is routed at
  `internal/httpapi/api.go:157` and answers `{"labels":[…]}` sorted
  (`internal/httpapi/claim.go:94-106`), so T4's `Services` reads an endpoint that already exists in
  the shape it assumes.
- **`e-tar-crawlerv1/internal/ocr`** — REPLACE, in that repository, not this one. Its `Session`,
  `Open`, `Submit`, `Collect`, `Events`, `Backlog`, `Close`, `Event`, `ErrNotReady`, `retryAfter`,
  `frame` and `readFrames` are what this record publishes; its `Pipeline` type and extension-to-label
  routing table are domain policy and stay there (verified standalone: `pipelines.go` references
  none of `Config`, `Result` or `Session`). ⚠ Two of its symbols are deliberately NOT published —
  see Wiring for `Config.Enabled` and the 409 diagnosis.
- **A `client.New(cfg) *Client` handle** — NOT built. See Alternatives; it remains deferred at
  `docs/adr/BACKLOG.md:328`, and this record deliberately does not pre-empt it.

## Decision

Add a `Session` handle to the `client` package, and re-implement the existing package-level `Submit`
on top of it so there is exactly ONE protocol implementation in this repository.

```go
func Open(ctx context.Context, cfg Config) (*Session, error)   // returns after `hello` AND the first `backlog`
func (s *Session) Upload(ctx context.Context, in Input) (jobID string, err error)
func (s *Session) Collect(ctx context.Context, jobID string) (Result, error)
func (s *Session) Events() <-chan Event                        // closes when the stream ends
func (s *Session) Backlog() map[string]bool                     // a COPY, snapshotted at Open
func (s *Session) Close() error                                 // idempotent

type Event struct{ JobID string; Ready bool; Reason string }
var ErrNotOpen  = errors.New(...)   // a Session that Open did not return, or one already Closed
var ErrNotReady = errors.New(...)   // 404 from /files/<id>
// additive: RetryableError gains RetryAfter time.Duration, parsed from the Retry-After header
func Services(ctx context.Context, cfg Config) ([]string, error) // + ErrNoServices
```

**Establishment is `hello` plus the first `backlog`, bounded by a grace period.** The router sends
`hello` then, for a client role, `backlog` (`internal/httpapi/sse.go:87-98`). `Open` waits for both,
accumulating the backlog into the map `Backlog()` returns, and gives up waiting for the second after
a bounded grace so a role or a router that sends no `backlog` cannot hang the caller. Returning on
`hello` alone would leave `Backlog()` empty in production while every test that sent the frames in
the other order passed — the failure `internal/ocr/ocr.go:231-243` records having already hit.

**How the wrong order is prevented, stated accurately.** `docs/adr/BACKLOG.md:325-326` asks for the
wrong order to be "unrepresentable or survivable". **It is survivable, and it is refused at runtime —
it is NOT unrepresentable, and an earlier draft of this record claimed that it was.** The claim was
false: unexported fields stop a caller SETTING them, but `&client.Session{}` and
`var s client.Session` are legal from any package, so a compiler cannot be made to reject
`s.Upload(...)` on a session `Open` never returned. Verified 2026-09-29 by compiling a two-package
module of exactly this shape — it builds. So the mechanism is explicit instead:

- `Session` carries an unexported `established` marker that only `Open` sets, and `Upload`,
  `Collect` and `Events` check it, returning `ErrNotOpen` on a zero-value or closed session rather
  than panicking, blocking forever on a nil channel, or silently uploading into a stream nobody
  holds. A typed error is the point: the caller can tell this apart from a transport failure.
- `Backlog()` makes the ordering **survivable**: a result that arrived before this stream existed is
  still collectable, so even a caller that gets the order wrong, or crashes between upload and
  collect, loses nothing.
- What the handle genuinely buys over two package-level functions is that there IS a receiver to
  carry that marker and the stream's lifetime. With `upload(cfg, …)` and `collect(cfg, …)` as free
  functions there is no object to have been established, so the ordering could not be checked at
  all — not at compile time and not at runtime. That, and not compile-time impossibility, is the
  argument against Alternative 1.

**What would make this decision wrong, and whether it is observable today.** The falsifier is that
re-implementing `Submit` on `Session` changes `Submit`'s observable behaviour — that the four
properties ADR-0011 §Consequences names (SSE-before-upload ordering, backlog replay, job-id match,
typed errors) do not survive. **Three of the four are observable today with no new instrument:**
`client/client_test.go` and `client/raw_test.go` are 776 lines of `package client_test` driving them
through the exported surface, and they must pass **byte-unchanged** (T3's Acceptance forbids editing
them). ⚠ **The fourth is not, and this is a correction to an earlier draft that claimed all four
were.** `TestStreamOpensBeforeUpload` records the SSE request at handler entry
(`client/client_test.go:60`) BEFORE the `hello` delay (`:65-68`), so it proves dial-before-upload and
would stay green if `Open` or the new `Submit` lost the `hello` wait. T1 adds
`TestOpenReturnsOnlyAfterHelloAndBacklog` for exactly that hole, and it is this record's
`Enforced-by:` check. The backlog replay keeps the gap ADR-0011 recorded — a test and no mutant —
neither widened nor closed here.

**What this does NOT claim.** It does not make the crawler faster, and it does not change what the
router admits. The stall M reported is `buffer_limit` × `slots` arithmetic (Context), and no line of
this record moves either number.

## Alternatives Considered

- **Export `Upload` and `Collect` as package-level functions taking `Config`:** the literal reading of
  ADR-0011's deferred entry. Rejected — with no receiver there is nothing that can have been
  established, so the ordering cannot be checked even at runtime and degrades to a doc comment. The
  failure it prevents (a fast job fires `ready` into a stream nobody holds; the caller waits forever)
  is precisely the one that passes every casual test and strands a production run on the jobs that
  went well. ⚠ An earlier draft rejected this alternative for making the ordering "no longer a
  compile-time property". That reason was wrong — the handle version is not compile-enforced either
  (Decision) — and the real reason is the missing receiver.
- **`client.New(cfg) *Client` with `Submit`/`Upload`/`Collect` methods:** the conventional SDK shape,
  deferred at `docs/adr/BACKLOG.md:328`. Rejected for now — it is a different decision about
  ERGONOMICS (where `Config` lives) and would rewrite every existing call site and all 776 lines of
  test, whereas `Session` is about STREAM LIFETIME and adds a type beside them. Doing both at once
  would mean a behaviour-preserving refactor and an API reshape in one diff, with no way to attribute
  a failure to either. The entry stays open and this record does not pre-empt its answer.
- **Leave `client.Submit` as the only entry point and let the crawler keep `internal/ocr`:** the
  status quo. Rejected — it is the shape M objected to, and it has a measured cost beyond
  duplication: the crawler's copy buffers a whole document in memory to build its multipart body
  (`internal/ocr/ocr.go:388-403`), which is the same defect this package carries
  (`client/client.go:289-306`), and fixing it in one place fixes it for one caller. Two live copies
  of a protocol is the shape where one is fixed and the other is not — and it already cost this
  record two blockers, since the duplicate had solved the establishment problem and this record's
  first draft had not (Context).
- **Have the crawler call `client.Submit` per document, deleting `internal/ocr` outright:** M's
  literal instruction, and the smallest crawler diff. Rejected on evidence, after being raised with
  him on 2026-09-29: it turns one SSE connection into one per concurrent document, and it discards
  the persisted-job-id resume, so a crash after upload orphans an uncollected job — and an
  uncollected job holds a `buffer_limit` slot (`internal/store/repo.go:302-313`), which makes the
  `buffer full` stall he reported strictly worse. He chose this record's direction instead.
- **Make `Session` an interface, so the zero value cannot be constructed at all:** it would make the
  ordering genuinely unrepresentable, since an interface has no composite literal. Rejected — it
  forces every consumer to accept an interface they cannot implement usefully, loses `go doc`'s
  rendering of the concrete type's fields and methods together, and buys a guarantee the
  `established` marker already provides as a typed error. Recorded because it is the one shape that
  would have made the retired claim true.
- **Give `Session` its own reconnect loop, mirroring `internal/agent/agent.go:97-124`:** rejected for
  this record. Reconnection needs a policy (how long, how many, what happens to an in-flight
  `Upload`) that only a caller can set, and `Events()` closing plus `Backlog()` on re-`Open` already
  lets a caller implement any of them. Deferred rather than refused.

## Component / Boundary Impact

Inherits ADR-0005's seam unchanged: `client/` is a library with no flags, no terminal output and no
exit codes; `cmd/client` owns all three. `Session` adds a lifetime to the library half and nothing
else moves.

| Component | Ownership after | One reason to change? |
|---|---|---|
| `client/` | The router's wire protocol as a library, now with an explicit stream lifetime. Public contract. | Yes — it changes when the router's wire format changes. |
| `client/session.go` (new) | The `/sse` connection's lifetime and establishment, and the three operations that ride it. | Yes — it changes when the stream's frames or lifecycle change. |
| `client/client.go` | `Submit`, now a thin composition over `Session`. Keeps every existing symbol. | Yes — it changes when the one-shot convenience shape changes. |
| `client/services.go` (new) | Reading `GET /services`. | Yes — it changes when that endpoint changes. |
| `cmd/client/` | Flags, rendering, exit codes. **Unchanged — not a file is edited.** | Yes — when the CLI's UX changes. |
| `e-tar-crawlerv1/internal/ocr` | Deleted, in that repository, by whoever does that work. | n/a — not this corpus's to change. |

The boundary property that changes: `client/` acquires a type with a `Close()`, so a consumer can now
leak a connection, and a type that is safe for concurrent use, which is a promise it must keep. Both
are named in Consequences.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| `client.Open`, `client.Session` + its six methods | **New public API.** A caller holds one stream and drives many jobs on it, concurrently. | `client/session.go` (T1, T2) | `e-tar-crawlerv1`; any external Go module |
| `client.Event` | **New public type.** Terminal-state notification carried on `Session.Events()`. | T1 | as above |
| `client.ErrNotOpen` | **New public sentinel.** A method called on a `Session` that `Open` did not return, or one already `Close`d. Exists because the zero value is constructible from any package (Decision). | T1 | as above |
| `client.ErrNotReady` | **New public sentinel.** `GET /files/<id>` answered 404 — ambiguous by construction (queued, failed, expired, or another customer's), so it is not "wait longer". | T2 | as above |
| `client.RetryableError.RetryAfter` | **Additive field**, `time.Duration`, parsed from the `Retry-After` header. Zero means the server named none — **not** "retry immediately". Existing construction sites leave it zero, so no consumer breaks. | T1 | as above |
| `client.Services`, `client.ErrNoServices` | **New public API.** Lists the labels a router serves, so a caller can fail early on an unserved label. | T4 | as above |
| `client.Submit` | **Re-implemented, signature and behaviour unchanged.** Becomes `Open` + `Upload` + wait + `Collect`. | T3 | `cmd/client`; existing external consumers |
| `GET /sse`, `POST /upload`, `GET /files/<id>`, `GET /services` | **No change.** This record adds no endpoint, no parameter and no frame; it is a client-side reshape of calls the router already serves. | — | — |
| `README.md` § The client | New subsection for the Session shape beside the existing `Submit` shape. | T4 | a developer or agent adopting the package |
| `ocr.Config.Enabled()` (crawler) | **NOT published.** `client.Config` has no `Enabled()`, and its field is `RouterURL` where the crawler's is `BaseURL`. Whether an unconfigured router means "skip this stage" is caller policy, not protocol — the crawler keeps a two-line helper over its own config. | — | `e-tar-crawlerv1` (3 sites: `cmd/crawler/main.go:315`, `:585`, `internal/crawl/attachments.go:225`) |
| A status code on a client error | **NOT added by this record**, and it is a known loss for the porting consumer: `internal/ocr/ocr.go:513-523` classifies 409 fatally AND explains it ("the mode in OCR_PIPELINES disagrees with how this label is configured on the router"). `client/client.go` has no 409 branch and exposes no status code, so the crawler keeps the fatal/retryable split but loses the message. | — | `e-tar-crawlerv1` |

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `Session` + `Open()` + `Events()` + `Backlog()` + `Close()` + `Event` + `ErrNotOpen` + `RetryableError.RetryAfter` (T1) | T1 | T2, T3, T4 | No — T1 adds types and symbols; nothing existing changes shape. T2/T3/T4 cannot compile before T1 lands. |
| `(*Session).Upload()` + `(*Session).Collect()` + `ErrNotReady` (T2) | T2 | T3, T4 | No — additive methods on T1's type. T3 replaces `Submit`'s body with calls to them, so T3 cannot start before T2. |
| `Submit` re-implemented over `Session` (T3) | T3 | T4 | No — signature and behaviour are held identical by T3's Invariants and its unchanged-test fence. T4 documents both shapes and needs `Submit` final. |

## Implementation

See `tasks/README.md`. Four tasks, sequential.

## Consequences

- **Positive:** the deferred backlog item is answered, and answered on the criterion its own entry set
  — the wrong order is survivable via `Backlog()` and refused via `ErrNotOpen`, rather than merely
  documented.
- **Positive:** one protocol implementation in this repository instead of two. `Submit` composes
  `Session` (T3), so a wire-format fix lands once and both entry points get it. The 776 lines of
  existing test become the regression net for that composition rather than covering only one path.
- **Positive:** a bulk caller pays one `/sse` connection per process instead of one per document, and
  a job whose result arrived while the caller was down is collectable via `Backlog()` rather than
  lost and re-paid-for.
- **Positive:** the establishment bug the first draft would have shipped is fixed before any code
  exists, because the duplicate being deleted had already recorded the answer. That is an argument
  for reading a duplicate before removing it, and it is why §Context names it.
- **Positive:** `e-tar-crawlerv1` can delete ~1,100 lines of duplicated protocol and keep only its
  routing policy. ⚠ That deletion is in another repository and is NOT evidence this record can
  produce; it is the motivation, not the acceptance.
- **Negative:** `client/` gains a resource with a lifetime. A consumer that forgets `Close()` leaks a
  goroutine and an HTTP connection until its context is cancelled — a failure `Submit` made
  impossible by construction. Mitigated only by documentation and the `Example` in T4; nothing in
  this repository can detect a downstream leak.
- **Negative:** `*Session` becomes a type this package PROMISES is safe for concurrent use, because
  the served path is concurrent uploads. That is a permanent obligation on every future change to it,
  and a data race in it would surface in a consumer's batch run rather than here. T1 and T2 carry the
  invariant and a concurrent test; `-race` over sequential tests would prove nothing.
- **Negative:** the public surface roughly doubles (one function and four types become two functions,
  six methods and seven types). Every added symbol is a permanent compatibility obligation for a
  consumer this repository cannot grep, which is the cost ADR-0011 accepted and this record enlarges.
- **Negative:** `Submit` stops being a self-contained read. Understanding it now requires reading
  `Session` too. That is the price of one implementation instead of two, and it is the trade this
  record chooses deliberately.
- **Neutral:** `cmd/client` is not edited and behaves identically — same flags, same exit codes, same
  output. Its 590-line test file is untouched and stays green.
- **Neutral:** no router-side code, endpoint, frame or database row changes. The router cannot tell a
  `Session` caller from a `Submit` caller, because on the wire they are the same three calls.
- **Neutral:** the crawler's in-memory multipart buffering is reproduced, not fixed — `Session.Upload`
  is a relocation of the existing `upload`, which buffers (`client/client.go:289-306`). Streaming it
  is listed in Out of Scope so the refactor stays provably behaviour-preserving.

## Out of Scope

- Reconnecting a dropped stream inside `Session`, and any retry policy — `Events()` closes and the
  caller re-`Open`s, with `Backlog()` as the resume primitive (deferred: `docs/adr/BACKLOG.md`)
- Streaming `Upload`'s multipart body instead of buffering the whole document in memory, which is a
  real defect in the code this record relocates (deferred: `docs/adr/BACKLOG.md`)
- A `client.New(cfg) *Client` handle, which is a separate decision about where `Config` lives
  (deferred: `docs/adr/BACKLOG.md`)
- Exposing an HTTP status code on the package's errors, which is what would let a consumer re-attach
  its own 409 diagnosis (deferred: `docs/adr/BACKLOG.md`)
- Deleting `e-tar-crawlerv1/internal/ocr` and porting that crawler onto this package — the motivating
  work, and the only place the duplication actually disappears (external: `github.com/teisora/e-tar-crawlerv1`: `internal/ocr`, `internal/crawl/attachments.go`)
- The crawler's `Pipeline` type and its extension-to-label routing table (permanent: boundary: which
  service a `.docx` versus a `.pdf` should go to is domain policy owned by the caller that knows its
  corpus; this package carries `Input.Label` and `Input.Raw` and takes no view on how a caller picks
  them, exactly as ADR-0005's seam keeps policy out of the protocol library)
- An `Enabled()`-style predicate on `client.Config` (permanent: boundary: "no router configured means
  skip the stage rather than fail it" is a decision about a caller's pipeline, and a protocol library
  that answers it starts owning the caller's error policy)
- Raising `defaultBufferLimit` or the worker's `slots` default, which is what actually throttles the
  214,030-document crawl that prompted M's report (permanent: fact: admission is gated on
  `inFlight >= u.BufferLimit` with the default set to 4, so throughput is an operator/capacity
  decision about a customer's row and a worker's flags rather than a client-library one; citation:
  file `internal/identity/service.go:25`)
- Any change to `cmd/client` (permanent: boundary: ADR-0005 owns the CLI's UX and this record changes
  no behaviour it could surface, so editing it would put an unattributable diff inside a
  behaviour-preserving refactor)
- A polling fallback for proxies that buffer SSE (deferred: `docs/adr/BACKLOG.md`)
- Tagging a semantic version for the package (permanent: fact: the module is at `v0` with no tags, so
  a consumer pins a pseudo-version, and release policy is a decision for whenever the first tag is
  cut; citation: file `go.mod:1`)

## Risks

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| `Open` returns before the router has subscribed, reintroducing the race the handle exists to prevent | Med | High | `TestOpenReturnsOnlyAfterHelloAndBacklog` (the `Enforced-by:` check) holds the frames back and asserts `Open` has not returned; T1's mutation target is the wait itself. ⚠ It is a NEW test because the pre-existing `TestStreamOpensBeforeUpload` records at handler entry and cannot catch this (Decision). |
| `Backlog()` is empty in production because establishment ended at `hello` | **Was High — this is the blocker the review caught** | High | Establishment is `hello` + first `backlog` with a bounded grace (Decision); T1's stub emits the ROUTER'S order (`hello` then `backlog`), not the reverse, and a test asserts a no-backlog role still returns within the grace. |
| A caller uses a zero-value or closed `Session` and gets a hang instead of an error | Med | Med | The `established` marker and `ErrNotOpen`, with a test per path: zero value, `Open` error ignored, and after `Close`. |
| A data race in `Session` under concurrent `Upload`, surfacing in a consumer's batch rather than here | Med | High | Concurrency is the served path, so T2 carries an explicit concurrent-`Upload` test under `-race` and T1/T2 carry the invariant. Sequential tests under `-race` prove nothing here, which is why the test is named separately. |
| The `Submit` re-implementation silently changes behaviour | Med | High | T3 may not edit `client/client_test.go` or `client/raw_test.go`; `git diff --exit-code` over both is the first segment of its fence. Bounded honestly: those tests cover three of the four properties, and the fourth is covered by T1's new test rather than by them. |
| `Collect`'s new `url.PathEscape` changes `Submit`'s observable behaviour for an exotic job id | Low | Low | Declared in T2 rather than smuggled: no existing test distinguishes it because ids are `job-1`, so T2 states it and T3's Stop Condition names it as an expected, declared difference rather than a surprise. |
| `Submit`'s progress stages diverge on the backlog path, which no existing test covers | Med | Low | T3 re-emits `StageCollecting`/`StageDone` at BOTH call sites and says so in its steps; `TestProgressCallbackSequence` covers only the event path, which T3 records rather than relying on. |
| `Events()` never closes on a dead stream, so a caller blocks forever instead of re-`Open`ing | Med | Med | `TestEventsClosesWhenStreamDies` in T1; `readFrames` already closes its channel on stream end (`client/sse.go:44-48`) and `pump` defers `close(s.events)`. |
| `Close()` races the pump goroutine, or panics on a second call | Med | Med | T1 makes `Close` idempotent behind a mutex and runs the whole package under `-race`. |
| A `Collect` is issued twice for one job, charging twice and finding the result gone | Low | High | `Collect` is unchanged in semantics from the existing `collect`; `ErrNotReady` gives the 404 a name so a caller can tell "not yet" from "already taken". The one-collection-per-job DISCIPLINE stays the caller's and is documented as such. |
| `RetryAfter` is read as "retry immediately" when it is zero | Med | Med | Zero-means-unspecified is in the field's doc comment with the reason, and T1's test asserts a header-less 429 yields exactly zero. |
| The public surface grows faster than it is documented, so an adopter uses `Submit` and never finds `Session` | Med | Low | T4 adds a compiled `Example` and a README section with a check that fails when the README names a symbol the package does not declare. |

## Rollback

`git revert` the four task commits, newest first. Safe and complete: this record adds files and
re-implements one function body, touching no persistent state, no migration, no wire format, no
endpoint and no stored data — a reverted tree is byte-identical to `8e6f81a` in every package but
`client/`, and identical there too once T1-T4 are undone. `cmd/client` is not edited at all, so
nothing user-facing moves either way.

The one thing a revert cannot recall is an external consumer that has already pinned
`client.Open` — the published-surface cost ADR-0011 named and this record enlarges. Reverting after
that point is a breaking change for code this repository cannot see; the mitigation is that the
module is at `v0` with no tags, so a consumer pins a specific pseudo-version and is not dragged
backwards by a revert on `main`.

## Follow-ups

- [ ] M to decide whether `e-tar-crawlerv1` is ported onto this package in the same working session
      or as its own piece of work — it is the motivation for this record and lives in another
      repository, so no task here can carry it.
- [ ] Add ADR-0012 to `client/public_test.go`'s `TestEnforcedByPointersResolve` list, which today
      iterates a hardcoded set of ADR-0005/0006/0011 only (`client/public_test.go:121-128`) — so this
      record's own `Enforced-by:` pointer is unverified by that check until it is added. T1 writes the
      test it names; nothing yet asserts the POINTER resolves.
- [ ] Revisit the `client.New(cfg) *Client` backlog entry once this lands: the package now has two
      entry points taking `Config`, which is the condition `docs/adr/BACKLOG.md:333` names as the
      moment passing it per call "starts to read as an omission rather than as simplicity".
