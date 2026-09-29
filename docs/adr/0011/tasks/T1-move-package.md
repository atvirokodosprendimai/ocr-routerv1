# Task ADR-0011-T1: Move `internal/client` to `client/` without changing one exported symbol

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** the `github.com/atvirokodosprendimai/ocr-router/client` import path, and the repaired `Governs:` / `Enforced-by:` headers in ADR-0005 and ADR-0006
**Consumes:** none
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the package's new location`, `the exported surface being unchanged`, `the corpus pointers naming the new path`

## Goal

Relocate the client protocol package out of `internal/` so other Go modules can import it, changing
no symbol and no behaviour.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/client/client.go` → `client/client.go` | move | The package itself. `git mv`; the only edit is none — the `package client` line is already right. |
| `internal/client/sse.go` → `client/sse.go` | move | Frame reassembly, same package. |
| `internal/client/client_test.go` → `client/client_test.go` | move + edit | Already `package client_test`; its own `import` of the package under test is one of the six lines. |
| `internal/client/raw_test.go` → `client/raw_test.go` | move + edit | Same. |
| `cmd/client/main.go` | edit | Import line. **This is the file that SELECTS the package** — `cmd/client` is the only in-repo caller of `client.Submit`, so deleting this import is what makes the package unreachable, and `TestSubmitWritesResultAndExitsZero` is what goes red when it is. |
| `cmd/client/output.go` | edit | Import line (`client.Result`). |
| `cmd/client/progress.go` | edit | Import line (`client.Stage`). |
| `cmd/client/rawoutput_test.go` | edit | Import line. |
| `client/public_test.go` | add | The record's `Enforced-by:` check, plus the corpus-pointer check. New file, `package client_test`. |
| `docs/adr/0005/0005-cmd-client.md` | edit | `Governs:` and `Enforced-by:` headers only — `internal/client/**` → `client/**`, and the test pointer's path. Tooling resolves these against today's tree; the prose body and task files are left alone deliberately (ADR-0011 §Out of Scope). |
| `docs/adr/0006/0006-raw-passthrough.md` | edit | `Governs:` header only, same repair. |

## Ordered Steps

1. [S1] Confirm the failing tests are red before the move: write `client/public_test.go` holding
   `TestClientPackageIsImportable` and `TestCorpusPointersDoNotNameInternalClient`, run them, and
   confirm BOTH fail — the first because `client/client.go` does not exist yet, the second because
   ADR-0005 and ADR-0006 still say `internal/client/**`. ⚠ Create the `client/` directory for the
   test file now; a test that cannot be placed until after the move cannot be red before it, and
   TDD red is the point.
2. [S2] [proof: human: the author reads the captured file and confirms it describes the pre-move package] Capture the exported surface as it stands: `go doc -all ./internal/client > /tmp/surface-before.txt`. It must be taken before anything moves; it is what S5 diffs against, and a test asserting the file merely exists would pass on an empty one.
3. [S3] `git mv internal/client client`, then update the import path in the six files listed above
   (`grep -rln "ocr-router/internal/client" --include=*.go .` enumerates them; four are in
   `cmd/client`, two are the package's own external tests). Use `mrw write` — this is six hunks
   across six files.
4. [S4] Repair the corpus pointers: ADR-0005's `Governs:` and `Enforced-by:`, and ADR-0006's
   `Governs:`. Header lines only. `TestCorpusPointersDoNotNameInternalClient` is what proves this
   was not skipped. [proof: acceptance]
5. [S5] [proof: human: the author reads the diff and judges whether each line is the import path or a changed symbol — a mechanical equality check would have to encode that one exception, and would then pass on a changed symbol that happened to sit on the same line] Confirm the surface is byte-identical: `go doc -all ./client > /tmp/surface-after.txt`, then `diff <(sed 's#/internal/client#/client#' /tmp/surface-before.txt) /tmp/surface-after.txt` must print nothing but the package's own import-path line. Any other difference means a symbol changed and this task has exceeded its scope — stop and re-read the Invariants.
6. [S6] Run the full project gate and confirm the six moved/edited call sites all compile and the
   package's own 776 lines of test still pass unchanged.

## Acceptance

```bash
set -o pipefail
go test ./client/... -count=1 -run 'TestClientPackageIsImportable|TestCorpusPointersDoNotNameInternalClient' -v 2>&1 | tee /tmp/acc-0011-T1a.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/acc-0011-T1a.out \
  && templ generate && go build ./... && go vet ./... && test -z "$(gofmt -l .)" \
  && go test ./client/... ./cmd/client/... -count=1 2>&1 | tee /tmp/acc-0011-T1b.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0011-T1b.out
```

<The two new checks run FIRST and alone, so neither can be carried by the 776 lines of pre-existing
test that follow — that is the "which of these subjects could carry the verdict by itself" question,
and before the move the regression half is green on its own while nothing is done. The regression
half is scoped to `./client/...` and `./cmd/client/...`, the only packages this task touches; the
build/vet/gofmt legs still cover the whole tree, because a stale import anywhere else is exactly the
failure mode a scoped test run would miss. `go doc` is deliberately NOT in the fence: S5's judgement
is human (see its proof marker) and a fence cannot make it.>

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestClientPackageIsImportable` | `client/public_test.go` | Walking up to the module root, `client/client.go` exists and no `internal/client` directory does — the decision stated as a check. Red before the move, and red again if anyone moves the package back. | — | S1, S3 |
| `TestCorpusPointersDoNotNameInternalClient` | `client/public_test.go` | The `Governs:` and `Enforced-by:` header lines of ADR-0005 and ADR-0006 contain no `internal/client` path. This is the check that makes the pointer repair inseparable from the move. | — | S1, S4 |
| `TestStreamOpensBeforeUpload` | `client/client_test.go` | Unchanged, moved. ADR-0005's `Enforced-by:` check; it is here so the fence proves the relocated package still enforces the ordering the move must not disturb. | — | S6 |
| `TestSubmitWritesResultAndExitsZero` | `cmd/client/main_test.go` | Unchanged. The rung-2 check: it goes red if `cmd/client`'s import of the package is dropped rather than repointed, which is what proves the package is still SELECTED and not merely present. | — | S3, S6 |
| `TestRawFlagReachesUploadQuery` | `client/raw_test.go` | Unchanged, moved. ADR-0006's behaviour survives the move. | — | S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestClientPackageIsImportable`, plus the 776 lines of moved test that exercise the protocol. |
| 2 — something selects it | `cmd/client/main.go`'s import and its `client.Submit` call. The mutation that proves it: delete that import — `cmd/client` stops compiling and `TestSubmitWritesResultAndExitsZero` goes red inside the Acceptance fence. |
| 3 — the caller can discover it | Partly, here: the import path is the declared interface and `TestClientPackageIsImportable` checks it resolves. The godoc `Example` and the README section that make it DISCOVERABLE are T2's job, and rung 3 is not honestly closed until T2 lands. |
| 4 — it is used | Nothing measures this yet. No external consumer exists, and this repository cannot see one if it did — that is named as a permanent cost in ADR-0011 §Consequences. |

## Mutation Log

## Invariants

- **No exported symbol is added, removed, renamed or re-signatured.** `go doc -all` before and after
  differ only in the package's own import path (S5).
- No function body changes. The only edits to `.go` files are `import` lines and the new test file.
- ADR-0005's seam holds: `client/` has no flags, writes nothing to a terminal, and decides no exit
  codes; `cmd/client` keeps all three.
- `cmd/client`'s observable behaviour is identical — same flags, same exit codes, same stdout/stderr
  split. Its 590-line test file is unchanged and must stay green.
- Every other package stays under `internal/`. This task publishes exactly one package.
- ADR-0005's and ADR-0006's DECISION text is untouched; only their machine-read path headers move.

## Risks

- A missed import line leaves the tree not compiling — caught by `go build ./...` in the fence, and
  the six sites are enumerated by a grep rather than from memory.
- The move is used as cover for an API change — S5's surface diff and the first Invariant exist for
  exactly this, and the human proof marker on S5 says who judges it.
- The corpus pointers are repaired in a later commit, or not at all — `TestCorpusPointersDoNotNameInternalClient`
  runs first in the fence, so the commit cannot go green without them.

## Stop Condition

Stop and ask if the surface diff in S5 shows anything but the import-path line: that means the move
is not as-is, and M chose as-is explicitly on 2026-09-29.

Stop and ask if repairing ADR-0005's `Enforced-by:` pointer turns out to require changing what that
check asserts rather than only where it lives — that would mean the move changed a decision's
enforcement, which is a different record.

## Out of Scope

- The godoc `Example` and the README section — T2's job.
- Rewriting `internal/client` references in ADR-0005's and ADR-0006's prose bodies and task files:
  they are historical records of runs against paths that existed then (ADR-0011 §Out of Scope).
- Any new exported symbol (deferred: `docs/adr/BACKLOG.md`).

## Verification Log
