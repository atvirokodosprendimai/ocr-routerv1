# Task ADR-0011-T2: Make the public surface discoverable — a compiled godoc Example and a README section

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** S (single file, plus a README section)
**Owner:** unassigned
**Produces:** the documented import-and-submit shape, in godoc and in `README.md`
**Consumes:** the `github.com/atvirokodosprendimai/ocr-routerv1/client` import path (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the Example compiling against the exported surface`, `the README naming symbols that exist`

## Goal

Give a developer arriving at the package a copy-pasteable way in, in the two places they will
actually look — `go doc` and the repository README — and make both fail loudly when the API moves
underneath them.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `client/example_test.go` | add | `package client_test`, holding `Example()` and `ExampleSubmit_progress()`. **This is the rung-3 artefact:** godoc renders an `Example` beside the symbol it names, so it is what a caller discovers without reading source — and because it is compiled, it is also a check. |
| `client/doc_example_test.go` | — | Not created. One file is enough; named here so the reader knows the split was considered and rejected. |
| `README.md` | edit | A new subsection under the existing `## The client` heading (currently line 139), which today documents only the CLI binary. This is where a reader who already found the CLI looks next. |
| `client/readme_test.go` | add | `TestReadmeClientExampleNamesRealSymbols` — the guard against README rot, following `cmd/router/packaging_test.go`'s existing pattern for exactly this failure. |

## Ordered Steps

1. [S1] Confirm the failing tests are red first: write `TestReadmeClientExampleNamesRealSymbols` in `client/readme_test.go` and run it against the un-edited README. It must FAIL, because the README has no `client.` snippet yet and the test refuses an empty match set. ⚠ A test that passes when it finds nothing is the exact shape this task is guarding against, so assert a non-zero symbol count explicitly.
2. [S2] Write `client/example_test.go` in `package client_test`: an `Example()` showing config, opening a file, `Submit`, and reading `Result.Units`; and an `ExampleSubmit_progress()` showing the `Progress` callback. Both must compile against exported symbols only — no `//go:build ignore`, and no output-comparison comment on `Example()`, since it would need a live router.
3. [S3] Add the README subsection under `## The client`, carrying the import path, the five-line `Submit` shape, and one sentence each on `FailedError` and `RetryableError` — the two the caller must branch on. [proof: human: a person reads the rendered section and judges whether it is a way IN for someone who has never seen the package; no check can measure that]
4. [S4] Re-run `TestReadmeClientExampleNamesRealSymbols` and confirm it now passes because every `client.X` the README names is genuinely exported. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./client/... -count=1 -run 'TestReadmeClientExampleNamesRealSymbols|TestExamplesAreAttachedForGodoc' -v 2>&1 | tee /tmp/acc-0011-T2a.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/acc-0011-T2a.out \
  && go vet ./client/... && test -z "$(gofmt -l .)" \
  && go test ./client/... ./cmd/client/... -count=1 -race 2>&1 | tee /tmp/acc-0011-T2c.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0011-T2c.out
```

<The two new units run FIRST and alone, so the T1-era regression that follows cannot carry the
verdict.

⚠ THIS FENCE ONCE CONTAINED A LEG THAT COULD NEVER PASS, and the correction is the most useful thing
in this task. It read `go doc ./client Submit | grep -q "Example"`, on the belief that `go doc`
renders examples. IT DOES NOT — example rendering is a godoc/pkgsite feature and the `go doc` CLI
emits no "Example" however correct the code is, reproduced on go1.26.6 by an independent review on
2026-09-29. The leg would have failed AFTER the work was done correctly, which is exactly inverted
from the failure it was defending against. ★ THE GENERAL LESSON: a gate asserting that a TOOL
reports something has to be run once against a known-good tree, or it encodes the author's belief
about the tool rather than a fact about the code. `TestExamplesAreAttachedForGodoc` replaces it and
asks the same question through `go/doc`, which is the package godoc itself uses. The regression leg
is scoped to the two packages this task can reach, and now carries `-race`, which ADR-0005-T1's
original acceptance required and an earlier draft of this fence silently dropped.>

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestReadmeClientExampleNamesRealSymbols` | `client/readme_test.go` | Every `client.<Ident>` the README's Go fence names is a symbol the package actually exports, and at least one was found — so the test cannot pass by matching nothing. | — | S1, S4 |
| `TestExamplesAreAttachedForGodoc` | `client/godoc_test.go` | `go/doc` binds a package-level example and an example on `Submit` — the rung-3 property, that a reader DISCOVERS them, which compiling alone does not give. | — | S2 |

⚠ **The `Example` functions themselves are NOT rows here, and that is the correction.** An earlier
draft listed `Example` and `ExampleSubmit_progress` as tests. They are not: an Example with no
`// Output:` comment is COMPILED AND NEVER RUN, and nothing in one can go red — there is no failure
call in its body. `adr-lint` says so outright. What they give is a compile-time check of the
exported surface, which is real and is not a test; what proves they are DOCUMENTATION rather than
dead code is `TestExamplesAreAttachedForGodoc`, which is a test and can fail.

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | The two `Example` functions compile and run under `go test`. |
| 2 — something selects it | godoc selects them by naming convention. The mutation that proves it: rename `ExampleSubmit_progress` to `TestProgressShape` — it still compiles and still passes, and the `go doc ./client Submit` leg of the fence goes red because nothing is attached any more. That is precisely the failure a test-only check cannot see. |
| 3 — the caller can discover it | This task IS rung 3 for ADR-0011, and it closes what T1 left open: the import path exists after T1, but nothing tells a caller what to type. `go doc` and the README section are the declared interface; the `go doc` fence leg and `TestReadmeClientExampleNamesRealSymbols` are the source checks on them. |
| 4 — it is used | Nothing measures this yet. No external consumer exists and this repository cannot observe one — ADR-0011 §Consequences names that as inherent to publishing. |

## Mutation Log

- 2026-09-29 · 43f2fb0* · mutant inconclusive · exit 1 · `client/example_test.go` · the example is renamed off the Example<Symbol> convention: it still compiles and still passes, and silently stops rendering beside Submit for every reader — the rung-3 failure a compile-only check cannot see · acceptance-sha256:ab7e5680bf94a2bdce15c0d6fc675cbdfcce19faba199533cfdc2e19f85bcd4e · covers:the Example compiling against the exported surface
  ```
  the fence failed on a build/parse error, not an assertion
  ```
- 2026-09-29 · 43f2fb0* · mutant killed · exit 1 · `client/example_test.go` · the example is renamed to a symbol that EXISTS, so go vet accepts it and the package still builds — but godoc now renders it beside Config instead of Submit, and the only entry point the package has loses its worked example with nothing reporting it · acceptance-sha256:ab7e5680bf94a2bdce15c0d6fc675cbdfcce19faba199533cfdc2e19f85bcd4e · covers:the Example compiling against the exported surface
- 2026-09-29 · 43f2fb0* · mutant killed · exit 1 · `README.md` · the README documents a function the package does not export, so the first thing an adopter copies does not compile — the exact rot this check exists for · acceptance-sha256:ab7e5680bf94a2bdce15c0d6fc675cbdfcce19faba199533cfdc2e19f85bcd4e · covers:the README naming symbols that exist

## Invariants

- No non-test, non-doc file changes. This task adds two `_test.go` files and edits `README.md`; it
  touches no package source and no exported symbol.
- The Example uses only exported symbols. Reaching for an unexported helper would falsify ADR-0011's
  stated premise that the exported surface is sufficient on its own.
- The README's existing `## The client` CLI documentation is added to, not replaced — the CLI
  remains the answer for someone who wants a binary rather than a library.
- `TestReadmeClientExampleNamesRealSymbols` asserts a non-zero match count, so it cannot go green by
  finding nothing.

## Risks

- The README snippet drifts from the API — that is what `TestReadmeClientExampleNamesRealSymbols`
  exists for, and the rung-2 row above says which mutation proves it can fail.
- The Example is written as a test rather than as documentation, i.e. named wrongly and never
  rendered — caught by the `go doc` leg of the fence, which is in the fence for this reason alone.
- The symbol-extraction regex over the README matches too loosely and passes on anything — mitigated
  by asserting against the package's real exported set rather than against a hand-kept list.

## Stop Condition

Stop and ask if the Example cannot be written without an unexported symbol: that falsifies ADR-0011's
premise that the exported surface is sufficient, and it makes the deferred `Upload`/`Collect` work
a prerequisite rather than a follow-up.

## Out of Scope

- The move itself — T1's job.
- Any new exported symbol to make the Example prettier (deferred: `docs/adr/BACKLOG.md`).
- Documenting the HTTP + SSE API for non-Go callers; `README.md` § The API already owns that.

## Verification Log
- 2026-09-29 · 43f2fb0* · exit 0 · `set -o pipefail …` · acceptance-sha256:ab7e5680bf94a2bdce15c0d6fc675cbdfcce19faba199533cfdc2e19f85bcd4e · ms:3497
- 2026-09-29 · 43f2fb0* · exit 0 · `set -o pipefail …` · acceptance-sha256:ab7e5680bf94a2bdce15c0d6fc675cbdfcce19faba199533cfdc2e19f85bcd4e · ms:3671
- 2026-09-29 · 43f2fb0* · exit 0 · `set -o pipefail …` · acceptance-sha256:ab7e5680bf94a2bdce15c0d6fc675cbdfcce19faba199533cfdc2e19f85bcd4e · ms:4500
- 2026-09-29 · 43f2fb0* · exit 0 · `set -o pipefail …` · acceptance-sha256:ab7e5680bf94a2bdce15c0d6fc675cbdfcce19faba199533cfdc2e19f85bcd4e · ms:5122
