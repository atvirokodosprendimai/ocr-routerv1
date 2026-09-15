# ADR Backlog

Deferred work, each entry naming the record that deferred it. `adr-debt docs/adr` sweeps the ADR
corpus for `(deferred: …)` dispositions and reports them; this file is where they land, so an entry
reported by `adr-debt` and missing here is a pointer to nothing.

## Taken up

- **Per-token rate limiting beyond the per-customer buffer limit.**
  Deferred by ADR-0001; **taken up by ADR-0002**
  (`docs/adr/0002/0002-rate-limiting-and-structured-logging.md`, §Decision 1).
- **Structured request and transition logging (`log/slog` JSON).**
  Deferred by ADR-0001 task T11; **taken up by ADR-0002** (§Decision 2).
- **A way into the admin dashboard from a browser.**
  Not previously deferred — it was a gap nobody had recorded, found on 2026-09-15 when the
  dashboard turned out to be reachable only by `curl`. **Taken up by ADR-0003**
  (`docs/adr/0003/0003-admin-password-login.md`).

## Open

- **Unmetered customers — "`-1` credits means infinite".** (requested by M, 2026-09-15; not yet
  deferred by any ADR, so this entry is the only record of it)
  Admission is `u.Credits <= 0` (`internal/router/service.go:111`) and every completed stage debits
  `len(out) * rate` (`:244`, charged at `:375`). A customer who should never be refused has to be
  topped up by hand for ever.
  **What an ADR has to decide**, because the request and the existing design conflict: credits are
  an APPEND-ONLY LEDGER and the only control is *adjust*, never *set* — ADR-0004 made that explicit
  and `TestCreditsAreNeverSetDirectly` pins it. `-1` is a SENTINEL, not a balance, and adjusting a
  sentinel is meaningless: `-1 + 50` is `49`, not "infinite plus fifty". So the record must choose
  between a sentinel in the balance column (cheap, and it puts a non-balance in a ledger) and a
  separate `unmetered` flag on the user (honest, and it is a schema change plus its own control),
  with `-1` kept only as the display and wire spelling. It must also say whether an unmetered job
  still accrues a cost for reporting, since "we cannot bill it" and "we cannot see what it cost"
  are different claims, and the metrics counter at `:385` currently conflates them.

- **`?raw=1` — stdio passthrough billed at a flat 1 credit per request.** (requested by M,
  2026-09-15)
  **Shape asked for:** the request body goes to the worker subprocess on stdin unchanged, whatever
  the worker writes on stdout comes back unchanged, and the request costs exactly 1 credit however
  much comes out.
  **What an ADR has to decide:** today a result is a list of UNITS — `joinUnits` renders them for
  the next stage (`internal/router/service.go:289`) and `len(out) * rate` prices them (`:244`). Raw
  mode has no units, so it needs a job-level mode that both the pricing and the unit-splitting
  respect, not a special case in one of them. Also: whether raw composes with pipelines at all
  (stage 2's input is `joinUnits` of stage 1, which raw has no answer for); whether the flat credit
  is charged on admission or on delivery, which decides who pays for a job that dies; what
  `GET /files/{id}` returns for a raw job, since the client currently expects units; and whether a
  worker opts into raw or is handed it, because a worker that ignores the mode silently returns
  unit-split output at a flat price.

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

- **Charts and historical analytics on the admin dashboard.**
  Deferred by ADR-0001 task T10 (§Out of Scope).
  T10 ships current state. Nothing retains a time series, so "was it like this yesterday" has no
  answer; T11's metrics are the intended source if this is ever built.

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
