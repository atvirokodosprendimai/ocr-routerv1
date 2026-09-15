# Task ADR-0002-T2: A slog logger whose signature makes logging a param value impossible

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** S (single file plus its test)
**Owner:** unassigned
**Produces:** `logging.Logger`, `logging.New()`, `logging.Options`, `logging.Job()`, `logging.Nop()`
**Consumes:** `core.ValidParamKey` (ADR-0001-T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the key-only param encoding`, `the level filter`

## Goal

Build the `log/slog` handler the router logs through, and make the redaction rule structural: there
is no argument anywhere in this package's surface that accepts a param VALUE, so no call site can
leak one by forgetting.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/logging/logging.go` | add | `New`, the JSON/text handler choice, level parsing, `Job`, `Nop` |
| `internal/logging/logging_test.go` | add | the failing tests, above all the redaction ones |

Nothing selects this package yet — T3 mounts the request middleware and T4 wires the transition
logging. Rung 2 is discharged there.

## Ordered Steps

1. [S1] Write the failing redaction test first: `Job()` given a param map whose value is a
   distinctive sentinel must emit the KEY and never the sentinel (TDD red). [proof: acceptance]
2. [S2] `Options{Level string, Format string, Out io.Writer}` and `New(Options) (*slog.Logger,
   error)`. `Format` is `json` (default) or `text`; `Level` is one of `debug|info|warn|error`. An
   unrecognised value is an **error**, not a silent fallback — an operator who typed `--log-level
   verbose` and got info-level logging would have no way to discover it.
3. [S3] `Nop()` returns a logger writing to `io.Discard` at a level above error. It exists so the
   consumers in T3 and T4 need no nil checks, on the same reasoning as T11's `nopCounter`.
4. [S4] ⚠ **`Job(id, userID, label string, params map[string]string) slog.Attr`** takes the map and
   emits a `params` attribute holding only the **sorted keys**. There is deliberately no variant
   that takes values: the rule is enforced by the absence of an argument rather than by asking
   every call site to remember. Keys are safe to emit because `core.ValidParamKey` already bounds
   them to `^[a-z][a-z0-9-]{0,31}$`; values have no constraint at all and ADR-0001 lets a crawler
   receive `?url=…`, which carries credentials often enough to treat as certain.
5. [S5] Keys are SORTED before emission, so two identical jobs produce identical lines and a diff
   of two runs shows real differences rather than map iteration order.
6. [S6] The handler writes to `Options.Out`, defaulting to `os.Stdout`. Injectable only so the
   tests can read what was written — every test in this package asserts against real emitted JSON
   rather than against a mock, because the thing being tested is what comes out.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go test ./internal/logging/... -count=1 2>&1 | tee /tmp/adr2-t2.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr2-t2.out
```

Red at authoring: `internal/logging` does not exist, so the run reports `no test files` and the
grep makes the fence non-zero.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestJobLogsParamKeysAndNeverValues` | `internal/logging/logging_test.go` | with `{"url": "https://user:hunter2@example.com/x"}`, the emitted line contains `url` and contains neither `hunter2` nor the host — **and asserts the key IS present**, so an implementation that logs nothing at all fails too | — | S4 |
| `TestJobWithNoParamsStillLogsTheJob` | `internal/logging/logging_test.go` | an empty param map emits the job id and an empty key list, not a missing attribute — the shape stays constant so a log query can rely on it | — | S4 |
| `TestParamKeysAreSorted` | `internal/logging/logging_test.go` | keys inserted in one order emit in sorted order, checked across 20 iterations so Go's randomised map order would break it | — | S5 |
| `TestOutputIsJSONByDefault` | `internal/logging/logging_test.go` | the default format parses as JSON with `level`, `msg` and `time` keys present | — | S2 |
| `TestTextFormatIsAvailable` | `internal/logging/logging_test.go` | `Format: "text"` emits non-JSON key=value output — the human-at-a-terminal escape hatch | — | S2 |
| `TestLevelFilters` | `internal/logging/logging_test.go` | at `warn`, an info line is absent and a warn line is present — the filter actually filters | — | S2 |
| `TestUnknownLevelIsAnError` | `internal/logging/logging_test.go` | `Level: "verbose"` returns an error rather than defaulting to info — a silent fallback leaves an operator with logging they cannot discover is wrong | — | S2 |
| `TestUnknownFormatIsAnError` | `internal/logging/logging_test.go` | `Format: "logfmt"` returns an error for the same reason | — | S2 |
| `TestNopWritesNothing` | `internal/logging/logging_test.go` | `Nop()` given an error-level call produces no bytes on a writer it is pointed at | — | S3 |
| `TestJobAttrIsUsableFromSlogDirectly` | `internal/logging/logging_test.go` | `logger.Info("x", logging.Job(...))` produces a nested `params` array — proves the return type composes with plain `slog`, which is how T4 will call it | — | S4 |
| `TestDefaultOutputIsStdoutNotStderr` | `internal/logging/logging_test.go` | with `Out` unset, a log line arrives on a pipe substituted for `os.Stdout` and nothing on one substituted for `os.Stderr` — `slog`'s own default is stderr, which would split the router's output across two streams | — | S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the ten tests above |
| 2 — something selects it | **nothing selects it yet.** T3's request middleware and T4's transition logging are the call sites, and their mutants are where this is discharged. Recorded rather than claimed, because a package with tests and no caller is this pipeline's most common shipped defect. |
| 3 — the caller can discover it | doc comments on every exported identifier; `--log-level` and `--log-format` arrive in T4 |
| 4 — it is used | nothing measures log volume; the operator reading stdout is the use, and that is not observable from here |

## Mutation Log

- 2026-09-15 · 7422213* · mutant killed · exit 1 · `internal/logging/logging.go` · the whole param map is logged, so a crawler URL carrying a password reaches every log line and every aggregator downstream · acceptance-sha256:d64a89ce0e22ba3f673e7c5e34a3748beb806de1828f281029dee3e87211325f · covers:the key-only param encoding
- 2026-09-15 · 7422213* · mutant killed · exit 1 · `internal/logging/logging.go` · an unrecognised level silently becomes info, so an operator who typed --log-level verbose has logging they cannot discover is wrong · acceptance-sha256:d64a89ce0e22ba3f673e7c5e34a3748beb806de1828f281029dee3e87211325f · covers:the level filter

## Invariants

- No exported function in this package accepts a param value.
- `Job` emits sorted keys and nothing derived from a value — not a length, not a hash, not a prefix.
- An unrecognised level or format is an error, never a silent default.
- `Nop()` writes no bytes at any level.

## Risks

- **A redaction test that only asserts the sentinel is ABSENT passes vacuously** when the
  implementation logs nothing, when the fixture has no params, or when the sentinel is misspelled.
  `TestJobLogsParamKeysAndNeverValues` therefore asserts the key IS present in the same run. This
  is the single most important line in the task.
- **A future helper could reintroduce the hole** — `JobWithParams`, or a debug-level dump. Nothing
  mechanical prevents it; the invariant above and the mutant bound to `the key-only param encoding`
  are what a reviewer has to look at. Stated rather than mitigated, because a grep for "does any
  function take a value" is defeated by a rename.
- **Sorting is invisible in a single-key fixture.** Every ordering test uses at least three keys
  whose insertion order differs from their sorted order.
- **`slog`'s default handler writes to stderr, not stdout.** The router's existing startup lines
  use stdout; splitting them across two streams would make a deployment that captures only one lose
  half. `Options.Out` defaults to `os.Stdout` explicitly for that reason, and it is asserted.

## Stop Condition

Stop and ask if the operator needs param values in logs for debugging. That would reverse this
task's central decision, and the answer is a redaction policy — which values, redacted how — not an
adjustment to this code.

## Out of Scope

- The request middleware — T3.
- Transition logging inside `router.Service` and the CLI flags — T4.
- Log shipping, rotation and retention (permanent: boundary: retention is valid for a deployment,
  and this repository does not know the deployment).
- Audit logging of admin actions as a separate tamper-evident stream (deferred:
  `docs/adr/BACKLOG.md`).

## Verification Log
- 2026-09-15 · 7422213* · exit 0 · `set -o pipefail …` · acceptance-sha256:d64a89ce0e22ba3f673e7c5e34a3748beb806de1828f281029dee3e87211325f · ms:1582
- 2026-09-15 · 7422213* · exit 0 · `set -o pipefail …` · acceptance-sha256:d64a89ce0e22ba3f673e7c5e34a3748beb806de1828f281029dee3e87211325f · ms:801
- 2026-09-15 · 7422213* · exit 0 · `set -o pipefail …` · acceptance-sha256:d64a89ce0e22ba3f673e7c5e34a3748beb806de1828f281029dee3e87211325f · ms:805
