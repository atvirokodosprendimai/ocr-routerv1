# ADR Backlog

Deferred work, each entry naming the record that deferred it. `adr-debt docs/adr` sweeps the ADR
corpus for `(deferred: …)` dispositions and reports them; this file is where they land, so an entry
reported by `adr-debt` and missing here is a pointer to nothing.

## Taken up

- **Per-token rate limiting beyond the per-customer buffer limit.**
  Deferred by ADR-0001; **taken up by ADR-0002**
  (`docs/adr/0002/0002-rate-limiting-and-structured-logging.md`, §Decision 1).
- **`?raw=1` — stdio passthrough billed at a flat 1 credit per request.**
  Requested by M 2026-09-15; **taken up by ADR-0006**
  (`docs/adr/0006/0006-raw-passthrough.md`), after M added the binary requirement on 2026-09-16.
  Every question this entry listed is answered there: raw composes with pipelines (the stage bridge
  is already a blob), the flat credit is charged on delivery, `GET /files/{id}` returns
  `application/octet-stream`, and a worker opts in by declaring a mode the admin record must
  already carry. The entry did NOT anticipate that the units channel is `[]string` end to end and
  cannot carry bytes at all — measured 2026-09-16, `encoding/json` replaces invalid UTF-8 with
  U+FFFD silently — which is what turned this from a flag into a record.
- **Structured request and transition logging (`log/slog` JSON).**
  Deferred by ADR-0001 task T11; **taken up by ADR-0002** (§Decision 2).
- **A way into the admin dashboard from a browser.**
  Not previously deferred — it was a gap nobody had recorded, found on 2026-09-15 when the
  dashboard turned out to be reachable only by `curl`. **Taken up by ADR-0003**
  (`docs/adr/0003/0003-admin-password-login.md`).
- **Unmetered customers — "`-1` credits means infinite".**
  Requested by M 2026-09-15; **taken up by ADR-0009**
  (`docs/adr/0009/0009-unmetered-customers.md`), after M settled its two open questions on
  2026-09-28. Both of this entry's questions were the right ones and both are answered there: a
  SEPARATE `unmetered` flag on the user rather than a sentinel in the balance column, and an
  unmetered job still ACCRUES its cost while debiting nothing, with `ocrr_credits_waived_total`
  added beside `ocrr_credits_debited_total` so the conflation this entry named is gone.
  What the entry did NOT anticipate is that the store needed no change at all: `Repo.DeliverJob`
  already guards both the balance decrement and the `credit_entries` insert with `charge != 0`, so
  the router passes 0 and the ledger correctly records nothing.
  ⚠ It also assumed `-1` would survive "as the display and wire spelling". It does not survive
  anywhere: the dashboard renders `unlimited`, the control is a toggle, and there is no wire surface
  carrying a balance at all. ADR-0009 marks that as its one deviation from the literal request, with
  M's ruling still outstanding as the record's only follow-up.

## Open

- **Draining: a worker finishes its in-flight jobs before exiting, rather than releasing them.**
  Deferred by ADR-0008 (`docs/adr/0008/0008-restart-is-not-a-failure.md`, §Out of Scope) and by its
  task T3, 2026-09-17. That record makes a shutting-down worker hand its leases BACK, which is right
  for a restart and wasteful for a deploy: work already half-done is thrown away and redone
  elsewhere. Draining needs a bound (how long may a shutdown wait?), a decision about what happens
  when it is exceeded, and an orchestrator that honours it. Worth taking up when deploys are
  frequent enough for the redone work to matter.
- **A metric for reclaimed jobs (`ocrr_jobs_reclaimed_total{label}`).**
  Open follow-up on ADR-0008, 2026-09-17. A service whose workers keep vanishing is an operational
  signal that nothing currently reports — it looks identical to a slow service. Bounded by ADR-0001
  T11's cardinality allow-list, which is why it is a question rather than a line of code.
- **A `job_failures` table keeping every attempt, not only the last.**
  Deferred by ADR-0007 (`docs/adr/0007/0007-failure-detail.md`, §Alternatives and §Out of Scope),
  2026-09-16. `jobs` keeps `attempts` and the LAST cause; a job that failed three different ways
  shows only the third. Deferred as speculative: nobody has asked to see attempt 2 of 3, and a
  second table is a migration, a retention policy and a join for a question nobody has posed. Take
  it up when someone actually needs to compare attempts.
- **Per-service failure counts on the Services table.**
  Deferred by ADR-0007 (§Out of Scope) and by its task T4, 2026-09-16. A recent-failure count and
  last error beside each label puts the signal next to the service that produced it. It is a good
  SUMMARY and a poor diagnosis — it cannot show which job, or let an operator read one — so it
  belongs beside the failures view ADR-0007 builds, not instead of it.
- **Alerting or notification on job failures.**
  Deferred by ADR-0007 (§Out of Scope), 2026-09-16. That ADR makes the failure data queryable —
  exit code as a column and a log attribute — and deliberately stops there. Alerting needs a
  destination, a threshold and a silencing story, none of which anyone has specified. ⚠ Note its
  open follow-up first: whether `exit_code` earns a metric dimension is undecided, and T11's
  cardinality allow-list exists to stop exactly that label being added speculatively.
- **Metric dimension for `exit_code` (`ocrr_jobs_total{state,exit_code}`).**
  Open follow-up on ADR-0007, 2026-09-16. Deliberately NOT added by that record: a subprocess can
  return 256 distinct codes, and ADR-0001 T11's allow-list exists because an unbounded label value
  is how a metrics backend falls over. Decide once there is real data about which codes actually
  occur.
- **Compression of a raw result in transit.**
  Deferred by ADR-0006 (`docs/adr/0006/0006-raw-passthrough.md`, §Out of Scope), 2026-09-16.
  A raw result streams uncompressed from the blob store to the client. For the outputs raw exists to
  carry — images, PDFs, audio — that is usually right, because they are already compressed and a
  second pass costs CPU for nothing. It is wrong for a raw service emitting large text, which is a
  shape nobody has asked for yet. Needs a decision on whether the router negotiates encoding at all,
  given that ADR-0001's "fat morph" argument elsewhere rests on brotli being negotiated on the SSE
  path and nobody has verified that it is.
- **Marking several services raw in one action from the dashboard.**
  Deferred by ADR-0006 task T8 (`docs/adr/0006/tasks/T8-admin-raw-control.md`, §Out of Scope),
  2026-09-16. T8 adds a per-row control. Bulk editing across rows is the same shape already
  deferred for customer settings by ADR-0004, and should be decided once for both rather than
  twice differently. ADR-0009 task T4 adds a THIRD instance on 2026-09-28 — marking several
  customers unmetered in one action — which settles the argument for deciding it once: three
  per-row controls have now each deferred the same feature separately.

- **OpenTelemetry / OTLP export for the router.**
  Deferred by ADR-0001 (§Alternatives), by task T11 (§Out of Scope), and **re-deferred by ADR-0002**
  (§Decision 3) on 2026-09-15.
  T11 ships Prometheus text format on a loopback listener. OTEL was rejected **for now** because it
  adds a collector as a runtime dependency for a single-process system; the sibling project
  `wgmesh`/chimney chose OTEL in a context where Coroot was already running, which is that
  project's call and not precedent here. ADR-0002 reviewed this and changed nothing: no new
  evidence arrived, and implementing it would have reversed an accepted decision on no grounds. The
  metric names T11 defines remain exportable over OTLP without re-measuring anything, so this stays
  a transport decision.

- **Logs delivered over SSE to the admin dashboard.**
  Deferred by ADR-0002 (§Alternatives, §Out of Scope).
  The system already has an SSE bus and a dashboard to render into, which is why this was
  considered seriously. It needs a decision about which principals may read which logs — a customer
  must never see another's params, and the transition log is full of them — and that authorization
  design is larger than the logging it would serve.

- **Audit logging of administrative actions as a separate tamper-evident stream.**
  Deferred by ADR-0002 task T2 (§Out of Scope).
  ADR-0002's request log records that an admin called an endpoint. It is not tamper-evident and it
  is not separable from operational noise, which is what an audit trail has to be.

- **Tracing spans across the router → worker → router round trip.**
  Deferred by ADR-0002 (§Out of Scope) and task T4.
  The transition log makes a job's path readable in one process. It does not correlate with what
  the worker did, because the worker emits nothing structured.

- **Per-route rate limits, as opposed to per-role.**
  Deferred by ADR-0002 task T3 (§Out of Scope).
  One limit covers every route a role can reach. `POST /upload` and `POST /claim` have very
  different costs, and a limit tuned for one is loose or tight for the other.

- **Structured logging inside `cmd/worker`.**
  Deferred by ADR-0002 task T4 (§Out of Scope).
  ADR-0002 covers the router. The worker still writes nothing structured, so the subprocess's
  stderr — the single most useful artefact when a job fails — is not captured anywhere.

- **`./cmd/client` — a CLI client that submits one file and waits.** (requested by M, 2026-09-15;
  wants its own ADR)
  Today a customer integrates by hand: `POST /upload`, hold `GET /sse` open, watch for `ready`, then
  `GET /files/{id}`. That is four moving parts and an SSE reader, which is a lot to ask of someone
  who wants one PDF OCR'd.
  **Shape asked for:** `client --router <url> --token <tok> -i input.pdf -o results.dat`. It BLOCKS
  until the job finishes and reports progress on the terminal — `uploading… / waiting… / got
  results → results.dat`.
  **What the ADR has to decide**, because none of it follows from the request: whether it waits on
  SSE or polls `GET /files/{id}` (SSE is what the router is built for, polling is far simpler and
  survives a proxy that buffers); what progress looks like when stdout is not a TTY, since the
  obvious spinner becomes noise in a CI log and in a pipe; what happens on `--label`, pipelines and
  query params, which the API supports and this flag set does not mention; the exit code for a job
  that goes `dead` or `expired`, because a client that exits 0 on a failed job is worse than one
  that hangs; and whether `-o -` writes to stdout, which is the shape that makes it composable.

- **Self-service password change in the dashboard UI.**
  Deferred by ADR-0003 (§Out of Scope) and task T2.
  An administrator changes their own password by asking someone with shell access to run
  `router admin set-password`. That is workable for one operator and absurd for five. It needs the
  current password re-verified in the form and every OTHER session of that user revoked on success,
  neither of which is implied by the CLI path.

- **Two-factor authentication for administrators.**
  Deferred by ADR-0003 (§Out of Scope).
  A password is one factor. TOTP is the obvious second and needs enrolment, recovery codes, and a
  decision about what happens when the only administrator loses their phone — which is the part
  that makes it a real piece of work rather than a library call.

- **Session listing and bulk revocation in the dashboard.**
  Deferred by ADR-0003 task T2 (§Out of Scope).
  `session.Store` already supports `RevokeAllForUser`, so the primitive exists and nothing surfaces
  it. "Sign out everywhere" is the feature; the UI decision is where it lives.

- **Remembering the requested URL across a login redirect.**
  Deferred by ADR-0003 task T3 (§Out of Scope).
  Signing in always lands on `/admin`, so a bookmarked `/admin/services` costs a second click. The
  reason it is not free: the stored destination is attacker-influenced input and has to be
  validated as a same-origin relative path, or the login page becomes an open redirect.

- **Audit logging of logins as a separate tamper-evident stream.**
  Deferred by ADR-0003 (§Out of Scope).
  ADR-0002's request log records that a login happened. It is not tamper-evident and not separable
  from operational noise, which is what an audit trail has to be.

- **A quota or cap on an unmetered customer — "unlimited, but not more than N per month".**
  Deferred by ADR-0009 (`docs/adr/0009/0009-unmetered-customers.md`, §Out of Scope) and by its task
  T2, 2026-09-28. That record makes a customer exempt from the balance entirely, which is what was
  asked for and is also the whole of the bound: nothing caps what an unmetered customer consumes.
  Needs a decision about what a cap means when there is no balance to subtract from — a monthly
  accrual ceiling read from `jobs.accrued_credits`, refusing admission past it, is the obvious shape
  and it reintroduces a refusal for exactly the customers the flag exists to stop refusing.

- **An audit trail of who marked a customer unmetered, and when.**
  Deferred by ADR-0009 (§Out of Scope), 2026-09-28. ADR-0002's request log records that an admin
  called the endpoint; it is not tamper-evident and not separable from operational noise. This is the
  same shape as the two audit entries above and should be decided once for all administrative
  actions rather than three times — making work free is simply the one with money attached.

- **Showing a customer's credit ledger in the dashboard.**
  Deferred by ADR-0009 (§Out of Scope) and by its task T4, 2026-09-28; found while auditing the
  dashboard on the same day. `Repo.Ledger` exists, is tested, and is called by nothing — the fourth
  instance of that defect in this codebase — so the Credits column shows a number that cannot be
  explained. An operator asking "why is this 37" has to open SQLite. The UI decision is where it
  lives: a row expansion like the token list, or its own view.

- **A confirmation step on the destructive row actions in the Customers table.**
  Deferred by ADR-0009 task T4 (§Out of Scope), 2026-09-28. `Mint`, `Disable` and the unmetered
  toggle fire on one click, from a row of visually identical secondary buttons, on rows that differ
  only by an email address. Needs a decision about which actions earn a confirmation — one on every
  action trains an operator to dismiss it, which is worse than none.

- **Refusing self-deactivation, and the lockout it currently causes.**
  Found 2026-09-28 while auditing the dashboard; deferred by ADR-0009 task T4 (§Out of Scope) because
  it is not about metering. ⚠ An administrator can click `Disable` on their own row.
  `identity.SetActive` checks only that the ACTOR is an admin, and both `ResolveSession` and `Login`
  refuse an inactive user — so the live session dies on the next request and the same credentials are
  refused afterwards. `router admin` has only `bootstrap` (refused once any admin exists) and
  `set-password`, so **there is no recovery path in the binary**. Needs a decision on whether an
  admin may disable another admin at all, and whether the fix is a server-side refusal in
  `SetActive`, a `router admin activate` command, or both.

- **Charts and historical analytics on the admin dashboard.**
  Deferred by ADR-0001 task T10 (§Out of Scope). ⚠ **PARTLY TAKEN UP by ADR-0010** on 2026-09-28 —
  the COUNTING half only.
  T10 ships current state. Nothing retains a time series, so "was it like this yesterday" has no
  answer; T11's metrics are the intended source if this is ever built.
  What ADR-0010 changed about this entry: a COUNT over a window turned out to need no retained series
  at all, because nothing deletes job rows — the reaper removes blobs and results, never rows. So
  per-customer counts for 24h / 7d / 31d / the previous calendar month are one indexed query. A CHART
  is still a series of counts at retained resolution and still has no source, so this entry stays open
  for exactly that: resolution, retention, and what happens to the numbers once a row is old.

- **A `job_stats` rollup table, updated on every job transition.**
  Deferred by ADR-0010 (`docs/adr/0010/0010-per-customer-usage-counters.md`, §Alternatives and
  §Out of Scope) and by its task T1, 2026-09-28, WITH THE CONDITION THAT TRIGGERS IT: take this up when
  the window scan is measurably slow, not before. ADR-0010 counts by scanning the job rows newer than
  the oldest window floor — about 62 days' worth — on every SSE push, which is bounded and cheap now
  and grows with traffic. A rollup is the answer then, and it is a second source of truth for numbers
  that are currently one query away: it needs a backfill and it can drift from `jobs` with nothing
  reporting the drift.

- **`GET /usage` — a customer reading its OWN counters over the API.**
  Deferred by ADR-0010 (§Alternatives, §Out of Scope), 2026-09-28. M chose dashboard-only when asked.
  Not rejected on merit: it is the endpoint a customer would reconcile their own invoice against, and
  the counting is already done. It is a new PUBLIC contract, so it needs its own shape, its scoping
  (a client must see only its own), and a rate-limit decision — ADR-0002's per-role limits were not
  tuned for an endpoint that runs an aggregate.

- **Bytes and credits per window, beside the counts.**
  Deferred by ADR-0010 (§Out of Scope) and by its task T1, 2026-09-28. `jobs.size_byte` and
  `jobs.accrued_credits` are already on every row and already inside the window the query scans, so
  this is two more conditional sums rather than new data. It was left out to keep the first version's
  table readable: four windows × four counts is already sixteen numbers per customer.

- **Per-service breakdown of a customer's usage.**
  Deferred by ADR-0010 (§Out of Scope), 2026-09-28. `jobs.label` is in the same rows. The reason it is
  not free: it multiplies the cells by the number of labels, so it is a different VIEW rather than more
  columns — probably a per-customer drill-down, which is the same UI question as showing a customer's
  credit ledger.

- **Sorting or filtering the usage table.**
  Deferred by ADR-0010 task T2 (§Out of Scope), 2026-09-28. "Who pushed the most this week" is the
  obvious next question and the table answers it only by eye. Sorting a live fragment needs a decision
  about where the sort lives, since a patch re-renders it: a query parameter like the jobs filter
  (shareable, survives the patch) or a client-side signal (cheaper, lost on every push).

- **The per-row edit inputs refill from a stale signal after an SSE patch.**
  Found 2026-09-28 during the dashboard audit; deferred by ADR-0010 task T2 (§Out of Scope) because
  that task AVOIDS it rather than repairing it — the usage counters got their own fragment specifically
  so the editable table is never patched. ⚠ The defect itself stands: datastar's `data-bind` initialises
  its signal only when missing (`{ifMissing:!0}` in the vendored bundle) and then an effect pushes the
  SIGNAL into the element, so after `refreshUsers` morphs `#user-table` the server's fresh empty inputs
  are refilled from the stale signal. The credit-adjust boxes are therefore NOT cleared after a
  successful adjustment, and a second click credits again. ⚠ Reasoned from the bundle, NOT observed in a
  browser — confirm it there before designing the fix.

- **Encryption at rest for source files and results.**
  Deferred by ADR-0001 task T4 (§Out of Scope).
  Source blobs sit unencrypted on disk and results sit unencrypted in memory. Needs a decision on
  key custody before anything else.

- **Packaging: systemd units, containers, release artefacts.**
  Deferred by ADR-0001 task T8 (§Out of Scope).
  The binaries build and run; nothing ships them.

- **Sandboxing the worker subprocess with containers or seccomp.**
  Deferred by ADR-0001 task T9 (§Out of Scope).
  The worker forks an operator-configured command with customer-supplied arguments. ADR-0001
  constrains the argv construction — no shell, validated keys, values as their own argv elements —
  and does not confine the process that results.

- **Windows support for the worker.**
  Deferred by ADR-0001 task T9 (§Out of Scope).
  Process-group handling and the tmpdir lifecycle are POSIX-shaped.
