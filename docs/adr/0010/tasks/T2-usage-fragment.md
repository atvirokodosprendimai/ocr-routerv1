# Task ADR-0010-T2: Render the counters as their own live fragment, and make the Customers page live

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** `views.Dashboard.Usage`, the `#usage` fragment
**Consumes:** `core.Usage`, `Repo.UsageByUser(ctx, now)` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the read model carrying usage`, `the fragment's own id`, `the Customers page subscription`, `the user table NOT being patched`, `the per-number labelling`

## Goal

The Customers page shows each customer's four windows, updates them over the existing SSE stream, and
leaves the editable table alone while doing it.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/web/views/model.go` | edit | `Dashboard.Usage map[string]core.Usage` — the read model is the one source both the first paint and every patch read |
| `internal/web/web.go` | edit | `buildDashboard` calls `UsageByUser`; `stream`'s `push` patches `views.Usage` — this is what SELECTS T1's method, which is unreachable until this line exists |
| `internal/web/views/usage.templ` | add | the `#usage` fragment: one row per customer, four window columns |
| `internal/web/views/views.templ` | edit | the Customers page renders `@Usage(d)` and gains `data-init="@get('/admin/stream')"` |
| `internal/web/views/views_templ.go` | regenerate | `templ generate`; never hand-edited |
| `internal/web/views/assets/app.css` | edit | the usage table's compact cell |
| `internal/web/usage_test.go` | add | the rendering, the labelling, the subscription, and what must NOT be patched |

## Ordered Steps

1. [S1] Write the failing tests first: `/admin/users` renders a `#usage` section containing each
   customer's numbers, and carries a `data-init` subscription. Red — neither exists.
2. [S2] Add `Usage` to `views.Dashboard` and fill it in `buildDashboard`. ⚠ In `buildDashboard`, not in
   the handler: a field filled on the page-load path and not the patch path renders once and then
   blanks itself on the first SSE event.
3. [S3] Write `views.Usage(d)` with root id `usage`. ⚠ A fragment cannot be patched into existence —
   the id must be in the first paint — and sixteen numbers per customer has to stay legible narrow, so
   each window is ONE cell: the pushed count, then delivered / failed / expired beneath it, muted.
4. [S4] Label every number for a reader who cannot see the column group. Each cell carries a `title`
   naming the window and the four values in words; the counts are not bare digits whose meaning lives
   only in a header two rows up. [proof: human: an operator reads one cell and can say which window and which bucket each number belongs to, without scrolling to the header]
5. [S5] Render `@Usage(d)` on the Customers page and add the `data-init` subscription it has never
   had — the page is not live today, so the counters would otherwise be live in a stream nobody joined.
6. [S6] Patch `views.Usage` from `stream`'s `push`, beside `Stats`, `Jobs` and `Workers`.
   ⚠ **DO NOT PATCH `#user-table`.** That fragment carries every row's number inputs, and re-rendering
   it every fifteen seconds would land under an operator who is typing in one — see the ADR's decision
   3 and the stale-signal defect it names.

## Acceptance

```bash
set -o pipefail
templ generate && go test ./internal/web/ -run 'Usage|Stream' -count=1 2>&1 | tee /tmp/adr10t2.out && \
  ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/adr10t2.out && \
  go test ./internal/web/... ./cmd/router/ -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestUsageSectionRendersEveryCustomer` | `internal/web/usage_test.go` | the fragment exists with id `usage` and carries each customer's pushed count — red if the read model is filled on the page path only | — | S1, S2, S3 |
| `TestUsageNumbersAreLabelledNotBare` | `internal/web/usage_test.go` | each cell names its window and its four buckets in words, so a number is never a digit whose meaning is two rows away | — | S4 |
| `TestTheCustomersPageSubscribesToTheStream` | `internal/web/usage_test.go` | the Customers page carries `data-init` with the stream URL — without it every counter is live in a stream nobody joined | — | S5 |
| `TestTheStreamPatchesUsage` | `internal/web/usage_test.go` | a stream connection receives a `usage` fragment, so "live" is observed rather than asserted | — | S6 |
| `TestTheStreamDoesNotPatchTheUserTable` | `internal/web/usage_test.go` | ⚠ the NEGATIVE that protects an operator mid-edit: no patch names `user-table`. Red for the obvious implementation, which is to patch the table the counters are about | — | S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestUsageSectionRendersEveryCustomer` |
| 2 — something selects it | `buildDashboard`'s call to `UsageByUser` and `push`'s call to `views.Usage` — this task is what discharges T1's rung-2 debt; the mutation deletes the `push` line and `TestTheStreamPatchesUsage` must go red |
| 3 — the caller can discover it | the section is on the page the operator already opens, and its heading names the windows |
| 4 — it is used | nothing measures whether the numbers are read; ADR-0002's request log shows the page being loaded |

## Mutation Log

- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/web.go` · the usage fragment is never pushed, so the counters are rendered once and never update — the liveness M asked for, silently absent · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · covers:the fragment's own id
- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/web.go` · the stream patches the EDITABLE table instead — the obvious implementation, which re-renders every row edit input every fifteen seconds under an operator who may be typing in one · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · covers:the user table NOT being patched
- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/views/views.templ` · the stream URL goes empty, so every data-init subscribes to nothing and the counters are live in a stream nobody joined. Mutated here rather than on the Customers data-init line, which is byte-identical to the Overview one and therefore not uniquely addressable · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · covers:the Customers page subscription
- 2026-09-28 · 08095e7* · mutant inconclusive · exit 1 · `internal/web/web.go` · the read model stops carrying the counts, so the section renders zeros for everyone — the shape of a field filled on one path and not the other · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · covers:the read model carrying usage
  ```
  the fence failed on a build/parse error, not an assertion
  ```
- 2026-09-28 · 08095e7* · mutant survived · exit 0 · `internal/web/views/usage.templ` · the breakdown is hidden, so each cell shows a bare pushed count and the delivered/failed/expired numbers are gone from the page · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · covers:the per-number labelling
  ```
  the fence passed with the mechanism broken; it may not materialize, compile, load, or assert on the changed path
  ```
- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/views/usage.templ` · probe: does the strengthened test see a broken detail span · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44
- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/views/usage.templ` · the breakdown becomes bare digits with no bucket names, which is exactly the unlabelled cell this record says sixteen numbers per row cannot afford · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · covers:the per-number labelling
- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/web.go` · the read model is filled with nothing, so every cell renders zeros while the data is in the table — the shape of a counter wired to the wrong source · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · covers:the read model carrying usage
- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/web.go` · the read model is filled with nothing, so every cell renders zeros while the data is in the table · acceptance-sha256:4dc7055fcc40db14ae24a36bcd495df05792df2149516f262f4a5adc96eb98b8 · covers:the read model carrying usage
- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/web.go` · the usage fragment is never pushed, so the counters render once and never update · acceptance-sha256:4dc7055fcc40db14ae24a36bcd495df05792df2149516f262f4a5adc96eb98b8 · covers:the fragment's own id
- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/views/views.templ` · the stream URL goes empty, so every data-init subscribes to nothing and the counters are live in a stream nobody joined · acceptance-sha256:4dc7055fcc40db14ae24a36bcd495df05792df2149516f262f4a5adc96eb98b8 · covers:the Customers page subscription
- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/web.go` · the stream patches the EDITABLE table instead — the obvious implementation, which re-renders every row edit input every fifteen seconds under an operator who may be typing in one · acceptance-sha256:4dc7055fcc40db14ae24a36bcd495df05792df2149516f262f4a5adc96eb98b8 · covers:the user table NOT being patched
- 2026-09-28 · 08095e7* · mutant killed · exit 1 · `internal/web/views/usage.templ` · the breakdown becomes bare digits with no bucket names — the unlabelled cell sixteen numbers per row cannot afford · acceptance-sha256:4dc7055fcc40db14ae24a36bcd495df05792df2149516f262f4a5adc96eb98b8 · covers:the per-number labelling

## Invariants

- `#user-table` is never patched by the stream.
- `Dashboard.Usage` is filled in `buildDashboard`, so the first paint and every patch agree.
- The `usage` id exists in the first paint.
- No new route: the counters ride the existing `/admin/stream`.

## Risks

- Sixteen numbers per customer is the readability risk, and a test can only pin the labelling, not the
  legibility. S4's proof is human for that reason and says what the person checks; the sign-off in the
  log records what was actually on screen.
- Making the Customers page live is a behaviour change to a page that has never updated itself. If an
  operator relied on it being static while editing, the separate fragment is what keeps that true for
  the part they edit.
- ★ **A MUTATION EXPOSED A WEAK TEST HERE, and the weakness was mine.** Hiding the breakdown span
  SURVIVED the first `TestUsageNumbersAreLabelledNotBare`, because that version grepped the WHOLE PAGE
  for "delivered", "failed" and "expired" — all three are job states and all three appear in the Stats
  cards, so the assertion passed on text this section never produced. It now reads only the `#usage`
  fragment and asserts the rendered SENTENCE (`0 delivered · 2 failed · 0 expired`), which nothing else
  on the page can satisfy. ⚠ The general question: which of a test's subjects could carry the verdict
  by itself?
- ⚠ **The fixture cannot produce a DELIVERED job, and the test says so rather than asserting a wrong
  number.** `Repo.CreateJob` ignores the `State` field and writes `queued` unconditionally, so
  `seedJobs`'s job constructed as `core.JobDelivered` is not one. The delivered bucket's semantics are
  T1's subject and are covered there against raw-seeded rows; this task's tests own the rendering.
- One mutation attempt came back INCONCLUSIVE (`Usage: nil`, which left `usage` unused and failed to
  compile — a skipped mutant wearing a kill's exit code). It was replaced with one that compiles and
  empties the map instead; both rows are in the log above.
- ⚠ **THE FENCE'S FILTER WAS WRONG AND adr-lint CAUGHT IT.** It read `-run 'Usage'`, which does not
  select `TestTheCustomersPageSubscribesToTheStream` or `TestTheStreamDoesNotPatchTheUserTable` — so
  the Tests table promised two tests the filtered segment never ran, and one of them is the NEGATIVE
  this whole decision rests on. They were still executed by the second, unfiltered segment, which is
  why nothing looked wrong. The filter is now `'Usage|Stream'`. ⚠ Changing it invalidated every pass
  and mutation row recorded under the old digest, so all of them were re-run; the superseded rows stay
  in the log above, bound to a digest that no longer matches, which is the mechanism working.

## Stop Condition

Stop if the counters cannot be rendered without patching `#user-table` — that would mean the row data
and the usage data are coupled in the view in a way the ADR assumed they are not, and the decision
needs revisiting rather than the invariant being dropped.

## Out of Scope

- Sorting or filtering the usage table. (deferred: `docs/adr/BACKLOG.md`)
- Charts. (deferred: `docs/adr/BACKLOG.md`)
- Fixing the stale-signal defect on the row inputs — this task avoids it, it does not repair it. (deferred: `docs/adr/BACKLOG.md`)

## Verification Log
- 2026-09-28 · 08095e7* · exit 1 · `set -o pipefail …` · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · ms:2454
  ```
  --- last 10 line(s) of stdout (of 14 after folding 14 raw)
      usage_test.go:44: the usage section never says "7d", so its numbers are bare digits
      usage_test.go:44: the usage section never says "pushed", so its numbers are bare digits
      usage_test.go:44: the usage section never says "delivered", so its numbers are bare digits
      usage_test.go:44: the usage section never says "failed", so its numbers are bare digits
      usage_test.go:44: the usage section never says "expired", so its numbers are bare digits
  --- FAIL: TestTheStreamPatchesUsage (0.07s)
      usage_test.go:74: the stream sent no usage fragment in its first push: "event: datastar-patch-elements\ndata: elements <section id=\"stats\"><h2>Now</h2><div class=\"cards\"><div class=\"card\"><div class=\"n\">1</div><div class=\"k\">queued</div></div><div class=\"card\"><div class=\"n\">0</div><div class=\"k\">processing</div></div><div class=\"card\"><div class=\"n\">0</div><div class=\"k\">done</div></div><div class=\"card\"><div class=\"n\">0</div><div class=\"k\">delivered</div></div><div c…"
  FAIL
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/web	0.612s
  FAIL
  --- last 2 line(s) of stderr
  (✓) Post-generation event received, processing... [ updates=0 needsRestart=true needsBrowserReload=true ]
  (✓) Complete [ updates=0 duration=28.604625ms ]
  ```
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · ms:6391
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · ms:4800
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · ms:7073
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · ms:6454
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · ms:5970
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · ms:5141
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · ms:4755
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:d5e43895ab7d11f992ff55bc5f5012514b3a893e1bc38475e0f48a0456808a44 · ms:4930
- 2026-09-28 · human-observed · S4 observed against a running binary on :1238: the Customers page serves <section id="usage"> with the month column headed "Aug 2026" — the previous COMPLETE calendar month, today being 28 Sep 2026 — and every cell rendering `0 pushed` over `0 delivered · 0 failed · 0 expired` plus a full aria-label naming the customer, the window and all four values ("acme@example.com over the last 24 hours: 0 pushed, 0 delivered, 0 failed, 0 expired"). A reader can say which window and which bucket every number belongs to without reaching the header
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:4dc7055fcc40db14ae24a36bcd495df05792df2149516f262f4a5adc96eb98b8 · ms:6784
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:4dc7055fcc40db14ae24a36bcd495df05792df2149516f262f4a5adc96eb98b8 · ms:5762
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:4dc7055fcc40db14ae24a36bcd495df05792df2149516f262f4a5adc96eb98b8 · ms:5728
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:4dc7055fcc40db14ae24a36bcd495df05792df2149516f262f4a5adc96eb98b8 · ms:6138
- 2026-09-28 · 08095e7* · exit 0 · `set -o pipefail …` · acceptance-sha256:4dc7055fcc40db14ae24a36bcd495df05792df2149516f262f4a5adc96eb98b8 · ms:5977
