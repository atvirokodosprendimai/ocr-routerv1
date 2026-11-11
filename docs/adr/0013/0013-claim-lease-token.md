# ADR-0013: Give every claim its own lease token, so a worker that lost a job cannot write to it

**Status:** Accepted
**Date:** 2026-11-11
**Owner:** M
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0001-ocr-router-architecture.md`, `docs/adr/0008/0008-restart-is-not-a-failure.md`, `REPORT.md` (F4, F5)
**Governs:** `internal/store/repo_write.go`, `internal/store/repo.go`, `internal/store/migrations/00009_lease_token.sql`, `internal/core/job.go`, `internal/router/service.go`, `internal/httpapi/claim.go`, `internal/httpapi/upload.go`, `internal/httpapi/files.go`, `internal/agent/agent.go`, `internal/agent/release.go`, `README.md`
**Enforced-by:** `internal/store/reclaim_test.go::TestWritersRefuseAStaleClaimOfTheSameWorker`, `internal/router/leasetoken_test.go::TestStaleClaimOfTheSameWorkerIsRefused`
**Invalidates:** none — ADR-0001's "the worker's TOKEN id is the lease holder" is NARROWED: the token id still names who may act, and the lease token now names which claim.
**Served-path change:** A worker that lost a job's lease and then claimed the same job again (in another slot, or after a restart with the same worker token) can no longer complete, fail or release it through the older claim: the router answers `409` naming a stale lease, where it used to accept the older claim's result on the newer claim's row.

## Context

The lease holder is `jobs.worker_id`, the worker's TOKEN id (ADR-0001). F5
(REPORT 2026-11-10) put `worker_id = ?` into every processing writer, which
refuses a DIFFERENT worker that lost the job. It cannot refuse the SAME worker
token claiming the job twice, and that is the normal case, not an edge:

- One worker process runs several slots under one token. Slot A's command runs
  past `--lease`; the reaper reclaims the job; slot B of the same process claims
  it again. Both slots now pass `holdsLease` — same token, `processing`, a live
  lease — and whichever reports first wins, with the other one's report landing
  on the same row.
- An operator runs two worker processes with one token (the token is a
  credential, not an instance id, and nothing stops this).
- A worker restarts with its token, re-claims its own reclaimed job, and the old
  process's subprocess (still running) reports late.

In each case the router accepts a result for a claim that is not the current
one. With a pipeline that means one stage's output overwriting another's input
(the F6 class); with `release` it means the stale slot handing back a job the
live slot is running, so it runs twice.

## Existing Primitives Audit

| Primitive | Disposition |
|-----------|-------------|
| `jobs.worker_id` + the F5 guard on every processing writer | **Reuse unchanged.** It still decides WHO; the token adds WHICH CLAIM. |
| `holdsLease` (service) and the guarded SQL (store) | **Extend.** Both compare the token as well; the SQL guard is what closes the read-then-write window, as F5's did. |
| `jobLocks` / `lockHolder` | **Reuse unchanged**, re-checking the token under the lock. |
| `core.NewID()` (uuidv7) | **Reuse** as the token: unique per claim, no new dependency. |
| `/claim`, `/upload` (worker), `/files/{id}` (worker), `/release` | **Extend**, additively: the claim answer gains `lease`, the other three accept it. No route is added. There is no heartbeat route in this protocol; nothing extends a lease. |

## Decision

**Every claim mints a lease token, stored on the row and returned to the worker.**
`ClaimOneQueued` writes `jobs.lease_token` in the same statement that leases the
job; every writer that ends a claim (complete, advance, requeue, reclaim,
release, dead) clears it, as they clear `worker_id`. The claim answer carries it
as `"lease"`.

**A worker presents the token wherever it names the job.** It rides beside the
job id: `?lease=` on `GET /files/{id}`, `POST /release` and the raw
`POST /upload?job_id=`; `"lease"` in the JSON result body. The router refuses a
token that is not the row's current one with `409` and a message that says the
claim is stale and the result must be dropped. An expired lease is refused as
before (`409`), now saying so.

**The store guards on the token the router read.** Every processing writer gains
`AND lease_token = ?`, bound to the token of the snapshot the router checked.
So a reclaim plus a re-claim between the router's read and its write matches no
row, even for a worker that presented no token at all.

**Compatibility: warn and accept for ONE release.** A worker that sends no token
(every worker built before this record) is still authorised by its token id and
the lease's expiry, exactly as today, and the router logs a warning naming the
worker token id and the job. It keeps the SQL guard above, so it is strictly
safer than before; what it cannot get is the refusal of a same-token stale
claim at the moment of reading. The next release removes the empty-token path:
a request without `lease` becomes `400`. A new worker against an old router
works unchanged: the old router ignores the unknown query key and JSON field,
and sends no `lease`, so the worker sends none.

## Alternatives Considered

- **A per-process worker instance id instead of a per-claim token.** Refuses the
  second-process case but not the two-slots-of-one-process case, which is the
  common one. Rejected.
- **Compare `lease_expires_at` as the token** (it differs per claim). A second
  claim in the same second has the same value, and it is a time, not an
  identity. Rejected.
- **Refuse token-less workers at once.** Simplest, and every deployed worker
  stops on the router upgrade. Rejected for one release; it is the follow-up.
- **A header (`Ocr-Lease`) instead of query/body.** One place to read, but the
  job id already rides query or body, and a token beside its id is the shape a
  hand-written worker gets right. Rejected.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| `jobs.lease_token` (schema) | new `TEXT NOT NULL DEFAULT ''` | migration `00009` | `internal/store` |
| `core.Job.LeaseToken` | new field | `internal/store` | `internal/router`, `internal/httpapi` |
| `Repo.ClaimOneQueued(…, workerID, lease, …)` and the six processing writers | gain `lease`; guard `lease_token = ?` | `internal/store` | `internal/router` |
| `Service.Complete/CompleteRaw/Fail/ReleaseLease(…, workerID, jobID, lease, …)` | gain `lease`; `""` is the one-release legacy path | `internal/router` | `internal/httpapi` |
| `POST /claim` answer | gains `"lease"` | `internal/httpapi` | `internal/agent`, third-party workers |
| `?lease=` / `"lease"` on result, input fetch, release | new, optional for one release | workers | `internal/httpapi` |

## Consequences

- **Positive:** a result is accepted only from the claim that is current, so a
  slow slot and a fast slot of one worker can no longer both write one job.
- **Positive:** the SQL guard on the snapshot's token closes the
  read-then-write window for old workers too.
- **Negative:** one more value a hand-written worker must carry. A worker that
  drops it is warned for one release and refused after.
- **Neutral:** the lease's length and the reaper are unchanged.

## Out of Scope

- A heartbeat that extends a lease (deferred: none exists today; when one is added it carries the token like every other worker request)
- Removing the token-less path (deferred: the next release, after deployed workers are upgraded)

## Rollback

1. **Schema:** `00009` adds one column with a default; `goose down` drops it.
2. **Contract:** additive. A rolled-back router ignores `lease` and the new
   worker sends none when the claim carries none. Roll the router back first.

## Implementation

Sequential commits on `fix/claim-token-f11`: (1) store column, claim mint,
writer guards, router passes the snapshot token; (2) router checks the
presented token, legacy warn path; (3) HTTP surface; (4) worker agent.
