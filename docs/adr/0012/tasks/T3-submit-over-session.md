# Task ADR-0012-T3: Re-implement `Submit` over `Session`, with the existing tests unedited

**Depends-on:** T2
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `client.Submit` re-implemented as a composition over `Session`, and the deletion of the duplicate `openStream` / `upload` / `collect`
**Consumes:** `client.Session` + `client.Open()` (T1), `(*Session).Upload()` + `(*Session).Collect()` (T2)
**Data dependency:** hermetic — the existing 776 lines of test already carry every stub this needs
**Proof map:** v1
**Rests-on:** `the 776 pre-existing lines passing byte-unchanged`, `client/client_test.go and client/raw_test.go being unedited`, `one protocol implementation remaining after the duplicates are deleted`, `cmd/client behaving identically`, `the progress stages firing identically on BOTH the event path and the backlog path`, `hello-before-upload surviving, which only T1's new test can show`

## Goal

Make `Submit` a thin composition over `Session` so this repository holds exactly ONE implementation
of the protocol, and prove the refactor changed nothing by running the 776 lines of pre-existing test
without editing a character of them.

This is the task the whole record exists to make safe. Everything before it added code that nothing
called; this one rewrites a function that `cmd/client` and any external consumer already depend on.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `client/client.go` | edit | `Submit`'s body becomes `Open` → `Upload` → select over `Events()` → `Collect`, and the now-unreachable `openStream`, `upload` and `collect` are DELETED. **This file is what SELECTS `Session` in production** — after this task, deleting `Submit`'s `Open` call is what makes the handle unreachable from any in-repo caller, and `TestStreamOpensBeforeUpload` is what goes red. |
| `client/session.go` | edit | Only if a helper `Submit` needs is still private to the file — e.g. hoisting the job-id match into a small unexported func both entry points share. Prefer no edit; a hunk here must be justified in the commit message. |

⚠ **`client/client_test.go` and `client/raw_test.go` are NOT in this table and may not be edited.**
They are the entire proof of this task. Their absence from this table is the claim, and the fence
asserts it mechanically rather than trusting review to notice.

## Ordered Steps

1. [S1] Establish the red that matters BEFORE touching `Submit`: write
   `TestSubmitAndSessionShareOneImplementation` in a new `client/oneimpl_test.go`, asserting that
   `client/client.go` no longer declares `func openStream`, `func upload` or `func collect`. It is red
   now (all three exist) and green only once the duplicates are gone. ⚠ This is a source-level
   assertion because the property is STRUCTURAL — "there is one implementation" is not observable from
   behaviour, and a behavioural test would pass with both copies present, which is the whole failure
   mode this task is preventing.
2. [S2] [proof: human: the author reads the captured surface and confirms it describes the pre-refactor package] Capture the exported surface before the rewrite: `go doc -all ./client > /tmp/surface-0012-before.txt`. It must be taken first; it is what S6 diffs against.
3. [S3] Rewrite `Submit`'s body: `Open` (which waits for `hello` AND the first `backlog`, T1), then
   `Upload`, then check `Backlog()` for a result that was already waiting, then range over `Events()`
   matching on job id, then `Collect`. `defer s.Close()`.
   ★ **Re-emit the progress stages `Collect` no longer reports.** T2's `Collect(ctx, jobID)` takes no
   `Progress`, while the `collect` being deleted reported `StageCollecting` and `StageDone` internally
   (`client/client.go:348`, `:379`, `:390`). So `Submit` must report them at BOTH call sites — the
   event path and the backlog path — in the same order as today: `StageUploading`, `StageWaiting`,
   `StageCollecting`, `StageDone`. `cmd/client/progress.go` renders them and ADR-0005 owns that UX.
   ⚠ `TestProgressCallbackSequence` (`client/client_test.go:553`) exercises only the EVENT path, so a
   divergence on the backlog path passes the fence — this step is the only thing standing between that
   and a silent regression. [proof: acceptance]
4. [S4] Delete `openStream`, `upload` and `collect` from `client/client.go`, plus any now-unused
   import. `go vet` and the unused-import compile error are what prove nothing still references them.
   [proof: acceptance]
5. [S5] [proof: acceptance] Run the 776 pre-existing lines plus `cmd/client`'s 590 under `-race`,
   UNEDITED, and confirm every one passes. This is most of the task's verdict — but it is THREE of the
   four properties ADR-0011 §Consequences names, not four: backlog replay, job-id match and typed
   errors are asserted by tests written against the OLD implementation, so their passing is evidence
   about the NEW one. ⚠ **SSE-before-upload is NOT among them.** `TestStreamOpensBeforeUpload` records
   the request at handler entry (`client/client_test.go:60`) BEFORE the hello delay (`:65-68`), so it
   proves dial-before-upload and would stay green if the `hello` wait were lost. T1's
   `TestOpenReturnsOnlyAfterHelloAndBacklog` is what covers that, and it runs in this fence's
   unfiltered leg too.
6. [S6] [proof: human: the author reads the diff and judges whether it is empty for the right reason — the package built and `go doc` produced a surface — rather than because a command failed and printed nothing] Confirm the exported surface is unchanged by the refactor: `go doc -all ./client > /tmp/surface-0012-after.txt`, then `diff /tmp/surface-0012-before.txt /tmp/surface-0012-after.txt` must print NOTHING. T1, T2 and T4 add symbols; T3 adds none, so any output means this task exceeded its scope.
7. [S7] [proof: mutation] Invert the ordering inside the new `Submit` — call `Upload` before `Open` —
   and confirm `TestStreamOpensBeforeUpload` goes red. This is the one mutation that proves the
   refactor did not quietly lose the property the whole package is built around, and it must be run
   against the NEW body, not the old one.

## Acceptance

```bash
set -o pipefail
git diff --exit-code -- client/client_test.go client/raw_test.go \
  && go test ./client/... -count=1 -race -v -run 'TestSubmitAndSessionShareOneImplementation' \
       2>&1 | tee /tmp/acc-0012-T3a.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/acc-0012-T3a.out \
  && templ generate && go build ./... && go vet ./... && test -z "$(gofmt -l .)" \
  && go test ./client/... ./cmd/client/... -count=1 -race 2>&1 | tee /tmp/acc-0012-T3b.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0012-T3b.out
```

<`git diff --exit-code` over the two test files is the FIRST segment on purpose: it is the one claim
this task cannot make by testing, and putting it first means an edited safety net fails the gate before
a single test runs. ⚠ It catches an edit to a TRACKED file and would NOT catch one staged and
committed — which is why the unedited-tests Invariant also says it, and why the reviewer reads the
commit's diffstat. The regression leg here is the VERDICT rather than a guard, which is the reverse of
T1 and T2, so it is deliberately unfiltered and covers `cmd/client` too.>

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestSubmitAndSessionShareOneImplementation` | `client/oneimpl_test.go` | `client/client.go` declares none of `func openStream`, `func upload`, `func collect` — the structural claim that the duplicate protocol is gone rather than merely unused. Red before S4, and red again if anyone reintroduces a private copy. | — | S1, S4 |

⚠ **The 776 pre-existing lines are this task's real evidence and are still not listed here**, for the
same reason as T1 and T2: a Tests row promises a test the FILTERED leg selects, and these run in the
unfiltered leg. The difference is that here they are the VERDICT rather than a regression guard — so
the fence runs them unscoped and unfiltered, and S5 says what their passing means. Listing them above
would put thirteen already-green suites beside the one red one.

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `Session` exists from T1; this task does not add a mechanism, it removes a duplicate one. |
| 2 — something selects it | ★ **This is the task that closes rung 2 for the whole record.** `Submit` calls `Open`, `Upload` and `Collect`, and `cmd/client/main.go` calls `Submit`. The mutation that proves the chain: S7's inverted ordering inside the new body — it can only go red if `Submit` genuinely runs through `Session`. |
| 3 — the caller can discover it | Not closed here. T4's `Example` and README section are what make the Session shape discoverable; `Submit`'s own discoverability is unchanged and already covered by ADR-0011-T2. |
| 4 — it is used | `Submit` is used by `cmd/client` and its 590-line test. `Session` directly: nothing in this repository, by design — the consumer is in another one (ADR-0012 §Consequences). |

## Mutation Log

<Tool-written by `adr-verify --mutant` at execution time. Empty at authoring.>

## Invariants

- **`client/client_test.go` and `client/raw_test.go` are byte-identical to their state at `8e6f81a`.**
  Not "morally unchanged" — byte-identical, asserted by `git diff --exit-code` in the fence.
- **`Submit`'s signature is unchanged**, and so is every observable behaviour it has: the stage
  callbacks fire in the same order with the same details, the same error types come back for the same
  conditions, and the same `Result` shape is returned.
- **The exported surface gains and loses nothing in this task.** `go doc -all` before and after are
  identical (S6).
- **Exactly one implementation remains.** `openStream`, `upload` and `collect` are gone from
  `client/client.go`, not merely unreferenced.
- **`cmd/client` is not edited**, and its behaviour is identical — same flags, same exit codes, same
  stdout/stderr split.
- **`Submit` still establishes the stream before uploading.** The property survives as a consequence of
  calling `Open` first, and S7 is what proves it rather than asserting it.
- `Submit` closes its session on every exit path, including the error ones, so the one-shot call leaks
  nothing.

## Risks

- **The refactor changes behaviour in a way the existing tests do not cover, and a consumer finds it.**
  The honest bound: those 776 lines cover THREE of the four properties ADR-0011 names — and ADR-0011's
  own claim that all four are covered is narrower than it reads, because `TestStreamOpensBeforeUpload`
  asserts dial-before-upload rather than hello-before-upload (S5). The fourth is covered by T1's new
  test, not by this task's regression net. What remains genuinely uncovered is the backlog replay's
  mutation gap, named in ADR-0011 §Consequences and unchanged by this record.
- **The progress stages diverge on the backlog path**, because `Collect` no longer emits them and only
  the event path is tested (S3). Not closed by any existing test; S3 is a procedural mitigation and
  says so.
- A subtle difference between the deleted `openStream` and T1's `Open` — a timeout, a header, the
  handling of a frame arriving during establishment — passes the tests because no test distinguishes
  them. Mitigated by T1's `Open` being a relocation of `openStream` rather than a rewrite, and by S6
  proving no surface changed; not fully closed, which is why S7 mutates the property that matters most.
- `Submit` now holds a `Session` and must close it. A missed `defer` leaks a goroutine per call and
  `cmd/client` is short-lived enough to hide it — caught by `-race` plus the `Close` idempotence test
  from T1, and bounded by the fact that `Submit`'s context cancellation also stops the pump.
- The structural test in S1 is a source-text assertion, so a rename (`upload` → `doUpload`) satisfies it
  while leaving a duplicate. Accepted: it is a tripwire against the specific regression of
  reintroducing the old trio, not a proof of singularity, and the reviewer reading the diff is what
  covers the rename. Stated rather than overclaimed.

## Stop Condition

⚠ **Stop and ask the moment any of the 776 pre-existing lines needs editing to pass.** That is not a
test to fix — it is the falsifier ADR-0012 §Decision pre-registered. A behaviour-preserving
re-implementation does not require changing a test written against the behaviour it preserves, so an
edit means either the refactor changed behaviour or the record's premise was wrong. Either way it is
M's call, not a local repair.

Stop and ask if S6's surface diff prints anything at all: T3 is defined as adding no symbol.

⚠ **Do NOT stop for the one declared behaviour difference: `Collect` path-escapes the job id where the
deleted `collect` did not** (T2 S4, ADR-0012 §Risks). It is expected, it is correct, and no existing
test distinguishes it because every id in the suite is `job-1`. Every OTHER observable difference is a
stop.

Stop and ask if `Submit` cannot be expressed over `Session` without adding a parameter, an option or an
exported helper — that would mean `Session`'s shape from T1/T2 is wrong, and the fix belongs there.

## Out of Scope

- `Services` and the documentation — T4.
- Any change to `Submit`'s signature or behaviour (permanent: boundary: the entire evidential value of
  this task is that a caller cannot tell the difference, which is what lets the pre-existing tests
  serve as proof; a deliberate behaviour change would need its own record and its own tests).
- Any change to `cmd/client` (permanent: boundary: ADR-0005 owns the CLI, and an edit there inside a
  behaviour-preserving refactor would be unattributable).
- Adding reconnection to `Submit` now that it has a `Session` to hang it off (deferred:
  `docs/adr/BACKLOG.md`).
- Streaming the multipart body (deferred: `docs/adr/BACKLOG.md`).

## Verification Log

<Tool-written by `adr-verify` at execution time. Empty at authoring.>
