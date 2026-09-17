# Task ADR-0008-T2: Make the reaper spend the abandonment budget, not the failure one

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `Service.reclaimJob` bounded by `Config.MaxReclaims`, `--max-reclaims`
**Consumes:** `jobs.reclaims` + `Repo.ReclaimJob` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the reaper spending reclaims rather than attempts`, `the reclaim budget still being bounded`, `the terminal reason naming which budget ran out`

## Goal

A lease taken back because a worker vanished costs the job a reclaim, not an
attempt — and a job that keeps killing workers still dies.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/router/service.go` | edit | `reclaimJob` beside `failJob`; `Config.MaxReclaims` |
| `internal/router/reaper.go` | edit | The expired-lease loop calls `reclaimJob` — the line this whole record is about |
| `cmd/router/main.go` | edit | `--max-reclaims`, default 3 — the flag that SELECTS the bound |
| `cmd/router/wire.go` | edit | Into `router.Config` |
| `internal/router/reclaim_test.go` | add | The failing tests below |

## Ordered Steps

1. [S1] Write the failing tests: `TestReclaimDoesNotSpendAnAttempt`, `TestReclaimBudgetIsStillBounded`, `TestFailureStillSpendsAnAttempt`. Confirm red. [proof: acceptance]
2. [S2] Add `Config.MaxReclaims` and the `--max-reclaims` flag, default 3. [proof: acceptance]
3. [S3] Add `Service.reclaimJob`: if `job.Reclaims+1 < MaxReclaims` call `Repo.ReclaimJob`,
   otherwise mark the job dead with a reason NAMING abandonment. ⚠ The terminal reason must say
   which budget ran out: "abandoned too many times" and "attempts exhausted" are different facts and
   an operator reading one must not think it is the other.
4. [S4] Point the reaper's expired-lease loop at it, leaving every other `failJob` caller alone.
5. [S5] Keep the existing counter and log line, now reporting a reclaim rather than a failure, so
   the ADR-0002 line says what actually happened. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/router -run 'TestReclaimDoesNotSpendAnAttempt|TestReclaimBudgetIsStillBounded|TestFailureStillSpendsAnAttempt|TestThreeRestartsDoNotKillAJob|TestReclaimLogSaysReclaimed' -count=1 -v 2>&1 | tee /tmp/acc-0008-T2.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0008-T2.out \
  && go test ./internal/router/... ./internal/httpapi/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestReclaimDoesNotSpendAnAttempt` | `internal/router/reclaim_test.go` | A job whose lease expires comes back queued with `attempts` unchanged and `reclaims` at 1 | — | S3, S4 |
| `TestThreeRestartsDoNotKillAJob` | `internal/router/reclaim_test.go` | M's actual case: claim/abandon three times over, and the job is STILL alive and runnable. Today the third kills it | — | S3, S4 |
| `TestReclaimBudgetIsStillBounded` | `internal/router/reclaim_test.go` | At `--max-reclaims` the job dies, with a reason naming ABANDONMENT — the poison-pill bound, which removing the counter entirely would lose | — | S3 |
| `TestFailureStillSpendsAnAttempt` | `internal/router/reclaim_test.go` | A worker REPORTING a failure still spends an attempt and still dies at `--max-attempts`. This task must not make genuine failures free | — | S4 |
| `TestReclaimLogSaysReclaimed` | `internal/router/logger_test.go` | The transition line for an expired lease reports a reclaim, not a failure, so the log matches the counter | — | S5 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestReclaimDoesNotSpendAnAttempt` |
| 2 — something selects it | The reaper's expired-lease loop is the ONLY caller of `reclaimJob`; pointing it back at `failJob` restores today's behaviour and turns `TestReclaimDoesNotSpendAnAttempt` red — the mutation to record, and the one line the whole record turns on |
| 3 — the caller can discover it | `--max-reclaims`'s `Usage` string. An operator who cannot find the bound cannot raise it when a flaky host makes workers vanish |
| 4 — it is used | Nothing measures this yet; the ADR's follow-up holds the metric question deliberately |

## Mutation Log

- 2026-09-17 · b3755a7* · mutant killed · exit 1 · `internal/router/reaper.go` · the expired-lease loop points back at failJob, which is exactly the behaviour before this record: a router restart spends the retry budget and three of them kill a healthy job · acceptance-sha256:d0066f862827e79bd46a74f64d7f7fe607073d03bdabc663ffa791cbf892068c · covers:the reaper spending reclaims rather than attempts
- 2026-09-17 · b3755a7* · mutant killed · exit 1 · `internal/router/service.go` · the reclaim becomes unbounded, so a poison-pill job that takes down every worker it touches is requeued forever and never reaches a terminal state — worse than the defect being fixed · acceptance-sha256:d0066f862827e79bd46a74f64d7f7fe607073d03bdabc663ffa791cbf892068c · covers:the reclaim budget still being bounded
- 2026-09-17 · b3755a7* · mutant killed · exit 1 · `internal/router/service.go` · a job killed by flaky infrastructure records the failure budget as its cause, sending an operator to look for a broken command when no command ever failed · acceptance-sha256:d0066f862827e79bd46a74f64d7f7fe607073d03bdabc663ffa791cbf892068c · covers:the terminal reason naming which budget ran out
- 2026-09-17 · b3755a7* · mutant killed · exit 1 · `internal/router/reaper.go` · the expired-lease loop points back at failJob — the behaviour before this record, where a router restart spends the retry budget · acceptance-sha256:f249139b182d39c32b120fe1ee19c25fdaf0e3ed7ee1a9d7d3d2374ab5b73fc9 · covers:the reaper spending reclaims rather than attempts
- 2026-09-17 · b3755a7* · mutant killed · exit 1 · `internal/router/service.go` · the reclaim becomes unbounded, so a poison-pill job is requeued forever and never reaches a terminal state · acceptance-sha256:f249139b182d39c32b120fe1ee19c25fdaf0e3ed7ee1a9d7d3d2374ab5b73fc9 · covers:the reclaim budget still being bounded
- 2026-09-17 · b3755a7* · mutant killed · exit 1 · `internal/router/service.go` · a job killed by flaky infrastructure records the failure budget as its cause · acceptance-sha256:f249139b182d39c32b120fe1ee19c25fdaf0e3ed7ee1a9d7d3d2374ab5b73fc9 · covers:the terminal reason naming which budget ran out

## Execution Note — the default is 10, not 3 (2026-09-17)

S2 says "`--max-reclaims`, default 3", and the Tests table names
`TestThreeRestartsDoNotKillAJob`. **Those two cannot both hold.** With
`Reclaims+1 < MaxReclaims` (S3's own condition) a bound of 3 makes the THIRD
abandonment terminal, so three restarts kill the job — which is the incident
verbatim, reported by M on 2026-09-16.

Shipped at **10**, with the reason in a comment at the flag. The bound's purpose,
stated in this task's own Invariants, is to stop a poison pill looping forever;
10 does that, and no number between 4 and 10 is better justified. `MaxAttempts`
is untouched at 3.

If the owner wants the two budgets equal, the fix is to change this number and
retire `TestThreeRestartsDoNotKillAJob` — not to keep both and let the fence
decide which one was meant.

## Invariants

- The reaper is the only caller of `reclaimJob`. Every other requeue is a failure
  and keeps spending `attempts`.
- The reclaim budget is BOUNDED. Unbounded requeue turns a poison-pill job into a
  loop that takes down every worker in turn, which is worse than the defect being
  fixed because it has no terminal state.
- A terminal reason names which budget ran out.
- `--max-attempts` and its behaviour are untouched.

## Risks

- The natural simplification is one counter with a flag, which re-merges exactly
  what this record separates. `TestFailureStillSpendsAnAttempt` and
  `TestReclaimDoesNotSpendAnAttempt` fail in opposite directions, so a merged
  implementation cannot pass both.
- A job can now die for two reasons that both read as "requeued too often". The
  ADR names this as a cost; S3's reason text is the mitigation, and it is prose.

## Stop Condition

Stop if the reaper turns out to reclaim leases for a reason OTHER than a silent
worker — a shutdown path, a rebalance — because then "abandonment" is not one
thing and the budget is being asked to cover two.

## Out of Scope

- The cooperative release path — T3.
- Showing `reclaims` anywhere (deferred: `docs/adr/0007/0007-failure-detail.md`).

## Verification Log
- 2026-09-17 · b3755a7* · exit 1 · `set -o pipefail …` · acceptance-sha256:d0066f862827e79bd46a74f64d7f7fe607073d03bdabc663ffa791cbf892068c · ms:506
  ```
  --- last 5 line(s) of stdout
  # github.com/atvirokodosprendimai/ocr-router/internal/router_test [github.com/atvirokodosprendimai/ocr-router/internal/router.test]
  internal/router/reclaim_test.go:82:43: unknown field MaxReclaims in struct literal of type router.Config
  internal/router/service_test.go:36:3: unknown field MaxReclaims in struct literal of type router.Config
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/router [build failed]
  FAIL
  ```
- 2026-09-17 · b3755a7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d0066f862827e79bd46a74f64d7f7fe607073d03bdabc663ffa791cbf892068c · ms:6613
- 2026-09-17 · b3755a7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d0066f862827e79bd46a74f64d7f7fe607073d03bdabc663ffa791cbf892068c · ms:5402
- 2026-09-17 · b3755a7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d0066f862827e79bd46a74f64d7f7fe607073d03bdabc663ffa791cbf892068c · ms:5383
- 2026-09-17 · b3755a7* · exit 0 · `set -o pipefail …` · acceptance-sha256:f249139b182d39c32b120fe1ee19c25fdaf0e3ed7ee1a9d7d3d2374ab5b73fc9 · ms:6815
- 2026-09-17 · b3755a7* · exit 0 · `set -o pipefail …` · acceptance-sha256:f249139b182d39c32b120fe1ee19c25fdaf0e3ed7ee1a9d7d3d2374ab5b73fc9 · ms:6001
- 2026-09-17 · b3755a7* · exit 0 · `set -o pipefail …` · acceptance-sha256:f249139b182d39c32b120fe1ee19c25fdaf0e3ed7ee1a9d7d3d2374ab5b73fc9 · ms:5375
