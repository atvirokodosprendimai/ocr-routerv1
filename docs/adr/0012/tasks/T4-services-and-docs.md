# Task ADR-0012-T4: `Services`, and document both shapes so neither rots

**Depends-on:** T3
**Covers:** none — no spec
**Estimated scope:** S (few files)
**Owner:** unassigned
**Produces:** `client.Services()`, `client.ErrNoServices`, a runnable `ExampleOpen`, the README's Session section, and ADR-0012 added to the `Enforced-by:` pointer check
**Consumes:** `client.Session` + `client.Open()` (T1), `(*Session).Upload()` + `(*Session).Collect()` (T2), `Submit` re-implemented over `Session` (T3)
**Data dependency:** hermetic — an `httptest.Server` serving `/services`; the doc checks read tracked files
**Proof map:** v1
**Rests-on:** `Services returning the router's labels`, `a router without /services being named rather than erroring opaquely`, `the README naming only symbols that exist`, `the hand-written surface list being extended so the README check can pass`, `ExampleOpen running rather than merely compiling`, `ADR-0012's own Enforced-by pointer resolving`

## Goal

Add the one remaining protocol call the motivating consumer needs — asking a router which services it
serves, so a caller can fail early on a label nothing serves instead of after an upload — and make both
entry points discoverable, with checks that actually fail when the documentation drifts.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `client/services.go` | add | `Services(ctx, Config) ([]string, error)` and `ErrNoServices`. Its own file: it is the one call that needs no stream, so putting it in `session.go` would imply a dependency that does not exist. |
| `client/services_test.go` | add | The two `Services` tests. |
| `client/example_test.go` | edit | Add `ExampleOpen` **with an `// Output:` comment**. ⚠ The file today declares `Example`, `ExampleSubmit_progress` and `ExampleInput_params` — there is no `ExampleSubmit` — and NONE of them carries an `// Output:` comment, so `go test` compiles them and runs none. An example without one is not a test. |
| `README.md` | edit | A `### Many jobs on one stream` subsection under the existing `## The client`, showing `Open`/`Upload`/`Events`/`Collect` and saying plainly when to prefer it over `Submit`. |
| `client/readme_test.go` | edit | ⚠ **Extend the hand-written `exported` list at `client/readme_test.go:28`** — it is "the package's public surface, written out rather than reflected", so the moment the README names `client.Open` the EXISTING `TestReadmeClientExampleNamesRealSymbols` goes red until every new symbol is added. That is a required edit, not an optional one, and it is why this task touches the file at all. |
| `client/public_test.go` | edit | Add ADR-0012 to `TestEnforcedByPointersResolve`'s hardcoded record list (`client/public_test.go:121-128`), which today covers ADR-0005/0006/0011 only — so this record's own `Enforced-by:` pointer is checked by nothing until it is added. |

## Ordered Steps

1. [S1] Write the failing checks FIRST and confirm the red. `TestServicesReturnsLabels` and
   `TestServicesNoEndpointIsNamed` do not compile (no `Services`), which — Go compiling a test package
   as a unit — makes the whole `client_test` package unbuildable; that build failure IS the red for this
   step, and per-test verdicts arrive in S5. Separately and BEFORE that, run the EXISTING
   `TestReadmeClientExampleNamesRealSymbols` and `TestEnforcedByPointersResolve` and record that they
   are GREEN, so their going red in S3/S4 is attributable to this task's doc changes rather than to
   something already broken.
2. [S2] Add `Services` and `ErrNoServices`: GET `/services`, decode `{"labels":[…]}` (the shape
   `internal/httpapi/claim.go:94-106` actually returns, routed at `internal/httpapi/api.go:157`), and
   map a 404 to `ErrNoServices` so "this router is too old to tell you" is distinguishable from "this
   router has no services" and from a transport failure. Reuse `classify` for every other non-200.
   [proof: acceptance]
3. [S3] Add `ExampleOpen` to `client/example_test.go` **with an `// Output:` comment**, so `go test`
   RUNS it: open a session against a stub, upload two documents, persist their ids, range over
   `Events()` and collect each, printing a deterministic line the comment asserts. Keep it short enough
   to read in `go doc` and complete enough that copying it yields working code — an example that omits
   `defer s.Close()` teaches the leak ADR-0012 §Consequences names. [proof: acceptance]
4. [S4] Write the README subsection, then extend `readme_test.go`'s `exported` list with every symbol it
   names — run the test between the two edits and confirm it goes RED on the README alone, which is the
   proof the check is live rather than vacuous. ⚠ Say WHEN to use which: `Submit` for one document in
   one process, `Session` for many documents, for concurrency, or when the job id must outlive the
   process. A reader who cannot tell which to pick will pick `Submit` and hit the N-connections problem
   this record was written to avoid. [proof: acceptance]
5. [S5] [proof: acceptance] Add ADR-0012 to `TestEnforcedByPointersResolve`'s list and confirm it
   resolves `client/session_test.go::TestOpenReturnsOnlyAfterHelloAndBacklog` — which exists from T1, so
   this is checking the POINTER, not the test. Then run the whole tree's gate under `-race`.
6. [S6] [proof: mutation] Rename a symbol the README's Session section names (e.g. `Open` → `Opn` in the
   prose) and confirm `TestReadmeClientExampleNamesRealSymbols` goes red. The mechanism under test is
   the DOC-ROT TRIPWIRE, not `Services`, because doc rot is the failure this task exists to prevent and
   the one that stays green by default.

## Acceptance

```bash
set -o pipefail
go test ./client/... -count=1 -race -v \
  -run 'TestServicesReturnsLabels|TestServicesNoEndpointIsNamed|TestReadmeClientExampleNamesRealSymbols|TestEnforcedByPointersResolve|ExampleOpen' \
  2>&1 | tee /tmp/acc-0012-T4a.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/acc-0012-T4a.out \
  && grep -q -- "--- PASS: ExampleOpen" /tmp/acc-0012-T4a.out \
  && templ generate && go build ./... && go vet ./... && test -z "$(gofmt -l .)" \
  && go test ./... -count=1 -race 2>&1 | tee /tmp/acc-0012-T4b.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0012-T4b.out
```

<⚠ **The `grep -q -- "--- PASS: ExampleOpen"` segment is what makes the example part of the verdict**,
and an earlier draft got this wrong: naming an `Example` in `-run` proves nothing, because an example
with no `// Output:` comment is compiled and NOT run, so `-run 'ExampleOpen'` selects nothing and the
`no tests to run` guard is satisfied by the sibling `TestServices*` matches. With the `// Output:`
comment from S3 the example runs and `-v` prints that line. The final leg is the whole tree rather than
two packages, because this is the record's last task and the point at which the full project gate
should be green.>

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestServicesReturnsLabels` | `client/services_test.go` | A 200 from `/services` yields the labels the router listed, and an empty list yields an empty slice rather than an error — a router with no worker connected is a real answer, not a fault. | — | S1, S2 |
| `TestServicesNoEndpointIsNamed` | `client/services_test.go` | A 404 returns exactly `ErrNoServices` under `errors.Is`, distinguishably from a transport error and from an empty list. Three outcomes a single `err != nil` would flatten into one. | — | S1, S2 |
| `TestReadmeClientExampleNamesRealSymbols` | `client/readme_test.go` | **Pre-existing, and this task makes it exercise new ground**: every `client.`-qualified symbol in the README — now including the Session subsection — is in the package's hand-written surface list. The doc-rot tripwire, and the mechanism S6 mutates. Listed despite being pre-existing because S4 deliberately drives it red and back to green, so it is a verdict here rather than a bystander. | — | S1, S4, S6 |
| `TestEnforcedByPointersResolve` | `client/public_test.go` | **Pre-existing, extended here**: after S5 its record list includes ADR-0012, so this record's `Enforced-by:` pointer is checked to name a file that exists and a function that file declares. Red between S5's list edit and T1's test existing — and T1 is a dependency, so it is green by the time it runs. | — | S1, S5 |
| `ExampleOpen` | `client/example_test.go` | The Session shape compiles AND RUNS end to end against a stub: open, two uploads, ids retained, events ranged, results collected, session closed, with an `// Output:` comment making it a real test. | — | S1, S3 |

⚠ `Example`, `ExampleSubmit_progress` and `ExampleInput_params` already exist (ADR-0011-T2) and are not
listed: they are pre-existing, and none of them runs, so none can carry a verdict. Bringing them under
`// Output:` is deliberately not this task's job — see Out of Scope.

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | The five checks above; three are new and two are pre-existing checks this task drives red and back to green. |
| 2 — something selects it | `Services` is selected by its own tests only — stated plainly; its production caller is the crawler's early-label check, in another repository. `ExampleOpen` IS a real selector for the Session shape once it has an `// Output:` comment: `go test` compiles AND runs it. |
| 3 — the caller can discover it | ★ **This is the task that closes rung 3 for the record.** `go doc ./client` renders both shapes' examples, and the README says which to pick and why. `TestReadmeClientExampleNamesRealSymbols` is what keeps that true. |
| 4 — it is used | Still nothing measures this, and this repository cannot (ADR-0012 §Consequences). The follow-up in ADR-0012 tracks the crawler port. |

## Mutation Log

<Tool-written by `adr-verify --mutant` at execution time. Empty at authoring.>

## Invariants

- **`client/client_test.go` and `client/raw_test.go` stay unedited**, as in every task of this record.
- **The three pre-existing `Example` functions are not renamed or rewritten.** ADR-0011-T2 authored
  them; this task adds a sibling.
- **`Services` opens no stream and needs no `Session`.** It takes `Config` directly, so a caller can ask
  what a router serves before deciding whether to connect at all.
- **The README's existing `Submit` section is not rewritten**, only followed by a new subsection — a
  reader who already knows `Submit` finds it where it was.
- **Every example closes what it opens.** `defer s.Close()` is in `ExampleOpen`, because an example is
  copied verbatim more often than it is read carefully.
- **`readme_test.go`'s `exported` list is extended, never reflected.** It is hand-written on purpose;
  replacing it with reflection would make it pass for symbols the README invented.
- `client/` still has no flags, no terminal output and no exit codes (ADR-0005's seam).

## Risks

- The README prose drifts from the API — the exact failure `TestReadmeClientExampleNamesRealSymbols`
  exists for, and S6 proves that check can fail. Bounded honestly: it verifies symbols are NAMED and
  in the surface list, not that the surrounding sentences are still true, and no cheap check covers prose.
- **The hand-written surface list is itself a rot surface**: a future symbol added to the package and to
  the README but not to the list fails the test with a message that reads like a README error. Accepted —
  it is the existing design and changing it is out of scope — but worth the one-line comment at the list.
- `ExampleOpen` becomes long enough that nobody reads it, so it documents nothing. Kept to the two-job
  case deliberately; the concurrency story is prose, not code, because an example with a `WaitGroup` in
  it teaches goroutine plumbing rather than the protocol.
- **`ExampleOpen`'s `// Output:` makes it order-dependent.** Collecting from a map or ranging events
  without ordering gives a nondeterministic line and a flaky example. Sort before printing, or print one
  aggregate line.
- `Services` is added with no in-repo caller — the "component with tests and no caller" shape this
  pipeline calls its most common shipped defect. Accepted and named: it is a protocol call the motivating
  external consumer already makes (`internal/ocr.Services`), and leaving it out forces that consumer to
  keep a hand-written copy of one HTTP GET, which is the duplication this record removes. Rung 2 says so
  plainly rather than claiming a selector it does not have.
- `/services` may not exist on an older deployed router, and a caller that treats the error as fatal
  breaks against it — which is why `ErrNoServices` is a named sentinel rather than a generic error.

## Stop Condition

Stop and ask if writing the README subsection reveals that a reader genuinely cannot tell which entry
point to use without also understanding SSE — that would mean the API needs a better default rather than
better prose, which is a change to T1's shape and not a documentation task.

Stop and ask if `ExampleOpen` cannot be made deterministic enough for an `// Output:` comment without
distorting what it teaches — a runnable-but-misleading example is worse than a compiled-only one, and
the trade is worth naming rather than resolving silently.

<T4's earlier Stop Condition about `/services` possibly not existing is retired: it was already
answered from source before authoring — `internal/httpapi/api.go:157` routes it and
`internal/httpapi/claim.go:94-106` returns `{"labels":[…]}` sorted.>

## Out of Scope

- Bringing the three pre-existing `Example` functions under `// Output:` comments so they run
  (deferred: `docs/adr/BACKLOG.md`).
- Replacing `readme_test.go`'s hand-written surface list with reflection (permanent: boundary: the list
  is deliberately hand-written so the check fails closed on a symbol nobody vetted; reflection would make
  it pass for anything the package happens to export).
- Reconnection, and documenting a reconnect recipe in the README (deferred: `docs/adr/BACKLOG.md`).
- A `client.New(cfg) *Client` handle (deferred: `docs/adr/BACKLOG.md`).
- Porting `e-tar-crawlerv1` onto the package, which is what would give `Services` and `Session` real
  callers (external: `github.com/teisora/e-tar-crawlerv1`: `internal/ocr`).
- Documenting the router's HTTP + SSE API itself (permanent: boundary: `README.md` § The API already
  owns the wire contract, and this task documents the Go package that speaks it — duplicating the wire
  description here would create a second thing to keep in step with the router).
- Caching `Services`' answer (permanent: boundary: label validity is derived from live worker
  connections and changes when a worker starts or stops, so a cache in the client library would hand a
  caller a stale answer with no way to know it; a caller that wants one knows its own tolerance).

## Verification Log

<Tool-written by `adr-verify` at execution time. Empty at authoring.>
