# Task ADR-0007-T3: Carry the exit code from the worker to the job row and the log line

**Depends-on:** T1, T2
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `exit_code` on the worker's report, `Router.Fail(…, exitCode, …)`, `exit_code` on the ADR-0002 failure log line
**Consumes:** `jobs.exit_code` + `core.Job.ExitCode` (T1), `runner.Failure` (T2)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the code reaching the job row`, `the code reaching the structured log`, `an absent code staying absent`

## Goal

A command that exits 3 leaves `3` on its job row and on its failure log line.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/agent/agent.go` | edit | `report` sends `exit_code` when the failure has one |
| `internal/httpapi/upload.go` | edit | `workerResult` gains `exit_code`; passed to `Router.Fail` |
| `internal/router/service.go` | edit | `Fail` and `failJob` carry it through to both persistence paths |
| `internal/router/logger.go` | edit | The failure line gains the attribute — ADR-0002 owns this shape, which is why it is in `Invalidates:` |
| `internal/router/reaper.go` | edit | The reaper fails jobs too, and passes NO code: a lease it took back never exited |
| `internal/httpapi/failure_test.go` | add | The end-to-end tests below |

## Ordered Steps

1. [S1] Write the failing tests: `TestExitCodeSurvivesToTheJobRow`, `TestTimeoutLeavesNoExitCode`, `TestFailureLogCarriesTheExitCode`. Confirm red. [proof: acceptance]
2. [S2] Add `exit_code` to `workerResult` as `*int`, and send it from the agent when
   `failure.ExitCode != nil`. Absent on the wire means absent, not zero.
3. [S3] Thread it through `Router.Fail` → `failJob` → BOTH `RequeueJob` and `FailJobDead`, so a
   retried job and a dead one carry it alike.
4. [S4] Add the attribute to the failure transition line, beside `reason`. Additive only: no
   existing attribute changes, which is what keeps ADR-0002's schema intact.
5. [S5] The reaper passes nil. A lease reclaimed from a vanished worker never produced an exit
   status, and recording one would invent a fact. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/httpapi ./internal/router -run 'TestExitCodeSurvivesToTheJobRow|TestTimeoutLeavesNoExitCode|TestFailureLogCarriesTheExitCode|TestReaperRecordsNoExitCode' -count=1 -v 2>&1 | tee /tmp/acc-0007-T3.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0007-T3.out \
  && go test ./internal/httpapi/... ./internal/router/... ./internal/agent/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestExitCodeSurvivesToTheJobRow` | `internal/httpapi/failure_test.go` | A worker reporting `exit_code: 3` leaves `*ExitCode == 3` on the persisted job — the whole point, asserted across every hop rather than at one | — | S2, S3 |
| `TestTimeoutLeavesNoExitCode` | `internal/httpapi/failure_test.go` | A failure reported WITHOUT a code leaves the column NULL, so "exited 0" and "never exited" stay distinguishable at the far end | — | S2, S3 |
| `TestFailureLogCarriesTheExitCode` | `internal/router/exitcodelog_test.go` | The failure line carries `exit_code`, and a SUCCESS line carries none — without the negative half, an implementation that puts a code on every line passes, and every delivered job then looks like it exited 0 | — | S4 |
| `TestReaperRecordsNoExitCode` | `internal/router/exitcodelog_test.go` | A lease reclaimed by the reaper leaves the column NULL — the worker vanished, it did not exit | — | S5 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestExitCodeSurvivesToTheJobRow` |
| 2 — something selects it | `resultFromWorker` is the only place the wire field is read; dropping it there makes `TestExitCodeSurvivesToTheJobRow` red — the mutation to record, and the shape where a field is sent, stored and never read |
| 3 — the caller can discover it | `exit_code` on the worker report is the declared interface; `TestTimeoutLeavesNoExitCode` pins what omitting it means |
| 4 — it is used | The failure log line (S4). T4 adds the human's view; no metric dimension, deliberately — the ADR's follow-up says why |

## Mutation Log

- 2026-09-17 · f801c15* · mutant killed · exit 1 · `internal/httpapi/upload.go` · the wire field is sent, decoded and then dropped — a field that exists, is populated and is read by nothing · acceptance-sha256:ab537c9832ac7a00d7c8ed258421b454108dd853975beb092ad38b270ab5409d · covers:the code reaching the job row
- 2026-09-17 · f801c15* · mutant killed · exit 1 · `internal/router/logger.go` · the failure line loses the attribute, so the code is queryable in the database and invisible in the logs an operator actually greps · acceptance-sha256:ab537c9832ac7a00d7c8ed258421b454108dd853975beb092ad38b270ab5409d · covers:the code reaching the structured log
- 2026-09-17 · f801c15* · mutant killed · exit 1 · `internal/store/repo_write.go` · a timeout is stored as exit 0, so every codeless failure reads as a job that exited cleanly and failed anyway · acceptance-sha256:ab537c9832ac7a00d7c8ed258421b454108dd853975beb092ad38b270ab5409d · covers:an absent code staying absent

## Invariants

- Absent on the wire means NULL in the column. Nothing in this chain converts a
  missing code to 0.
- `last_error` is unchanged in meaning and content.
- ADR-0002's log schema only GAINS an attribute, on failure lines only.
- The reaper never records an exit code.

## Risks

- Three components change together and a `*int` is easy to flatten to `int` at
  any hop, which silently turns every codeless failure into "exited 0".
  `TestTimeoutLeavesNoExitCode` runs the whole chain for exactly that reason.
- `Router.Fail`'s signature change reaches `internal/httpapi` and the reaper —
  swept with `git grep -n "\.Fail(\|failJob(" -- '*.go'`.

## Stop Condition

Stop if ADR-0002's log schema turns out to be consumed by something that rejects
unknown attributes — that would make an additive change breaking, and it is the
owner's call, not this task's to work around.

## Out of Scope

- Showing any of it — T4.
- A metric dimension for the code (deferred: `docs/adr/BACKLOG.md`).

## Verification Log
- 2026-09-17 · f801c15* · exit 0 · `set -o pipefail …` · acceptance-sha256:ab537c9832ac7a00d7c8ed258421b454108dd853975beb092ad38b270ab5409d · ms:17752
- 2026-09-17 · f801c15* · exit 0 · `set -o pipefail …` · acceptance-sha256:ab537c9832ac7a00d7c8ed258421b454108dd853975beb092ad38b270ab5409d · ms:12997
- 2026-09-17 · f801c15* · exit 0 · `set -o pipefail …` · acceptance-sha256:ab537c9832ac7a00d7c8ed258421b454108dd853975beb092ad38b270ab5409d · ms:16785
