# Task ADR-0008-T1: Count abandonment separately from failure

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** migration `00005`, `core.Job.Reclaims`, `Repo.ReclaimJob(ctx, id, reason, now)`
**Consumes:** none
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `a reclaim leaving attempts untouched`, `a failure leaving reclaims untouched`, `the count surviving a router restart`

## Goal

`jobs` carries a `reclaims` count, and the write that increments it leaves
`attempts` alone.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/store/migrations/00005_reclaims.sql` | add | `ALTER TABLE jobs ADD COLUMN reclaims INTEGER NOT NULL DEFAULT 0`, with a Down |
| `internal/core/job.go` | edit | `Job.Reclaims int` |
| `internal/store/repo.go` | edit | `jobColumns` and `scanJob` carry it |
| `internal/store/repo_write.go` | edit | `ReclaimJob`, beside `RequeueJob` so the pair stays visibly parallel |
| `internal/store/reclaim_test.go` | add | The failing tests below |

## Ordered Steps

1. [S1] Write the failing tests: `TestReclaimIncrementsReclaimsNotAttempts` and `TestRequeueIncrementsAttemptsNotReclaims`. Confirm red. [proof: acceptance]
2. [S2] Write `00005_reclaims.sql`. Default 0, so every existing row reads as never abandoned —
   which is what an existing deployment's history actually means.
3. [S3] Add `core.Job.Reclaims` and scan it. [proof: acceptance]
4. [S4] Add `Repo.ReclaimJob`: requeue the job — state, `worker_id` cleared, lease cleared, reason
   recorded — and increment `reclaims`. ⚠ Write it DIRECTLY BESIDE `RequeueJob`: the two differ in
   exactly one column, and separating them is how one later gains a field the other forgets.

## Acceptance

```bash
set -o pipefail
go test ./internal/store -run 'TestReclaimIncrementsReclaimsNotAttempts|TestRequeueIncrementsAttemptsNotReclaims|TestReclaimClearsTheLease' -count=1 -v 2>&1 | tee /tmp/acc-0008-T1.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0008-T1.out \
  && go test ./internal/store/... ./internal/router/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestReclaimIncrementsReclaimsNotAttempts` | `internal/store/reclaim_test.go` | After `ReclaimJob`, `reclaims` is 1 and `attempts` is UNCHANGED — the whole decision, asserted on both columns because moving only one is the defect | — | S2, S4 |
| `TestRequeueIncrementsAttemptsNotReclaims` | `internal/store/reclaim_test.go` | The mirror: after `RequeueJob`, `attempts` moves and `reclaims` does not. Without this pair, "the counters are separate" is asserted in one direction only | — | S4 |
| `TestReclaimClearsTheLease` | `internal/store/reclaim_test.go` | A reclaimed job has no `worker_id` and no `lease_expires_at`, so the old holder's late completion is refused by `holdsLease` rather than landing on a job another worker now owns | — | S4 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestReclaimIncrementsReclaimsNotAttempts` |
| 2 — something selects it | Nothing calls `ReclaimJob` yet — T2 is what selects it, and says so. This task deliberately ships a writer with no caller, which is the one shape this corpus otherwise treats as a defect; it is bounded by T2 landing next |
| 3 — the caller can discover it | The migration and the exported method are the interface at this stage; no wire surface |
| 4 — it is used | Nothing measures this yet; the ADR's follow-up holds the metric question |

## Mutation Log

- 2026-09-17 · 453b802* · mutant killed · exit 1 · `internal/store/repo_write.go` · a reclaim also spends an attempt, so three router restarts kill a job whose command never failed once — the exact defect ADR-0008 exists to remove · acceptance-sha256:cb94cf6b1c03dc86b878991f7e05f1779e51a1955586e15f5328509726558304 · covers:a reclaim leaving attempts untouched
- 2026-09-17 · 453b802* · mutant killed · exit 1 · `internal/store/repo_write.go` · a real command failure is also counted as an abandonment, so neither number means anything and the reclaim budget T2 spends is consumed by failures · acceptance-sha256:cb94cf6b1c03dc86b878991f7e05f1779e51a1955586e15f5328509726558304 · covers:a failure leaving reclaims untouched
- 2026-09-17 · 453b802* · mutant killed · exit 1 · `internal/store/repo.go` · the column is written but never read back, so the count is invisible to every later query — which is what "it did not survive" looks like from the outside · acceptance-sha256:cb94cf6b1c03dc86b878991f7e05f1779e51a1955586e15f5328509726558304 · covers:the count surviving a router restart

## Invariants

- `ReclaimJob` never touches `attempts`; `RequeueJob` never touches `reclaims`.
- A reclaim clears `worker_id` and the lease, or a late result from the previous
  holder could be accepted for a job somebody else now holds.
- Existing rows default to 0 reclaims — an existing deployment's jobs were never
  abandoned under a counter that did not exist.

## Risks

- The two writers differ by one column and will be read as duplication by someone
  later. They are kept adjacent and the paired tests assert both directions, so
  merging them into one function with a flag breaks a named test rather than
  quietly changing which budget is spent.

## Stop Condition

Stop if `RequeueJob` turns out to be called from somewhere that means
abandonment rather than failure — that would mean the two causes are already
conflated at a site this task does not know about, and the sweep
(`git grep -n "RequeueJob(" -- '*.go'`) belongs in the record before either is
changed.

## Out of Scope

- Deciding WHEN to reclaim — T2.
- The release route — T3.

## Verification Log
- 2026-09-17 · 453b802* · exit 1 · `set -o pipefail …` · acceptance-sha256:cb94cf6b1c03dc86b878991f7e05f1779e51a1955586e15f5328509726558304 · ms:288
  ```
  --- last 10 line(s) of stdout (of 11 after folding 11 raw)
  internal/store/reclaim_test.go:38:14: r.ReclaimJob undefined (type *"github.com/atvirokodosprendimai/ocr-router/internal/store".Repo has no field or method ReclaimJob)
  internal/store/reclaim_test.go:45:9: got.Reclaims undefined (type core.Job has no field or method Reclaims)
  internal/store/reclaim_test.go:45:28: before.Reclaims undefined (type core.Job has no field or method Reclaims)
  internal/store/reclaim_test.go:47:8: got.Reclaims undefined (type core.Job has no field or method Reclaims)
  internal/store/reclaim_test.go:47:25: before.Reclaims undefined (type core.Job has no field or method Reclaims)
  internal/store/reclaim_test.go:77:9: got.Reclaims undefined (type core.Job has no field or method Reclaims)
  internal/store/reclaim_test.go:79:66: got.Reclaims undefined (type core.Job has no field or method Reclaims)
  internal/store/reclaim_test.go:92:14: r.ReclaimJob undefined (type *"github.com/atvirokodosprendimai/ocr-router/internal/store".Repo has no field or method ReclaimJob)
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/store [build failed]
  FAIL
  ```
- 2026-09-17 · 453b802* · exit 0 · `set -o pipefail …` · acceptance-sha256:cb94cf6b1c03dc86b878991f7e05f1779e51a1955586e15f5328509726558304 · ms:3873
- 2026-09-17 · 453b802* · exit 0 · `set -o pipefail …` · acceptance-sha256:cb94cf6b1c03dc86b878991f7e05f1779e51a1955586e15f5328509726558304 · ms:2802
- 2026-09-17 · 453b802* · exit 0 · `set -o pipefail …` · acceptance-sha256:cb94cf6b1c03dc86b878991f7e05f1779e51a1955586e15f5328509726558304 · ms:3302
