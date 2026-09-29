# ADR-0011 Tasks

Implementation tasks for ADR-0011: Move the client protocol out of `internal/` so other modules can
import it. See the parent ADR for the decision.

**Source of truth:** the task files' `Depends-on` / `Produces` / `Consumes` / `Covers` headers.
This README is a derived index — when it disagrees with a task file, the task file wins and the
README must be regenerated. Regenerate rather than hand-edit.

## Execution Order

| Order | Task | Depends-on |
|-------|------|------------|
| 1 | T1 | none |
| 2 | T2 | T1 |

Two tasks, strictly sequential: T2's Example imports the path T1 creates, so it cannot compile
before T1 lands.

## Task Index

| ID | Title | Status | Covers | Acceptance |
|----|-------|--------|--------|------------|
| T1 | Move `internal/client` to `client/` without changing one exported symbol | pending | — | `go test ./client/... -run 'TestClientPackageIsImportable\|TestCorpusPointersDoNotNameInternalClient'`, then the full build/vet/gofmt gate and `go test ./client/... ./cmd/client/...` |
| T2 | Make the public surface discoverable — a compiled godoc Example and a README section | pending | — | `go test ./client/... -run 'TestReadmeClientExampleNamesRealSymbols\|Example'`, then `go doc ./client Submit` and `go test ./client/... ./cmd/client/...` |

Status: `pending` | `partial` | `blocked` | `done`.

## Contract Coupling

| Producer | Contract | Consumer(s) | Ordering note |
|----------|----------|-------------|---------------|
| T1 | The `github.com/atvirokodosprendimai/ocr-router/client` import path | T2 | T1 before T2 — T2's Example does not compile until the path exists. |

## Notes

- **T1 moves a directory and repairs three header lines in two OTHER records' front matter**
  (ADR-0005's `Governs:` and `Enforced-by:`, ADR-0006's `Governs:`). That is deliberate and is
  asserted by `TestCorpusPointersDoNotNameInternalClient`, so the two cannot be separated into
  different commits. It does NOT touch those records' prose bodies or task files — those are
  historical accounts of work done against paths that existed then.
- **T1 changes no behaviour at all.** If anything in its Acceptance goes red beyond a stale import,
  the move has exceeded its scope; T1's `## Stop Condition` says to stop rather than to fix forward.
- The existing mutation evidence for ADR-0005-T1 and ADR-0006-T7 still applies to the relocated
  code, because no line of it changes. That is the reason the move is as-is rather than an
  as-is-plus-a-little.
- Pre-flight for T1: the tree must be green before the move, or a failure afterwards cannot be
  attributed. Run the project gate first — `templ generate && go build ./... && go vet ./... &&
  test -z "$(gofmt -l .)" && go test ./... -count=1` — and note the exit code.
- ⚠ CI here is GitHub's default CodeQL setup and does **not** run `go test`. A green run is not
  evidence for either task; the Acceptance fences are.
