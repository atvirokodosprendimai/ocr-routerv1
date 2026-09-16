# ADR-0006: Carry binary worker output end to end as a raw passthrough mode

**Status:** Accepted
**Date:** 2026-09-16
**Owner:** M
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/BACKLOG.md`
**Governs:** `internal/runner/**`, `internal/results/**`, `internal/router/**`, `internal/blob/**`, `internal/httpapi/upload.go`, `internal/httpapi/files.go`, `internal/agent/**`, `internal/client/**`, `cmd/worker/**`, `cmd/client/output.go`, `internal/store/migrations/**`

<!-- Class: every component that carries a job's OUTPUT from the worker's stdout to the client's
file, plus the registry and pricing that decide a job's mode. Enumerated 2026-09-16 with
`git grep -ln 'Units\b|parseUnits|joinUnits' -- '*.go' | grep -v _test` → 14 files. Two of the 14
are deliberately excluded and named in Out of Scope: internal/bus (carries job ids, never output)
and internal/web/views (renders admin pages, not results). -->

**Enforced-by:** None — the check is `TestRawModeMismatchIsRefused`, which T2 creates; naming it before it exists would be the pointer-to-nothing this header exists to prevent. Set this to `internal/httpapi/raw_test.go::TestRawModeMismatchIsRefused` when T2 lands.
**Invalidates:** ADR-0001 — two clauses, each narrowed rather than removed: the worker subprocess contract `cmd -i <file> -o -` → JSON `[]string` on stdout (§Decision), which now holds for non-raw services only; and "results live in memory and are deleted once transferred" (§Decision, and `internal/results/store.go:1`), which now holds for non-raw results only.
**Served-path change:** A client submitting to a raw-mode service receives its worker's stdout bytes unchanged from `GET /files/{id}` — today any non-UTF-8 byte in that output is silently replaced with U+FFFD before the client ever sees it.

## Context

M, 2026-09-16: *"we need -raw because output could be binary not json"*. This is the deferred
backlog item **`?raw=1` — stdio passthrough billed at a flat 1 credit per request**
(`docs/adr/BACKLOG.md:36`, requested by M 2026-09-15). The binary requirement is new and it
changes the shape: the backlog entry says "whatever the worker writes on stdout comes back
unchanged" and never asked whether the existing transport *can* carry arbitrary bytes.

It cannot, and the failure is silent. A job's output is `[]string` from the worker's POST body
(`internal/httpapi/upload.go:115`) through the results store to the client's response
(`internal/httpapi/files.go:27`), and `encoding/json` replaces invalid UTF-8 with U+FFFD on
marshal — no error, changed bytes, changed length. Measured 2026-09-16 against Go 1.26.6 with a
PNG magic number:

```
in : 89 50 4e 47 0d 0a 1a 0a   (8 bytes)
out: ef bf bd 50 4e 47 0d 0a 1a 0a   (10 bytes)   round-trips intact: false
```

A `--raw` flag on the worker alone would therefore ship corrupted binary and report `204 No
Content`. That is why this is a record and not a flag.

The observed trigger: a worker whose `--cmd` printed `labas` failed with
`stdout is not a JSON array of strings (got "labas")`. That message is correct and the contract
it enforces (`internal/runner/runner.go:97`) is ADR-0001's, which also left an unticked box at
`docs/adr/0001-ocr-router-architecture.md:525` — *"Operator to supply the exact OCR command and
confirm the stdout contract"*. This ADR closes that box.

## Existing Primitives Audit

| Primitive | Disposition |
|-----------|-------------|
| `internal/blob` — content-addressed file store with atomic staged writes (`Put`, `Open`, `Delete`) | **Reuse unchanged.** It already carries arbitrary bytes for job INPUTS. A raw result is the same problem in the other direction. |
| `service_rates` table — admin-owned per-label pricing (`internal/store/migrations/00001_init.sql:105`) | **Reshape**: gains a `raw` column. It already exists precisely to keep a price out of a worker's reach, which is the property this ADR needs. |
| `jobs.pipeline` / `jobs.stage` — router-owned stage chaining, bridged by a blob (`internal/router/service.go:248`) | **Reuse unchanged.** The bridge between stages is already a blob, so a raw stage's output needs no new mechanism — `joinUnits` simply stops being involved. |
| `labelSeen map[string]time.Time` — the live-worker label registry (`internal/router/service.go:55`) | **Reshape**: must carry the worker's declared mode. Today a worker declares a label by subscribing to a bus topic named after it and carries no payload at all. |
| `http.MaxBytesReader` + streamed `blob.Put` on `/upload` (commit `b84f45a`) | **Reuse unchanged.** The client-upload path already streams without buffering; the raw result path is built the same way rather than differently. |

## Decision

Introduce a per-service **raw mode**. A raw service's worker writes arbitrary bytes on stdout; those
bytes reach the client unchanged, and the job costs a flat 1 credit.

**Three parties must agree on a label's mode, and the admin owns it.** `service_rates` gains a
`raw` column, set by an administrator exactly as `credits_per_unit` already is. A worker declares
its mode when it declares its label, and the router **refuses** a worker whose declaration
disagrees with the admin record. A client requests `?raw=1`, and the router refuses an upload
whose request disagrees. The job's mode is stamped on `jobs.raw` at admission and is immutable
thereafter, so a mid-flight change to `service_rates` cannot reprice a job already running.

> ⚠ **THIS DEVIATES FROM WHAT THE OWNER CHOSE ON 2026-09-16, DELIBERATELY, AND IT IS THE FIRST
> THING TO ACCEPT OR OVERRULE AT REVIEW.** M chose "worker declares, client must match" — two
> parties. ADR-0001 hit this exact shape and recorded the resolution as a security boundary: *"a
> worker advertising its own rate … would make a leaked worker token the ability to set what
> customers are charged. SPLIT THEM: validity is derived from live workers (cheap to derive,
> harmless if wrong); `credits_per_unit` lives in an admin-owned `service_rates` table."* `raw` **is
> a price** — flat 1 credit instead of `len(units) × rate` — so a worker declaring it is a worker
> setting its own rate through a second door, and "client must match" does not close it: a leaked
> worker token subscribes as label `ocr` declaring raw, and every raw request to `ocr` then bills 1
> credit instead of N, with no administrator involved. Adding the admin as the third party keeps
> M's "both must match" intact and puts the pricing half where every other pricing decision in this
> system already lives. Overruling this is a legitimate call; making it silently is not.

**The falsifying case, and whether data exists to produce it:** the mismatch refusal is decided by
comparing two stored values, so it fails whenever a worker declares a mode the admin record does not
carry. That case is constructible today with two lines of test fixture (`SetRate(label, n, raw=false)`
plus a worker declaring `raw=true`), which is what makes this a decision procedure rather than a
formality. It is valid for the SQLite-backed store this project ships; it says nothing about a
deployment that administers rates some other way, and there is none.

**Transport.** A raw result never becomes a string. The worker POSTs its bytes as the request body;
the router streams them into the blob store under the job id; `GET /files/{id}` streams them back
with `application/octet-stream` and deletes the blob on delivery. Nothing is base64'd — that would
cost +33% on the wire and, worse, put a whole binary result in the in-memory results store, which
ADR-0001 already lists as its unbounded-growth risk (`ocrr_results_in_memory`).

**Pricing.** A raw job costs 1 credit, charged **on delivery**, which is the single rule the ledger
already follows: nothing is charged before delivery, so a job that dies costs the customer nothing
and a double charge stays impossible.

**Pipelines compose.** A raw stage's output blob becomes the next stage's input blob directly. This
*removes* rather than extends `joinUnits`, whose newline-joining is documented as lossy
(`internal/router/service.go:289`) and would be actively wrong for binary.

**On branching within the worker arm.** ADR-0001 requires `/upload` to branch on the principal's
ROLE, never on the body or a header (`internal/httpapi/upload.go:22`). This ADR does not weaken
that. The role branch is unchanged and still first; *within* the already-authenticated worker arm,
`Content-Type: application/octet-stream` selects the raw payload shape and `application/json`
selects units-or-failure. The body cannot choose the mode, because `jobs.raw` was stamped at
admission and is the authority: a worker sending bytes for a non-raw job is refused, and a worker
sending units for a raw job is refused. The header selects a *representation* after the privilege
question is already settled.

## Alternatives Considered

- **Base64 the unit.** Worker `--raw` base64-encodes stdout into one unit; the client decodes.
  Transport untouched, smallest diff. Rejected: it puts the whole binary result in the memory-only
  results store at +33%, against an ADR-0001 risk that is already named and metered; and it needs a
  marker on the wire anyway, so it is not actually simpler than a blob once correct.
- **Worker-declared mode with no admin record (what M chose).** Two-party match, no schema change.
  Rejected on the ADR-0001 security boundary quoted in the Decision. Recorded here rather than
  omitted because it is the owner's stated preference and may be reinstated at review.
- **A separate `POST /results/{id}` route for raw.** Avoids any branching inside `/upload`.
  Rejected: a second worker-facing write route doubles the surface that must enforce the lease
  check, and the lease check is the thing protecting one customer's job from another's worker.
- **`?raw=1` as a pure per-request flag, any worker.** The original backlog shape. Rejected by the
  backlog entry's own analysis: *"a worker that ignores the mode silently returns unit-split output
  at a flat price."*
- **Refuse raw inside pipelines.** Smallest scope. Rejected by the owner 2026-09-16, and the
  mechanism is already there — the stage bridge is a blob.

## Component / Boundary Impact

| Component | Ownership after change | One reason to change? |
|-----------|------------------------|-----------------------|
| `internal/store` | Owns `service_rates.raw` and `jobs.raw`; still the only writer of schema | Yes — persistence |
| `internal/router` | Owns mode agreement, the mismatch refusal, raw pricing, and the raw stage bridge | Yes — job-state and ledger rules |
| `internal/blob` | Unchanged; now also holds results for raw jobs | Yes — bytes on disk |
| `internal/results` | Unchanged; holds non-raw results only | Yes — in-memory unit results |
| `internal/httpapi` | Owns the two refusals and the octet-stream representations | Yes — HTTP surface |
| `internal/runner` | Owns the stdout contract; gains a raw arm that skips `parseUnits` | Yes — subprocess contract |
| `internal/agent`, `cmd/worker` | Declares its mode, posts bytes | Yes — worker behaviour |
| `internal/client`, `cmd/client` | Requests raw, writes bytes | Yes — client behaviour |
| `internal/web` | Gains the admin control for `raw` beside the existing rate control | Yes — admin UI |

No component gains a second reason to change: raw is a mode threaded through existing owners, not
a new subsystem.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| `service_rates.raw` (schema) | new column, default 0, admin-owned | migration `00003` | `internal/store`, `internal/router`, `internal/web` |
| `jobs.raw` (schema) | new column, default 0, stamped at admission | migration `00003` | `internal/store`, `internal/router`, `internal/httpapi` |
| `GET /sse?label=<l>&raw=<0\|1>` | worker declares its mode alongside its label | `cmd/worker` | `internal/httpapi/sse.go`, `internal/router` |
| `POST /claim?label=<l>&raw=<0\|1>` | same declaration on the claim path | `cmd/worker` | `internal/httpapi/claim.go`, `internal/router` |
| `POST /upload?raw=1` (client) | client requests raw; refused unless the label's admin record agrees | `internal/client` | `internal/httpapi/upload.go` |
| `POST /upload?job_id=<id>` with `Content-Type: application/octet-stream` (worker) | raw result body is the bytes | `internal/agent` | `internal/httpapi/upload.go` |
| `GET /files/{id}` → `application/octet-stream` | raw jobs return bytes, not `{"units":[…]}` | `internal/httpapi/files.go` | `internal/client` |
| `core.ErrModeMismatch` | new sentinel → HTTP 409 | `internal/core` | `internal/httpapi` |
| `--raw` on `cmd/worker` | operator declares the worker serves a raw service | `cmd/worker` | operator |
| `--raw` on `cmd/client` | caller requests a raw service | `cmd/client` | operator |

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `Repo.ServiceMode(label)` / `Repo.SetRate(label, rate, raw)` | T1 | T2, T3, T8 | Yes — `SetRate` gains a parameter; every existing caller must be updated in T1 |
| `jobs.raw` column + `core.Job.Raw` | T1 | T3, T5, T6 | No — additive, defaults 0 |
| `Service.NoteWorker(label, raw)` + `core.ErrModeMismatch` | T2 | T4 | Yes — replaces `noteLabel`, which is unexported and has two call sites |
| `jobs.raw` stamped at admission | T3 | T5, T6 | No |
| worker raw result wire shape (`?job_id=`, octet-stream body) | T4 | T5 | No — new shape beside the existing one |
| raw result blob + `GET /files/{id}` octet-stream response | T5 | T6, T7 | No |

## Implementation

See `tasks/README.md` — 8 tasks in 5 waves.

## Consequences

- **Positive:** binary output works at all, and works without a silent corruption path. The proof
  that it is not corrupted is a byte-for-byte round trip, not an inspection.
- **Positive:** raw results never enter the in-memory results store, so the largest outputs are the
  ones that cost the router no RAM. `ocrr_results_in_memory` becomes a narrower, more honest metric.
- **Positive:** the lossy `joinUnits` newline encoding stops applying to the case it would damage
  most, without needing to be fixed.
- **Negative:** a third schema-backed mode agreement (admin, worker, client) is more moving parts
  than a flag, and three places can now disagree. Each disagreement is a refusal with a named
  sentinel rather than a silent divergence, which is the trade being made.
- **Negative:** `SetRate` is a breaking signature change with existing callers, including the admin
  UI. T1 carries the sweep.
- **Negative:** raw results now occupy disk until delivered or expired, where non-raw results
  occupy only memory. The blob reaper must cover them or they leak.
- **Neutral:** a raw job reports exactly one output, so per-unit metrics for raw labels are always
  1. Dashboards showing units/job will show a flat line for raw services; that is accurate.

## Out of Scope

- `internal/bus` and `internal/web/views/views_templ.go`, though both matched the class-enumeration grep — the bus carries job ids and never output bytes, and `views_templ.go` is generated from `.templ` sources that render admin pages rather than results (permanent: boundary: neither is on the path a job's output takes from stdout to the client)
- Streaming a raw result to the client *while* the worker is still producing it — delivery stays a complete-then-fetch handoff (permanent: boundary: the router cannot charge or retry a stream it has not finished receiving, and the lease/retry model in ADR-0001 assumes a whole result)
- Content-type negotiation or sniffing on a raw result; it is always `application/octet-stream` (permanent: boundary: the router never parses a raw payload, so any type it declared beyond octet-stream would be a worker's claim wearing the router's authority)
- Compression of raw results in transit (deferred: `docs/adr/BACKLOG.md`)
- Per-unit pricing for raw services — raw is flat-1 by definition here (permanent: boundary: "raw" means the router does not split the output, and a per-unit price requires units)
- Encryption of raw result blobs at rest; they sit unencrypted exactly as source blobs do today (permanent: fact: source blobs are already unencrypted on disk and a decision is already open for both; citation: file `docs/adr/BACKLOG.md:141`)
- Changing the non-raw units contract in any way (permanent: boundary: this ADR narrows where the JSON `[]string` contract applies and does not alter the contract itself)

## Risks

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| A worker is started `--raw` against a label the admin never marked raw, and every claim is refused | High | Med — the service silently serves nothing | T2's refusal is logged with both values and the label, and `cmd/worker` exits non-zero on a mode mismatch at startup rather than looping on refusals |
| Raw result blobs leak on disk for jobs that are never delivered | Med | High — unbounded disk growth | T5 extends the existing reaper's blob deletion to result blobs; T5's Acceptance includes an expiry case, and `ocrr_result_blobs` is added beside `ocrr_results_in_memory` |
| `SetRate`'s signature change misses a caller | Med | Med — admin cannot set a rate | Compiler catches every caller; T1's Affected Files names `internal/web/web.go:510` explicitly |
| A raw stage feeding a non-raw stage produces a blob the next tool cannot read | Med | Low — the job fails with the next stage's own error | Accepted and documented: the router does not inspect blobs, and a pipeline is the operator's composition. Named in the worker's README |
| The octet-stream branch inside the worker arm is read later as role-confusion and "fixed" back | Med | High — would reintroduce the units path for raw | The Decision states why it is not, and `TestUploadBranchesOnRoleNotBody` stays green unchanged; T5 adds a sibling test asserting the job's stamped mode overrides the header |

## Rollback

Persistent state and a public contract change, so both halves are needed.

1. **Schema:** migration `00003` is reversible — `goose down` drops `service_rates.raw` and
   `jobs.raw`. Both default to 0, so a database migrated down loses only which services were raw.
2. **Data:** raw result blobs live under the existing blob directory keyed by job id. After a
   rollback they are orphaned, not corrupting; the existing `blob.Delete` sweep removes them.
3. **Contract:** a rolled-back router refuses `?raw=1` as an unknown reserved param and a rolled-back
   worker never declares a mode, so an un-upgraded party degrades to a refusal rather than to silent
   unit-splitting. Roll the router back first, then workers — a raw worker against a non-raw router
   is refused, whereas the reverse would leave raw jobs queued with no worker to claim them.

## Follow-ups

- [x] Owner to accept or overrule the three-party mode agreement (Decision, boxed note) before execution starts — **ACCEPTED AS WRITTEN by M, 2026-09-16** ("implement this adr", given after the boxed note was put to him). The three-party agreement stands: the admin owns `service_rates.raw`, the worker declares and is refused on mismatch, the client requests and is refused on mismatch. T8 therefore stays in scope.
- [ ] Tick `docs/adr/0001-ocr-router-architecture.md:525` — "Operator to supply the exact OCR command and confirm the stdout contract" — once this record ships; ADR-0006 is what answers it, and that file is ADR-0001's to amend.
