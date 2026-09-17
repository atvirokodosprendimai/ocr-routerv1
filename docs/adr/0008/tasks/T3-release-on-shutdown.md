# Task ADR-0008-T3: Let a worker hand its leases back when it is told to stop

**Depends-on:** T2
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `POST /release?job_id=`, `Service.ReleaseLease`, worker release on shutdown
**Consumes:** `Repo.ReclaimJob` (T1), `Service.reclaimJob` and its budget (T2)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the lease guard on the release route`, `a release spending neither budget`, `the worker releasing what it holds on shutdown`

## Goal

Restarting a worker returns its in-flight jobs to the queue immediately, costing
the job nothing.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/router/service.go` | edit | `ReleaseLease` — the same `holdsLease` guard as `Complete` and `Fail`, then a requeue that spends neither budget |
| `internal/httpapi/claim.go` | edit | `POST /release`, worker-only, beside the claim it undoes |
| `internal/httpapi/api.go` | edit | The route — the line that SELECTS this; without it the handler is unreachable |
| `internal/agent/agent.go` | edit | Track in-flight job ids; release them when the context is cancelled |
| `internal/httpapi/release_test.go` | add | Route and guard tests |
| `internal/agent/release_test.go` | add | Worker-side tests |

## Ordered Steps

1. [S1] Write the failing tests: `TestReleaseRequeuesWithoutSpendingEitherBudget`, `TestReleaseWithoutTheLeaseIsRefused`, `TestWorkerReleasesOnShutdown`. Confirm red. [proof: acceptance]
2. [S2] Add `Service.ReleaseLease`: `holdsLease` first, then requeue clearing `worker_id` and the
   lease, incrementing NEITHER counter. ⚠ The guard is not optional duplication — it is the third
   place it appears, and a release without it lets any worker requeue a job another is running.
3. [S3] Add `POST /release?job_id=` to the worker-only group, returning 204, and 409 without the
   lease. Put it beside `handleClaim`: it is the claim's inverse and they should be read together.
4. [S4] Track in-flight job ids on the agent, and on context cancellation release each one. ⚠
   BEST EFFORT on a bounded timeout: the process is already shutting down, and a release that hangs
   would turn a fast restart into a slow one — which is the problem this task exists to remove.
5. [S5] A release failure is logged and never fatal. The lease expiry is still the safety net, so
   the worst case is today's behaviour. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/httpapi ./internal/agent -run 'TestReleaseRequeuesWithoutSpendingEitherBudget|TestReleaseWithoutTheLeaseIsRefused|TestWorkerReleasesOnShutdown|TestReleaseFailureIsNotFatal' -count=1 -v 2>&1 | tee /tmp/acc-0008-T3.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0008-T3.out \
  && go test ./internal/httpapi/... ./internal/agent/... ./internal/router/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestReleaseRequeuesWithoutSpendingEitherBudget` | `internal/httpapi/release_test.go` | After a release the job is queued with `attempts` AND `reclaims` both unchanged — a cooperative handover is evidence of nothing wrong | — | S2, S3 |
| `TestReleaseWithoutTheLeaseIsRefused` | `internal/httpapi/release_test.go` | A SECOND worker cannot release a job it does not hold, and the holder still can — so the guard refuses the right one rather than everything | — | S2 |
| `TestReleasedJobIsImmediatelyClaimable` | `internal/httpapi/release_test.go` | Another worker can claim it at once, which is the whole user-visible point: seconds instead of a full lease | — | S2 |
| `TestLateResultAfterReleaseIsRefused` | `internal/httpapi/release_test.go` | The released worker's subprocess finishing later cannot complete the job — `holdsLease` refuses it, which is why the release clears `worker_id` | — | S2 |
| `TestWorkerReleasesOnShutdown` | `internal/agent/release_test.go` | Cancelling the agent's context releases the job it holds, once per held job | — | S4 |
| `TestReleaseFailureIsNotFatal` | `internal/agent/release_test.go` | A router that 404s or hangs on release does not stop the worker exiting — the safety net is the lease, and shutdown must not block on a best-effort call | — | S4, S5 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestReleaseRequeuesWithoutSpendingEitherBudget` |
| 2 — something selects it | The route registration in `internal/httpapi/api.go`, and the agent's shutdown path. Removing the route makes the handler unreachable and the worker's release a 404 — and `TestEveryRouteIsMounted` already exists to catch exactly that, so the new route joins its list |
| 3 — the caller can discover it | `POST /release` is worker-facing and documented in the worker README; `TestUnauthenticatedIsRejected` walks the real route table, so the new route inherits that guard automatically |
| 4 — it is used | Nothing measures this yet. A released-jobs counter is the ADR's follow-up, deliberately not added here |

## Mutation Log

## Invariants

- `ReleaseLease` uses the same `holdsLease` guard as `Complete` and `Fail`. Three
  places, one rule.
- A release clears `worker_id` and the lease, so a late result from the releasing
  worker is refused rather than landing on a job somebody else now holds.
- A release spends NEITHER budget. It is the operator's action, not the job's.
- Release is best effort and never blocks or fails shutdown.
- Lease expiry remains the safety net for a worker that dies without warning.

## Risks

- A third site for the lease guard is a third place to forget it.
  `TestReleaseWithoutTheLeaseIsRefused` asserts both directions — refused for the
  non-holder, allowed for the holder — so a guard that refuses everything fails
  too.
- A worker that releases and then keeps running its subprocess could report a
  late result. Already covered by `holdsLease`, and asserted by
  `TestLateResultAfterReleaseIsRefused` rather than assumed.
- Shutdown could hang on an unreachable router. S4 bounds it; `TestReleaseFailureIsNotFatal`
  is what proves the bound rather than describing it.

## Stop Condition

Stop if the agent turns out not to know which jobs it holds — the release needs a
job id per in-flight slot, and if that is not tracked anywhere the change is
larger than this task and the owner should see the shape first.

## Out of Scope

- Draining: finishing in-flight work before exiting rather than releasing it (deferred: `docs/adr/BACKLOG.md`).
- Reclaiming on SSE disconnect (permanent: boundary: a stream can drop while the subprocess runs, and requeueing then runs the job twice).
- A budget for releases (permanent: boundary: a release loop needs a human restarting workers repeatedly, and `jobs.expires_at` already bounds the job's lifetime).

## Verification Log
