# Task ADR-0005-T2: The binary a person runs — flags, progress, exit codes

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (cmd + docs)
**Owner:** unassigned
**Produces:** the `cmd/client` binary, its flags, its progress rendering, its exit codes
**Consumes:** `client.Submit`, `client.Config`, `client.Progress`, `client.FailedError` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the failed-job exit code`, `the stderr split`, `the atomic write`, `the destination probe`

## Goal

`client --router … --token … -i scan.pdf -o out.txt`: block while the job runs, show what it is
doing on stderr, write the result, and exit with a code that says what the caller should do next.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `cmd/client/main.go` | add | flags, `OCRR_TOKEN`, the call into `client.Submit`, **the exit-code mapping** |
| `cmd/client/progress.go` | add | TTY and non-TTY rendering, both on stderr |
| `cmd/client/output.go` | add | the atomic write, `-o -`, `--json` |
| `cmd/client/main_test.go` | add | the failing tests, against an `httptest` router |
| `README.md` | edit | the client's usage, its exit codes, and the buffering-proxy caveat |

`main.go`'s exit-code mapping is the selecting line for T1's classification: without it every
failure would be exit 1, and the whole point of typing them would be lost.

## Ordered Steps

1. [S1] Write the failing test first: running the command against a test router writes the result to
   `-o` and exits 0, before the binary exists (TDD red). [proof: acceptance]
2. [S2] Flags: `--router`, `--token`, `-i`, `-o`, `--label`, `--pipeline`, `--param k=v`
   (repeatable), `--json`, `--quiet`, `--timeout`. `--token` falls back to `OCRR_TOKEN` — a token on
   a command line is in the shell history and in `ps` for every user on the box.
3. [S3] ⚠ **The destination is probed BEFORE the upload.** `GET /files/{id}` charges the credits, so
   discovering an unwritable `-o` afterwards means the customer paid for a result that goes nowhere.
   A directory, a bad path or a permissions failure exits 1 having spent nothing.
4. [S4] `-i` is required only when no `--param` is given: a params-only job is ADR-0001's crawler
   shape, where the service fetches its own input.
5. [S5] ⚠ **Progress goes to stderr, always.** `-o -` writes the result to stdout, and a progress
   line on the same stream corrupts it. On a TTY the line is rewritten in place with `\r`; off a TTY
   it is one plain line per transition, because `\r` in a CI log is noise. `--quiet` suppresses it;
   errors still print.
6. [S6] ⚠ **The output is written atomically**: a temp file in the destination's directory, then
   rename. The result has already been collected and charged by the time it is written, so a partial
   write on a full disk would lose work the customer paid for. `-o -` streams to stdout, where this
   does not apply and cannot.
7. [S7] The exit-code mapping, which is the reason T1 returns typed outcomes:
   **0** wrote the result · **1** the caller must fix something · **2** worth retrying ·
   **3** the job ran and failed. ⚠ A failed job must never exit 0 — that turns a dead job into
   silent data loss in whatever pipeline called it.
8. [S8] On failure, **no output file is created at all.** An empty file plus a non-zero exit invites
   a caller to use the file anyway.
9. [S9] README: usage, the four exit codes and what each means, `OCRR_TOKEN`, and the
   buffering-proxy caveat — behind such a proxy this client hangs rather than failing, which is the
   worst shape a failure can take and the operator should know before deploying it.
   [proof: human: a reader follows the README against a running router and gets a result file]
10. [S10] Verify by hand against a real router and a real worker: submit a file, watch the progress,
    read the output, and check the exit code on a job driven to `dead`.
    [proof: human: an operator runs the binary end to end and reads the exit code from their shell]

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./cmd/client/... -count=1 -race 2>&1 | tee /tmp/adr5-t2.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr5-t2.out \
  && go test ./internal/client/... ./cmd/router/... -count=1 2>&1 | tee /tmp/adr5-t2r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr5-t2r.out
```

`cmd/client` carries the verdict; `internal/client` runs second as T1's regression and `cmd/router`
because the client's tests drive a real router handler. Red at authoring: `cmd/client` does not
exist.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestSubmitWritesResultAndExitsZero` | `cmd/client/main_test.go` | against a stub router, the file contains the units and the exit code is 0 — the happy path every failure test below needs. ⚠ A STUB rather than the real router: the properties here are the BINARY's — exit codes, stream separation, atomic writes — and making a real job succeed or die on demand would need a real worker. `internal/client` covers the protocol and `cmd/router` covers the router; this covers what neither can | — | S1, S6 |
| `TestOutputToADirectoryIsRefused` | `cmd/client/main_test.go` | `-o` naming a directory exits 1 with a message saying so | — | S3 |
| `TestBadParamFormatIsRejected` | `cmd/client/main_test.go` | `--param nokey` exits 1 with a message showing the `k=v` form | — | S2 |
| `TestMissingTokenIsAUsageError` | `cmd/client/main_test.go` | no flag and no environment variable exits 1 naming `OCRR_TOKEN` | — | S2 |
| `TestMissingInputFileExitsOne` | `cmd/client/main_test.go` | a nonexistent `-i` exits 1 | — | S4 |
| `TestRouterUnreachableExitsTwo` | `cmd/client/main_test.go` | a dead router exits 2, not 1 — a caller cannot tell "worth retrying" from "fix something" otherwise | — | S7 |
| `TestExistingOutputIsReplaced` | `cmd/client/main_test.go` | a previous run's content does not survive a successful write | — | S6 |
| `TestCLIExposesEveryFlag` | `cmd/client/main_test.go` | every documented flag is on the real command tree — a flag in the code and not on the CLI is unreachable | — | S2 |
| `TestFailedJobExitsThree` | `cmd/client/main_test.go` | a job driven to `dead` exits **3**, not 0 — the failure that would turn a dead job into silent data loss downstream | — | S7 |
| `TestExpiredJobExitsThree` | `cmd/client/main_test.go` | an expired job also exits 3 | — | S7 |
| `TestNoOutputFileOnFailure` | `cmd/client/main_test.go` | after a failed job the `-o` path does not exist — an empty file beside a non-zero exit invites a caller to use it | — | S8 |
| `TestBadTokenExitsOne` | `cmd/client/main_test.go` | a rejected token exits 1 (fix something), never 2 (retry) — retrying a bad credential forever is the wrong action | — | S7 |
| `TestRateLimitedExitsTwo` | `cmd/client/main_test.go` | a `429` exits 2 (retry later) — the case that distinguishes a typed mapping from a single error code | — | S7 |
| `TestNoCreditsExitsOne` | `cmd/client/main_test.go` | a `402` exits 1, because retrying against an empty balance never succeeds | — | S7 |
| `TestDashOWritesToStdout` | `cmd/client/main_test.go` | `-o -` puts the units on stdout and **nothing else** — the composability claim | — | S5, S6 |
| `TestProgressGoesToStderrNotStdout` | `cmd/client/main_test.go` | with `-o -`, stdout contains only the result while stderr carries the progress — red if a progress line ever reaches stdout, which corrupts every pipe | — | S5 |
| `TestQuietSuppressesProgress` | `cmd/client/main_test.go` | `--quiet` leaves stderr empty on success | — | S5 |
| `TestNonTTYProgressHasNoCarriageReturns` | `cmd/client/main_test.go` | captured stderr contains no `\r` when the output is a pipe — a rewriting progress line is unreadable in a CI log | — | S5 |
| `TestUnwritableDestinationFailsBeforeUpload` | `cmd/client/main_test.go` | `-o` inside a read-only directory exits 1 **and the router received no upload** — the check that stops a customer paying for a result that cannot be written. Skipped under `os.Geteuid() == 0`, where mode bits are not enforced | — | S3 |
| `TestParamsOnlyJobNeedsNoInputFile` | `cmd/client/main_test.go` | `--param url=…` with no `-i` submits successfully — the crawler shape | — | S4 |
| `TestMissingInputAndParamsIsAUsageError` | `cmd/client/main_test.go` | neither `-i` nor `--param` exits 1 with a message naming both | — | S4 |
| `TestTokenFromEnvironment` | `cmd/client/main_test.go` | `OCRR_TOKEN` is used when `--token` is absent, and the flag wins when both are set | — | S2 |
| `TestJSONFlagWritesTheEnvelope` | `cmd/client/main_test.go` | `--json` writes parseable JSON with a `units` array, where the default writes newline-joined text | — | S2 |
| `TestTimeoutExitsTwo` | `cmd/client/main_test.go` | `--timeout` against a job that never completes exits 2, not 3 — a timeout says nothing about whether the job will eventually succeed | — | S2 |
| `TestOutputIsWrittenAtomically` | `cmd/client/main_test.go` | no temporary file survives beside the destination after a successful run | — | S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the eighteen tests above |
| 2 — something selects it | `main.go`'s exit-code mapping selects T1's classification — `TestRateLimitedExitsTwo` and `TestBadTokenExitsOne` both go red if it collapses to a single code, and T1's tests survive that entirely |
| 3 — the caller can discover it | `--help` lists every flag; the README documents the exit codes, which are the part a script depends on and the part no `--help` conveys well |
| 4 — it is used | S10's human sign-off — a real submission against a real router and worker |

## Mutation Log

- 2026-09-15 · 71306ea* · mutant killed · exit 1 · `cmd/client/main.go` · a failed job exits 0, so a pipeline sees success for a job that died — the single worst outcome this binary can produce · acceptance-sha256:c7887d0b72968e4afcf57f99521a147447f144efef7fb2e89541cc33877326a6 · covers:the failed-job exit code
- 2026-09-15 · 71306ea* · mutant killed · exit 1 · `cmd/client/progress.go` · progress is written to stdout, corrupting every pipe the -o - form is used in — the result and the status line interleave and nothing downstream can parse either · acceptance-sha256:c7887d0b72968e4afcf57f99521a147447f144efef7fb2e89541cc33877326a6 · covers:the stderr split
- 2026-09-15 · 71306ea* · mutant killed · exit 1 · `cmd/client/main.go` · an unwritable destination is discovered only after the result is collected and CHARGED, so the customer pays for output that goes nowhere and cannot be fetched again · acceptance-sha256:c7887d0b72968e4afcf57f99521a147447f144efef7fb2e89541cc33877326a6 · covers:the destination probe

## Invariants

- Progress never reaches stdout.
- A job that did not produce a result never exits 0.
- No output file exists after a failure.
- The destination is proven writable before anything is uploaded.

## Risks

- **`TestProgressGoesToStderrNotStdout` is the one that protects every pipe**, and it only works if
  the two streams are captured SEPARATELY. A fixture that merges them passes while stdout is being
  corrupted.
- **The exit-code tests are worthless if they assert "non-zero".** Each asserts its exact code,
  because the whole point is that 1, 2 and 3 mean different actions.
- **`TestUnwritableDestinationFailsBeforeUpload` must assert the router saw NO upload**, not merely
  that the command failed. Failing after the upload still charges the customer, which is the
  outcome the probe exists to prevent.
- **A chmod-based test cannot fail as root**, which is the default in many CI containers. Skipped
  with a stated reason under `os.Geteuid() == 0` rather than passing vacuously — the same
  discipline ADR-0001 T11 used.
- **`TestTimeoutExitsTwo` needs a job that genuinely never completes**, not a slow one: a race
  between the timeout and a completion makes it flaky in exactly the way that gets a test deleted.
- **The human sign-off is not optional.** ADR-0003's history in this repository is a feature that
  passed every test while being unusable in a browser, and this is the first binary whose entire
  purpose is to be run by a person.

## Stop Condition

Stop and ask before adding a retry loop. A client that retries a `429` on its own looks helpful and
removes the caller's ability to decide — exit 2 hands that decision back, and turning it into an
internal loop is a product choice, not an implementation detail.

## Out of Scope

- Batch or directory submission (deferred: `docs/adr/BACKLOG.md`).
- A polling fallback for buffering proxies (deferred: `docs/adr/BACKLOG.md`).
- Automatic retry on exit-2 conditions (permanent: boundary: the caller owns the retry policy; the
  exit code is how it is handed to them).
- Progress as a percentage (permanent: boundary: the router reports states, not fractions, and a
  fabricated percentage is worse than an honest state name).

## Verification Log
- 2026-09-15 · 71306ea* · exit 0 · `set -o pipefail …` · acceptance-sha256:c7887d0b72968e4afcf57f99521a147447f144efef7fb2e89541cc33877326a6 · ms:11157
- 2026-09-15 · 71306ea* · exit 0 · `set -o pipefail …` · acceptance-sha256:c7887d0b72968e4afcf57f99521a147447f144efef7fb2e89541cc33877326a6 · ms:9357
- 2026-09-15 · 71306ea* · exit 0 · `set -o pipefail …` · acceptance-sha256:c7887d0b72968e4afcf57f99521a147447f144efef7fb2e89541cc33877326a6 · ms:9779
- 2026-09-15 · 71306ea* · exit 0 · `set -o pipefail …` · acceptance-sha256:c7887d0b72968e4afcf57f99521a147447f144efef7fb2e89541cc33877326a6 · ms:9110
- 2026-09-15 · 71306ea* · exit 0 · `set -o pipefail …` · acceptance-sha256:c7887d0b72968e4afcf57f99521a147447f144efef7fb2e89541cc33877326a6 · ms:10294
- 2026-09-15 · human-observed · S9+S10: ran the three binaries end to end on 2026-09-15 against a real router, a real worker and a fake OCR command emitting ADR-0001's JSON array. Happy path: 'uploading… / waiting for 01a0a462… / collecting… / done', exit 0, result.txt held three pages, and the customer was charged 3 credits (100→97). -o - piped to wc -l gave exactly 3 lines with no progress in the stream. --json produced a parseable envelope. Exit 3 verified on a real failed job (a worker whose stdout was not a JSON array) with the reason on stderr and NO result file created. Exit 1 verified against a drained balance ('no credits', Payment Required). Exit 2 verified against an unreachable router. -o /dev/null exits 0 and the device node survives as a character device.
