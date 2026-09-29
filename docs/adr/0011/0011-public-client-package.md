# ADR-0011: Move the client protocol out of `internal/` so other modules can import it

**Status:** Accepted
**Date:** 2026-09-29
**Owner:** M
**Spec:** None — no spec stage
**Cross-references:** `docs/adr/0005/0005-cmd-client.md`, `docs/adr/0006/0006-raw-passthrough.md`, `docs/adr/0001-ocr-router-architecture.md`, `README.md`
**Governs:** `client/**`
**Enforced-by:** `client/public_test.go::TestClientPackageIsImportable`
**Invalidates:** none — checked. ADR-0005 decided the SEAM (protocol in a package, flags and rendering in `cmd/client`) and this record keeps that seam exactly as it is; it changes only which directory the package half sits in. ADR-0006 added `Input.Raw` and `Result.Raw` to that package and is likewise unaffected by the path. Both records' `Governs:` lines name `internal/client/**` and are repaired by T1 in the same commit as the move — see the Context.
**Served-path change:** A Go program in another module can write `import "github.com/atvirokodosprendimai/ocr-router/client"` and call `client.Submit(ctx, cfg, in, prog)` to upload a file and block for its result, instead of re-implementing the SSE-before-upload ordering, the backlog replay and the job-id matching against the wire format by hand.

## Context

M, 2026-09-29: *"we need importable package for cliena, with all mechanics. so other clients could
import package, get callback functions like 'take results', 'upload file' etc. without any effort"*.

**The mechanics already exist and are finished.** `internal/client` is 519 lines of protocol plus
776 lines of tests (`git ls-files 'internal/client/*'`, 4 files, 2026-09-29), and it already carries
every element the request names:

| Requested | Exists as | Site |
|---|---|---|
| upload file | `Input{Filename, Body, Label, Pipeline, Params, Raw}` | `internal/client/client.go:37` |
| take results | `Result{JobID, Units, Raw}` | `internal/client/client.go:57` |
| callback functions | `Progress func(Stage, string)` over four stages | `internal/client/client.go:68` |
| the whole mechanic in one call | `Submit(ctx, cfg, in, prog) (Result, error)` | `internal/client/client.go:125` |
| deciding what to do on failure | `*FailedError`, `*RetryableError` | `internal/client/client.go:92`, `:107` |

So the gap is not missing code. **The gap is one directory name.** Go's `internal/` rule is a
compiler-enforced import boundary: a package under `internal/` is importable only from within
`github.com/atvirokodosprendimai/ocr-router`, so the request as written is unsatisfiable while the
package sits there, and satisfied entirely by moving it.

**The blast radius was measured, not estimated** (`grep -rn "internal/client"`, 2026-09-29, over
every tracked file rather than only `*.go`): three non-test files (`cmd/client/main.go`,
`output.go`, `progress.go`) and three test files (`cmd/client/rawoutput_test.go` plus the package's
own two external test files). Six import lines.

⚠ **The part that is invisible from the diff, and is why this is a record rather than a `git mv`:**
`adr-state.mjs` reports `internal/client/**` as governed by both ADR-0005 and ADR-0006. Those two
`Governs:` lines, and ADR-0005's `Enforced-by:` pointer at
`internal/client/client_test.go::TestStreamOpensBeforeUpload`, all name paths that stop resolving
the moment the directory moves. The ADR template names this exact failure from another corpus
(2026-08-28: a directory move re-anchored every path, seven records' `Governs:` stopped naming
anything, `adr-context` answered "none governs" for the whole gate surface, and every gate stayed
green for two days). Repairing them is part of T1, in the same commit as the move, precisely so the
two cannot be separated.

**What this record does not do is enlarge the API.** M's phrase "callback functions like take
results, upload file" reads at first as a request for separate `Upload()` and `Collect()` entry
points. Asked directly on 2026-09-29, he chose **move as-is: `Submit` only**. That is recorded here
rather than inferred, because the opposite reading is the one a later session will arrive at from
the sentence alone.

## Existing Primitives Audit

- **`internal/client` (the whole package)** — REUSE, byte-for-byte. `Submit`, `Config`, `Input`,
  `Result`, `Stage`, `Progress`, `FailedError`, `RetryableError` and the unexported `openStream` /
  `upload` / `collect` / `readFrames` all move unchanged. No behaviour is edited by this record; the
  only edits to Go source are the `package` path in six `import` lines.
- **`internal/client/client_test.go` + `raw_test.go`** — REUSE, unchanged but for their own import
  line. They are already `package client_test`, i.e. already written against the exported surface
  from outside the package, which is why the move costs them nothing. That is also the evidence that
  the exported surface is sufficient on its own: 776 lines of test already drive the protocol
  through `Submit` alone, with no access to an unexported helper.
- **`cmd/client`** — RESHAPE by one line each in three files. ADR-0005's seam (protocol in a
  package, flags/rendering/exit codes in `cmd`) is preserved exactly; `cmd/client` simply imports
  the package from its new path.
- **`cmd/router/packaging_test.go`** — REUSE as a PATTERN, not as code. It is this repository's
  existing answer to "a documentation example rots silently and nothing fails", and T2 follows its
  shape for the README snippet rather than inventing a new one.
- **A `Client` struct, or exported `Upload`/`Collect`** — REPLACE nothing; they are not built. See
  Alternatives.

## Decision

Move `internal/client/` to `client/` at the module root, so its import path becomes
`github.com/atvirokodosprendimai/ocr-router/client`. The move is a `git mv` plus the `import` line
in six files; **no exported or unexported symbol is added, removed, renamed or re-signatured**, and
no function body changes.

In the same commit, repair the three corpus pointers the move would otherwise strand: ADR-0005's
`Governs:` and `Enforced-by:`, and ADR-0006's `Governs:`.

Then document the surface where a caller will look for it: a compiled `Example` in
`client/example_test.go` (which `go doc` renders and `go test` runs) and a README section under
`## The client` showing the five-line import-and-submit shape.

**What would make this decision wrong, and whether it can be observed today.** The falsifying
condition is that the exported surface turns out to be insufficient on its own — that a caller
outside the module cannot do what `cmd/client` does without reaching for an unexported helper.
**That is already observable and already answered:** the package's own tests are `package
client_test`, compiled from outside the package against the exported surface only, and they cover
the whole protocol including reconnection, raw mode and cancellation. If they needed an unexported
symbol they would not compile today. A second falsifier — a caller needing to fire a job and collect
it from a different process — is real, is NOT covered by `Submit`, and is deliberately out of scope
below on M's call.

## Alternatives Considered

- **Also export `Upload` and `Collect` separately:** promote the two unexported halves of `Submit`
  so a caller can post a job, keep the id, and collect the result later from another process.
  Rejected — M chose the as-is move on 2026-09-29. It is also a strictly larger permanent contract:
  `Submit`'s correctness argument is the ORDERING (stream established before upload,
  `internal/client/client.go:112-124`), and exporting the halves hands a caller the two pieces in an
  order they can get wrong, with a failure mode — a fast job stranding the caller forever — that
  passes every casual test. Recorded in Out of Scope with a pointer, not discarded.
- **Wrap `Config` in a `client.New(cfg) *Client` with methods:** the conventional SDK shape.
  Rejected — M chose as-is. It would also rewrite every call site and all 776 lines of test for an
  ergonomic difference of one value passed per call, and `Submit`'s signature is already
  self-contained.
- **`pkg/client/` instead of `client/`:** rejected. `pkg/` adds a segment that carries no meaning in
  a module with one public package, and the Go project's own layout guidance treats it as optional
  at best. `client/` reads as `…/ocr-router/client`, which is what an importer types.
- **Leave it in `internal/` and publish a thin copy at `client/`:** rejected outright. Two live
  copies of a protocol is the shape where one is fixed and the other is not, and the divergence is
  invisible until a caller hits it in production.
- **Leave it in `internal/` and tell callers to vendor or copy the file:** rejected. It answers the
  literal request with the maximum ongoing cost — every consumer re-inherits the SSE ordering bug
  the moment the router's wire format moves.

## Component / Boundary Impact

Inherits ADR-0005's seam unchanged: the protocol is a library with no flags, no terminal output and
no exit codes; `cmd/client` owns all three. Nothing about ownership moves with the directory.

| Component | Ownership after | One reason to change? |
|---|---|---|
| `client/` (was `internal/client/`) | The router's wire protocol, as a library. Now a PUBLIC contract other modules pin. | Yes — it changes when the router's wire format changes. |
| `cmd/client/` | Flags, rendering, exit codes. Unchanged. | Yes — it changes when the CLI's UX changes. |
| `docs/adr/0005/`, `docs/adr/0006/` | Unchanged authority; repaired path pointers only. | n/a — no decision changes. |

The one boundary property that genuinely changes: `client/` acquires an audience outside this
repository, so a breaking change to it is no longer a same-commit refactor. That cost is in
Consequences, and it is the cost M accepted.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| Go import path `…/ocr-router/client` | **New public API.** The package and every exported symbol in it become importable from other modules. No symbol's name or signature changes. | `client/` (T1) | any external Go module; `cmd/client` |
| Go import path `…/ocr-router/internal/client` | **Removed.** Only ever reachable from inside this module; six in-repo import lines are updated in the same commit. | — | `cmd/client` (T1) |
| `docs/adr/0005/0005-cmd-client.md` `Governs:` / `Enforced-by:` | Path repair, `internal/client/**` → `client/**`. No decision text changes. | T1 | `adr-context`, `adr-state` |
| `docs/adr/0006/0006-raw-passthrough.md` `Governs:` | Path repair, `internal/client/**` → `client/**`. No decision text changes. | T1 | `adr-context`, `adr-state` |
| `README.md` § The client | New subsection documenting the import path and the `Submit` shape. | T2 | a developer or agent adopting the package |

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| The `github.com/atvirokodosprendimai/ocr-router/client` import path | T1 | T2 | No — T2 only reads and documents the surface T1 relocates. T2 cannot compile before T1 lands. |

## Implementation

See `tasks/README.md`. Two tasks, sequential.

## Consequences

- **Positive:** the request is satisfied with a move rather than a build — no new protocol code, and
  therefore no new protocol bugs. An external caller inherits the SSE-before-upload ordering, the
  backlog replay that survives a dropped stream, the job-id match, and the typed errors. ⚠ THREE OF
  THOSE FOUR ARE MUTATION-VERIFIED under ADR-0005-T1 and ADR-0006-T7 — the ordering, the job-id
  match and the typed failure. The BACKLOG REPLAY has a test and no mutant, so it is covered rather
  than proved; an earlier draft of this line claimed all four, and a review caught it.
- **Positive:** the corpus's path pointers are repaired in the same commit as the move that would
  have broken them, so `adr-context` keeps answering for this code.
- **Negative:** `client/` becomes a public compatibility surface. A rename or signature change in it
  is now a breaking change for consumers this repository cannot see or grep, where until now it was
  a same-commit refactor across six lines. Nothing in this repository can detect a downstream break;
  that is inherent to publishing and is the cost M accepted.
- **Negative:** `Submit` is a blocking, single-process call. A caller who needs to submit and collect
  from different processes has no supported path and must wait for the deferred work below.
- **Neutral:** `cmd/client` keeps behaving identically — same flags, same exit codes, same output.
  Nothing a current CLI user does changes.
- **Neutral:** the module's `internal/` tree loses one package. Every other `internal/` package stays
  private, which is still correct: they are the router's guts, not a client's protocol.

## Out of Scope

- Exported `Upload()` and `Collect()` entry points, for submitting a job in one process and
  collecting its result in another (deferred: `docs/adr/BACKLOG.md`)
- A `client.New(cfg) *Client` handle with methods, instead of passing `Config` per call (deferred:
  `docs/adr/BACKLOG.md`)
- Any change to the exported surface's names, signatures or behaviour — this record moves a
  directory and nothing else, so that every verification failure during execution points at the move
  rather than at a behaviour change smuggled alongside it (permanent: boundary: the whole value of
  an as-is move is that the existing mutation evidence for ADR-0005-T1 and ADR-0006-T7 still applies
  unchanged to the relocated code)
- Publishing a versioned release or a `v2` module path for the new package (permanent: boundary:
  the module is `go 1.26.6` at `v0` and unreleased, so a consumer pins a commit; semantic-version
  policy is a decision for whenever the first tag is cut, and pre-deciding it here would bind a
  release nobody has scheduled)
- Making the other `internal/` packages importable (permanent: boundary: they are the router's
  internals — `store`, `router`, `httpapi`, `session` — and ADR-0001's security split depends on a
  worker or a client holding no router capability; publishing them would be a privilege decision,
  not a packaging one)
- A non-Go client library, in any language (permanent: boundary: the HTTP + SSE API is already the
  language-neutral contract and is documented in `README.md` § The API; a second hand-written client
  is a second thing to keep in step with the wire format)
- Repairing the `internal/client` references inside ADR-0005's and ADR-0006's TASK files and prose
  bodies (permanent: boundary: those are historical records of runs that happened against paths that
  existed at the time, and rewriting them would falsify where work was done — only the machine-read
  `Governs:` and `Enforced-by:` headers, which tooling resolves against today's tree, are repaired)

## Risks

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| A missed import line leaves the tree not compiling | Low | Low | `go build ./...` is inside T1's Acceptance and fails loudly; the six sites were enumerated by `grep -rn "internal/client" --include=*.go` and are listed in T1's Affected Files. |
| The move is used as cover for an API change, so a later failure is misattributed | Low | Med | T1's Invariants forbid any exported symbol change, and its Acceptance diffs the exported surface before and after with `go doc` rather than trusting review to notice. |
| ADR-0005 / ADR-0006 path pointers are repaired in a LATER commit, or not at all, leaving `adr-context` blind to this code | Med | Med | The repair is in T1's Affected Files and asserted by T1's `TestCorpusPointersDoNotNameInternalClient`, so the commit that moves the directory cannot go green without it. |
| Someone later "tidies" the package back under `internal/`, silently unpublishing it | Low | High | `client/public_test.go::TestClientPackageIsImportable` is the `Enforced-by:` check and fails on exactly that. |
| The README snippet drifts from the real API | Med | Low | T2's `TestReadmeClientExampleNamesRealSymbols`, following `cmd/router/packaging_test.go`'s existing pattern for the same failure. |

## Rollback

`git revert` the T1 commit. The move touches no persistent state, no migration, no wire format and
no stored data — it is a directory rename plus six import lines and three header lines — so the
revert is complete and leaves a tree identical to `fc9086b` plus T2's docs, which are inert on their
own. The only thing a revert cannot recall is a consumer who has already pinned
`…/ocr-router/client`; that risk starts the moment this merges and is the published-surface cost
named in Consequences.

## Follow-ups

- [ ] M to confirm the package path `client/` rather than `pkg/client/` before the first external
      consumer pins it — the choice is the author's (Alternatives), and it is cheap to change now
      and expensive after someone imports it.
