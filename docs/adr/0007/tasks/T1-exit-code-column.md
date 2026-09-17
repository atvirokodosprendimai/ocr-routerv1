# Task ADR-0007-T1: Store a failed job's exit code, nullable so "no exit" stays distinct from 0

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** migration `00004`, `core.Job.ExitCode *int`, `Repo` read/write of `jobs.exit_code`
**Consumes:** none
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the column being nullable`, `the code surviving a write-read round trip`

## Goal

`jobs` carries a nullable `exit_code`, and a job created or failed with one reads
it back.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/store/migrations/00004_exit_code.sql` | add | The column, with a `-- +goose Down` so Rollback step 1 is real |
| `internal/core/job.go` | edit | `Job.ExitCode *int` — a POINTER, because a timeout has no exit code and 0 means success |
| `internal/store/repo.go` | edit | `jobColumns` and `scanJob` carry it, scanning through `sql.NullInt64` |
| `internal/store/repo_write.go` | edit | `FailJobDead` and `RequeueJob` persist it |
| `internal/store/exitcode_test.go` | add | The failing tests below |

## Ordered Steps

1. [S1] Write the failing tests: `TestExitCodeRoundTripsThroughTheJobRow` and `TestNoExitCodeStaysNull`. Confirm red. [proof: acceptance]
2. [S2] Write `00004_exit_code.sql`: `ALTER TABLE jobs ADD COLUMN exit_code INTEGER NULL`, with a
   Down that drops it. NULL is the default and is load-bearing — see the invariant.
3. [S3] Add `core.Job.ExitCode *int` and scan it through `sql.NullInt64`, so an absent code is
   `nil` rather than 0.
4. [S4] Widen `RequeueJob` and `FailJobDead` to persist it. Both are on the failure path and a code
   stored by only one of them would appear and vanish as a job retried. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/store -run 'TestExitCodeRoundTripsThroughTheJobRow|TestNoExitCodeStaysNull|TestExitCodeSurvivesARequeue' -count=1 -v 2>&1 | tee /tmp/acc-0007-T1.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0007-T1.out \
  && go test ./internal/store/... ./internal/router/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestExitCodeRoundTripsThroughTheJobRow` | `internal/store/exitcode_test.go` | A job failed with code 3 reads back `*ExitCode == 3`; a job failed with 0 reads back 0 and NOT nil — the two are different answers | — | S2, S3, S4 |
| `TestNoExitCodeStaysNull` | `internal/store/exitcode_test.go` | A job failed WITHOUT a code (a timeout) reads back `nil`, not 0 — so "exited cleanly" and "never exited" can be told apart by every later reader | — | S2, S3 |
| `TestExitCodeSurvivesARequeue` | `internal/store/exitcode_test.go` | `RequeueJob` persists it too, so a code does not appear on the dead row and vanish on the retried one | — | S4 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestExitCodeRoundTripsThroughTheJobRow` |
| 2 — something selects it | `FailJobDead` and `RequeueJob` are the only writers; dropping the parameter from either makes `TestExitCodeSurvivesARequeue` red — the mutation to record |
| 3 — the caller can discover it | The migration is the declared interface at this stage; no wire surface yet — T3 adds it |
| 4 — it is used | Nothing measures this yet. T4 is what shows it to a human |

## Mutation Log

- 2026-09-17 · 404cdc4* · mutant killed · exit 1 · `internal/store/repo_write.go` · a failure with no exit status is stored as 0 — the code for SUCCESS — so "show me the clean exits" silently includes every job that never exited · acceptance-sha256:a91b35c6df983e4c0375d84f31921afe2087c1a01dae35eeecd9fbad1693500d · covers:the column being nullable
- 2026-09-17 · 404cdc4* · mutant killed · exit 1 · `internal/store/repo_write.go` · RequeueJob drops the code, so it appears on a dead row and vanishes the moment the job retries · acceptance-sha256:a91b35c6df983e4c0375d84f31921afe2087c1a01dae35eeecd9fbad1693500d · covers:the code surviving a write-read round trip

## Invariants

- `exit_code` is NULL when no exit happened, NEVER 0. A timeout, an output-limit
  trip and a contract violation all reach the failure path without an exit
  status, and 0 is the code for SUCCESS — storing it would make "show me the
  clean exits" silently include every job that never exited at all.
- `last_error` keeps its current meaning and content. This task adds a sibling.
- Both failure writers persist the code, or it appears and vanishes across a retry.

## Risks

- `sql.NullInt64` scanning is easy to collapse into a plain `int`, which
  reintroduces the 0/NULL confusion invisibly. `TestNoExitCodeStaysNull` is the
  test that catches exactly that collapse.
- The writer sweep must find both: `git grep -n "FailJobDead(\|RequeueJob(" -- '*.go'`.

## Stop Condition

Stop if `jobs` cannot take a nullable column on this SQLite version without a
table rebuild — the ADR's Rollback promises a reversible `ADD COLUMN`, and if that
cannot be delivered the Rollback section is wrong and must be amended first.

## Out of Scope

- Anything that PRODUCES a code — T2 and T3.
- Showing it — T4.

## Verification Log
- 2026-09-17 · 404cdc4* · exit 1 · `set -o pipefail …` · acceptance-sha256:a91b35c6df983e4c0375d84f31921afe2087c1a01dae35eeecd9fbad1693500d · ms:280
  ```
  --- last 10 line(s) of stdout (of 22 after folding 22 raw)
  	have (context.Context, string, string, nil, "time".Time)
  	want (context.Context, string, string, "time".Time)
  internal/store/exitcode_test.go:59:9: got.ExitCode undefined (type core.Job has no field or method ExitCode)
  internal/store/exitcode_test.go:61:44: got.ExitCode undefined (type core.Job has no field or method ExitCode)
  internal/store/exitcode_test.go:73:73: too many arguments in call to r.RequeueJob
  	have (context.Context, string, string, *int, "time".Time)
  	want (context.Context, string, string, "time".Time)
  internal/store/exitcode_test.go:73:73: too many errors
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/store [build failed]
  FAIL
  ```
- 2026-09-17 · 404cdc4* · exit 0 · `set -o pipefail …` · acceptance-sha256:a91b35c6df983e4c0375d84f31921afe2087c1a01dae35eeecd9fbad1693500d · ms:2705
- 2026-09-17 · 404cdc4* · exit 0 · `set -o pipefail …` · acceptance-sha256:a91b35c6df983e4c0375d84f31921afe2087c1a01dae35eeecd9fbad1693500d · ms:2961
