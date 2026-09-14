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
| `internal/web/mount.go` | add | **the `/admin` route table** — what makes every view reachable |
| `internal/web/handlers.go` | add | page loads, actions, the dashboard SSE stream |
| `internal/web/views/layout.templ` | add | shell, layout-level signals only |
| `internal/web/views/users.templ` | add | user list, create form, per-user settings |
| `internal/web/views/jobs.templ` | add | live job table, queue depth per label |
| `internal/web/views/workers.templ` | add | connected workers and the labels they serve |
| `internal/web/views/services.templ` | add | live labels and their admin-owned rates |
| `internal/web/*_test.go` | add | render, role-gate and round-trip tests |
| `cmd/router/wire.go` | edit | **mount the `/admin` subtree** — the line that selects all of it |
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
| `TestAdminRoutesRejectNonAdmin` | `internal/web/handlers_test.go` | client and worker tokens get 403 on every mounted `/admin` path, driven from the route table | — | S2 |
| `TestAdminSubtreeIsMounted` | `cmd/router/main_test.go` | `/admin` resolves through the binary's own `buildHandler` — goes red if the `web.Mount` line is deleted | — | S2 |
| `TestCreateUserRoundTrip` | `internal/web/handlers_test.go` | posting the create action creates the user and returns a fragment listing them | — | S7 |
| `TestCreateUserDuplicateShowsInlineError` | `internal/web/handlers_test.go` | a duplicate email returns **200** with an error fragment, not a 4xx | — | S7 |
| `TestMintTokenShowsPlaintextOnce` | `internal/web/handlers_test.go` | the mint fragment contains the token and a later page load does not | — | S8 |
| `TestNoFormTags` | `internal/web/views_test.go` | no rendered admin view contains a `<form>` element | — | S4 |
| `TestBindingsAreKebabCase` | `internal/web/views_test.go` | every `data-bind:` / `data-signals:` suffix matches `^[a-z0-9-]+$` — catches the casing trap that binds a different signal with no error anywhere | — | S4 |
| `TestUsesDataInitNotOnLoad` | `internal/web/views_test.go` | no view contains `data-on-load`, and the stream is opened with `data-init` | — | S4 |
| `TestDomEventsUseColon` | `internal/web/views_test.go` | no `data-on-click`-style hyphenated DOM event appears; the six legitimate hyphenated attributes are allow-listed by name | — | S4 |
| `TestStoredTextIsNotInCompiledAttributes` | `internal/web/views_test.go` | rendering a user whose email is `a@b(c).com` and a job whose error is `@fail(x)` puts that text in a **text node** and in no `data-*` attribute value — the silent-breakage guard | — | S5 |
| `TestJobsFragmentHasStableID` | `internal/web/views_test.go` | each live fragment root carries the id the SSE patch targets | — | S6 |
| `TestAdminStreamClearsWriteDeadline` | `internal/web/handlers_test.go` | against a server with a 100ms `WriteTimeout`, the admin stream still delivers after 300ms | — | S6 |
| `TestAdminStreamRerendersOnEvent` | `internal/web/handlers_test.go` | a job state change publishes to the `admin` topic and the stream emits a patched jobs fragment | — | S6 |
| `TestRateEditPersists` | `internal/web/handlers_test.go` | editing a rate writes `service_rates` and a later delivery charges at the new rate | — | S9 |
| `TestServicesShowsLabelWithoutExplicitRate` | `internal/web/handlers_test.go` | a live label with no `service_rates` row is listed as charging the default 1, rather than omitted | — | S9 |
| `TestEveryInputHasALabel` | `internal/web/views_test.go` | every rendered `<input>` has an associated `<label>` — the one accessibility property that is cheap to assert mechanically | — | S10 |
| `TestLoadingAndEmptyStatesExist` | `internal/web/views_test.go` | the jobs view with zero jobs renders an empty state, and the action markup carries `data-indicator` | — | S10 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the seventeen tests above |
| 2 — something selects it | `cmd/router/wire.go` calls `web.Mount`; `TestAdminSubtreeIsMounted` goes red when that line is removed. Mounted is not constructed — the dashboard shares the API's constructed deps, so the end-to-end test exercising both is what covers the second selection. |
| 3 — the caller can discover it | the README names the dashboard URL and the bootstrap token flow; `--help` does not mention it, which is fine because it is not a flag |
| 4 — it is used | human sign-off below — browser verification with two tabs open |

## Mutation Log

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
