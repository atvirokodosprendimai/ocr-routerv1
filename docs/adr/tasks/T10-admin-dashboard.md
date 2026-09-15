# Task ADR-0001-T10: Give the admin a live dashboard over templ and datastar

**Depends-on:** T8
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `web.Mount(r chi.Router, deps)` — the `/admin` subtree and its SSE stream
**Consumes:** `cmd/router` composition root (T8), `httpapi` deps (T7), `router.Service` (T6), `identity.Service` (T3), `store.Repo` (T2)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the rendered markup`, `the admin role gate`, `the signal patch`

## Goal

Let an admin create customers, mint and revoke tokens, set credits, buffer limit, priority,
job TTL and per-service rates, and watch jobs, workers and queue depth update live without
a refresh.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/web/web.go` | add | the `/admin` route table, page loads, actions and the dashboard SSE stream |
| `internal/web/views/layout.templ` | add | shell, layout-level signals only, and the datastar rules that bind every view |
| `internal/web/views/views.templ` | add | overview, jobs, workers, users and services — pages and live fragments |
| `internal/web/views/model.go` | add | `Dashboard`, the read model every view is a pure function of |
| `internal/web/views/css.go` | add | the stylesheet as a Go constant, because templ forbids a variable inside `<style>` at COMPILE time |
| `internal/web/handlers_test.go` | add | role-gate, action and stream tests |
| `internal/web/views/views_test.go` | add | the markup rules, beside the views they govern |
| `cmd/router/wire.go` | edit | **mount the `/admin` subtree behind the API's own authenticator** — the line that selects all of it |
| `cmd/router/main_test.go` | edit | `TestAdminSubtreeIsMounted` |
| `README.md` | edit | dashboard URL and first-login instructions |

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write
   `handlers_test.go` asserting that a client token gets 403 on every `/admin` path, before
   any implementation (TDD red). [proof: acceptance]
2. [S2] `web.Mount` registers the subtree behind the **same** auth middleware as the API,
   plus `RequireRole(core.RoleAdmin)`. The dashboard is not a second auth system.
3. [S3] Views are `templ`, rendered server-side. Run `templ generate` after every `.templ`
   edit and **never hand-edit a `*_templ.go`**. Note that plain Go written inside a `.templ`
   file is compiled from the generated file, so keep logic in `handlers.go` where it can be
   tested and mutated. [proof: acceptance]
4. [S4] **datastar v1 attribute discipline**, from the team's cached reference — each of
   these is a silent failure, not an error:
   - `data-init="@get('/admin/stream')"` opens the stream. **`data-on-load` does not exist.**
   - DOM events take a colon: `data-on:click="@post('/admin/users')"`. The six hyphenated
     `data-on-*` attributes are separate things and are not used here.
   - **No `<form>` tags.** Each input is `data-bind:<kebab-name>`.
   - ⚠ **Kebab in the attribute, camelCase only inside an object value.**
     `data-bind:newUserEmail` silently binds `newuseremail`. Every binding in this task uses
     kebab (`data-bind:new-user-email`).
   - Signals are global and ship on **every** action, so layout holds only layout signals
     and each page declares its own; `_`-prefixed signals stay client-side.
5. [S5] ⚠ **Never interpolate stored text into a compiled `data-*` attribute.** A customer
   email or a job's error text containing `@word(` breaks the datastar expression compiler
   and **every later attribute on that element silently stops running**. Stored text is
   rendered as a **text node** by templ, or sent as a **server signal patch**
   (`sse.MarshalAndPatchSignals`) before the element patch — never into `data-signals`.
6. [S6] The dashboard SSE stream subscribes to an `admin` bus topic, clears its write
   deadline exactly as the API stream does, pings, and re-renders the jobs and workers
   fragments by id on each event. Each fragment's root carries a stable `id`, which is the
   morph selector.
7. [S7] Actions are `@post`/`@patch` returning `200` with an HTML fragment — never a 4xx for
   a validation failure. A duplicate email re-renders the form with an inline error.
8. [S8] Mint-token flow: the plaintext is shown **once**, in the fragment returned by the
   mint action, with a copy affordance and an explicit "this will not be shown again".
   It is never re-rendered on a later page load, because the router does not have it.
9. [S9] Per-service rate editing writes `service_rates`; the live-label list is read from
   the worker registry. The two are shown side by side so an admin can see a label that has
   workers but no explicit rate (charging the default of 1).
10. [S10] Accessibility and responsiveness are part of done: semantic landmarks, a real
    `<label>` per input, visible focus, ≥44px touch targets, one column below 861px, and
    designed loading (`data-indicator:fetching` + `data-show`), empty and error states.

## Acceptance

```bash
set -o pipefail
templ generate \
  && go build ./... \
  && go test ./internal/web/... -count=1 -race 2>&1 | tee /tmp/adr1-t10.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t10.out \
  && go test ./cmd/router/... -count=1 2>&1 | tee /tmp/adr1-t10r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t10r.out
```

`templ generate` is **inside** the fence: the generated files are what compile, so a fence
that skipped it could pass against stale generated code. Red at authoring: `internal/web`
does not exist.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestAdminRoutesRejectNonAdmin` | `internal/web/handlers_test.go` | client and worker tokens get 403, and no token gets 401, on every `/admin` path | — | S2 |
| `TestAdminPagesRenderForAdmin` | `internal/web/handlers_test.go` | all three pages render the layout for an admin | — | S2 |
| `TestPrincipalReachesTheDashboard` | `internal/web/handlers_test.go` | an ADMIN token is not forbidden — the guard for using ONE context key across two packages, whose failure mode is "the admin is always forbidden" with nothing naming the cause | — | S2 |
| `TestAdminSubtreeIsMounted` | `cmd/router/main_test.go` | `/admin` resolves through the binary's own `buildApp` — every test in `internal/web` mounts the subtree itself and would survive deleting the mount line | — | S2 |
| `TestCreateUserRoundTrip` | `internal/web/handlers_test.go` | the create action creates the user and patches the user table back | — | S7 |
| `TestCreateUserDuplicateShowsInlineError` | `internal/web/handlers_test.go` | a duplicate email is **200** with an explanatory fragment — a 4xx carries nothing for datastar to morph, so the page would silently show nothing | — | S7 |
| `TestMintTokenShowsPlaintextOnce` | `internal/web/handlers_test.go` | the mint fragment carries the token and a later page load does not | — | S8 |
| `TestRateEditPersistsAndChangesTheCharge` | `internal/web/handlers_test.go` | setting a rate of 4 then delivering a 2-unit job charges 8 — the edit must reach the MONEY, not just the row | — | S9 |
| `TestRateRejectsBadInput` | `internal/web/handlers_test.go` | empty label, negative and non-numeric rates each answer 200 with a visible error | — | S7, S9 |
| `TestAdminStreamPushesOnEvent` | `internal/web/handlers_test.go` | the stream pushes on connect and again when the admin topic fires | — | S6 |
| `TestAdminStreamClearsWriteDeadline` | `internal/web/handlers_test.go` | against a server with a **150ms `WriteTimeout`**, the stream still delivers 400ms later — run against a server that HAS a WriteTimeout so it cannot pass vacuously | — | S6 |
| `TestNoFormTags` | `internal/web/views/views_test.go` | no rendered view contains a `<form>` | — | S4 |
| `TestUsesDataInitNotOnLoad` | `internal/web/views/views_test.go` | no view uses `data-on-load` (which does not exist in v1 and fails silently), and the stream opens with `data-init` | — | S4 |
| `TestDomEventsUseColon` | `internal/web/views/views_test.go` | no hyphenated DOM-event binding appears; the six legitimate `data-on-*` attributes are allow-listed by name | — | S4 |
| `TestBindingsAreKebabCase` | `internal/web/views/views_test.go` | every signal-path attribute suffix is lower-case — the casing trap binds a DIFFERENT signal with nothing reporting it | — | S4 |
| `TestStoredTextIsNotInCompiledAttributes` | `internal/web/views/views_test.go` | a job error of `Call @Anna(invoices)` and an email of `a@b(c).com` render as TEXT NODES and appear in no `data-*` attribute value — and the test first asserts the text rendered at all, so the negative cannot pass vacuously | — | S5 |
| `TestLiveFragmentsHaveStableIDs` | `internal/web/views/views_test.go` | each live fragment carries its morph id AND that id exists in the first paint — a fragment cannot be patched into existence | — | S6 |
| `TestEveryInputHasALabel` | `internal/web/views/views_test.go` | every input and select has an associated `<label for>` | — | S10 |
| `TestEmptyStatesExist` | `internal/web/views/views_test.go` | the jobs, workers and user tables each render an empty state — a blank table is indistinguishable from a broken page | — | S10 |
| `TestLoadingStateExists` | `internal/web/views/views_test.go` | the create action has a `data-indicator` and something consumes it with `data-show` | — | S10 |
| `TestWorkersViewSurfacesQueueWithNoWorker` | `internal/web/views/views_test.go` | a label with 5 queued jobs and zero workers is called out by name and count — precisely the silent failure an operator needs to see | — | S9 |
| `TestTokenIsPresentedAsShownOnce` | `internal/web/views/views_test.go` | the mint fragment tells the operator the token is shown once, or they navigate away and lose it | — | S8 |
## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the seventeen tests above |
| 2 — something selects it | `cmd/router/wire.go` calls `web.Mount`; `TestAdminSubtreeIsMounted` goes red when that line is removed. Mounted is not constructed — the dashboard shares the API's constructed deps, so the end-to-end test exercising both is what covers the second selection. |
| 3 — the caller can discover it | the README names the dashboard URL and the bootstrap token flow; `--help` does not mention it, which is fine because it is not a flag |
| 4 — it is used | human sign-off below — browser verification with two tabs open |

## Mutation Log

- 2026-09-15 · a5bff80* · mutant killed · exit 1 · `internal/web/views/views.templ` · data-on-load does not exist in datastar v1: the page never subscribes, with no console error and nothing in the network tab. · acceptance-sha256:f3ae36b8997cb342abe5343000e09095eee051e1839c44e9438a817e57db0edb · covers:the rendered markup
- 2026-09-15 · a5bff80* · mutant killed · exit 1 · `internal/web/web.go` · Without the gate any authenticated client could create users, mint tokens and change pricing. · acceptance-sha256:f3ae36b8997cb342abe5343000e09095eee051e1839c44e9438a817e57db0edb · covers:the admin role gate

## Invariants

- The dashboard uses the **same** authentication as the API, gated to `admin`.
- No `<form>` tags; every input is `data-bind:` with a kebab-case suffix.
- Stored or user-supplied text never reaches a compiled `data-*` attribute value.
- Every action returns `200` with an HTML fragment.
- A minted token's plaintext is rendered exactly once and never persisted in a view.
- `*_templ.go` is generated, never hand-edited.

## Risks

- **The datastar failures in this task are all silent** — `data-on-load`, a camelCase bind,
  and `@word(` in a compiled attribute each produce no console error and no failed request;
  the page simply does nothing. That is why S4 and S5 have *mechanical* markup tests rather
  than relying on review, and why the browser sign-off below is not optional.
- ⚠ **A markup assertion checks the spelling of your intent, never the runtime's reading of
  it.** The team has a measured case where four passing markup tests shipped a checkbox that
  rendered pre-ticked, because they asserted the expression *string* rather than what it
  evaluates to. These tests bound the class of error they can catch — hence S10's browser
  step, which is where evaluation is actually observed.
- **Checkbox-to-array binding is positional**: `$foo.length` is the number of boxes, fixed
  at first paint, never the number selected. If any multi-select appears in this dashboard,
  count with `filter(Boolean)`. Flagged here because the natural reading is the wrong one.
- **The Go SDK version is not cached and must be checked**, unlike the attribute surface.
  `PatchElementTempl` / `PatchSignals` are the current names; the templ docs still show the
  retired `MergeFragmentTempl` / `MarshalAndMergeSignals`.

## Stop Condition

Stop and ask if the operator wants the dashboard to expose OCR **results** — it currently
shows metadata only. Results are customer content and the ADR deletes them on delivery;
showing them would make the admin a second reader of data the design says is transient.

## Out of Scope

- A customer-facing portal (permanent: boundary: the operator specified an admin dashboard only; customers use the API).
- Charts and historical analytics (deferred: docs/adr/BACKLOG.md).
- Editing a job's pipeline after upload (permanent: boundary: a pipeline is fixed at upload; changing it mid-flight has no defined semantics for already-accrued cost).

## Verification Log

<!-- Human sign-off required for S10, recorded via `adr-verify --human`: the dashboard
     opened in a browser with two tabs, a job driven through its states in one tab and
     observed updating live in the other, at both mobile and desktop widths. -->
- 2026-09-15 · a5bff80* · exit 1 · `set -o pipefail …` · acceptance-sha256:f3ae36b8997cb342abe5343000e09095eee051e1839c44e9438a817e57db0edb · ms:5087
  ```
  --- last 2 line(s) of stdout
  ok  	github.com/atvirokodosprendimai/ocr-router/internal/web	2.525s
  ?   	github.com/atvirokodosprendimai/ocr-router/internal/web/views	[no test files]
  --- last 2 line(s) of stderr
  (✓) Post-generation event received, processing... [ updates=0 needsRestart=true needsBrowserReload=true ]
  (✓) Complete [ updates=0 duration=16.490875ms ]
  ```
- 2026-09-15 · a5bff80* · exit 1 · `set -o pipefail …` · acceptance-sha256:f3ae36b8997cb342abe5343000e09095eee051e1839c44e9438a817e57db0edb · ms:3904
  ```
  --- last 2 line(s) of stdout
  ok  	github.com/atvirokodosprendimai/ocr-router/internal/web	2.503s
  ?   	github.com/atvirokodosprendimai/ocr-router/internal/web/views	[no test files]
  --- last 2 line(s) of stderr
  (✓) Post-generation event received, processing... [ updates=0 needsRestart=true needsBrowserReload=true ]
  (✓) Complete [ updates=0 duration=15.57725ms ]
  ```
- 2026-09-15 · a5bff80* · exit 0 · `set -o pipefail …` · acceptance-sha256:f3ae36b8997cb342abe5343000e09095eee051e1839c44e9438a817e57db0edb · ms:6236
- 2026-09-15 · a5bff80* · exit 0 · `set -o pipefail …` · acceptance-sha256:f3ae36b8997cb342abe5343000e09095eee051e1839c44e9438a817e57db0edb · ms:5269
- 2026-09-15 · a5bff80* · exit 0 · `set -o pipefail …` · acceptance-sha256:f3ae36b8997cb342abe5343000e09095eee051e1839c44e9438a817e57db0edb · ms:5376
- 2026-09-15 · a5bff80* · exit 0 · `set -o pipefail …` · acceptance-sha256:f3ae36b8997cb342abe5343000e09095eee051e1839c44e9438a817e57db0edb · ms:6772
