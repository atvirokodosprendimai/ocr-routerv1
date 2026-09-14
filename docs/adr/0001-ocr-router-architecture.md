# ADR-0001: Route OCR work to internet-resident workers over an SSE command bus with page-metered credits

**Status:** Accepted
**Date:** 2026-09-15
**Owner:** M (operator) — authored by claude-code-aks
**Spec:** None — no spec stage
**Cross-references:** none
**Governs:** `cmd/**`, `internal/**`, `db/migrations/**`, `docs/adr/0001-ocr-router-architecture.md`
**Enforced-by:** None — this is a greenfield structural decision; its clauses are enforced by the per-task Acceptance fences and their bound mutants, not by a standing gate. The single-writer clause has no cheap mechanical check: naming `go vet` here would name a check that cannot fail on it.
**Invalidates:** none — checked (`adr-state.mjs` reports no decision records under this repository, 2026-09-15)
**Served-path change:** A customer with a bearer token can `POST /upload` a document and receive OCR'd page text back over `GET /files/{id}` after being told on `GET /sse` that it is ready; nothing served this before, the repository was empty.

## Context

Greenfield. The repository at `b3f6be7` contains `LICENSE`, `README.md` (14 bytes) and
`.gitignore` and nothing else — verified 2026-09-15 by `find . -type f -not -path './.git/*'`,
which returned those three paths. There is no prior architecture doc, no spec, and
`adr-state.mjs` reports no decision records, so nothing constrains this design except the
operator's brief and the team's standing craft records.

The operator's brief, in substance:

- Go + datastar. SQLite, **no cgo**.
- Multi-user; only an admin creates users, by email.
- Two commands: `./cmd/router` and `./cmd/worker`.
- Clients **and** workers speak the same endpoints — `POST /upload`, `GET /sse`,
  `GET /files/:uuidv7` — authenticated by **bearer token**.
- SSE is the **command channel**: the router tells a client "take that `:uuidv7` file" and
  tells workers that work exists. A poll expressed as a push. Keepalive is in scope.
- A worker downloads to a tmpdir (tmpfs or a folder) and forks `cmd -i <file> -o -`,
  reading an **array of strings** from stdout — one entry per page.
- 1 page = 1 credit; the router counts the pages out of the worker's response.
- Results live **in memory** and are deleted once transferred to the client.
- A reconnecting client must be told which files are waiting for it.
- Each **customer** (not each token) has a buffer limit — N files in flight, so workers
  never starve.
- **Customer priority with a TTL**: a VIP customer's job is taken before a low-priority
  customer's. Added by the operator on 2026-09-15, mid-authoring.
- **The worker is not OCR-specific.** Added by the operator on 2026-09-15, mid-authoring,
  and it is the largest of the three late changes: *"we don't care what files arrive… worker
  only downloads file, puts it to tmpdir, spawns subprocess with args and reads stdout…
  we can reuse workers in more flexible ways"*. A job carries a **label** naming the service
  it wants (`ocr` by default, `strip-html`, whatever else exists); workers advertise the one
  label they serve. OCR becomes the default instance of a generic pattern, not the product.
- An admin dashboard.

Three forks were put to the operator on 2026-09-15 and answered:

1. **Undelivered results** → memory + TTL; a restart re-queues from the source blob, which
   is kept on disk until delivery. Charged once, at delivery.
2. **Router scale** → exactly one router process.
3. **Dispatch** → broadcast + first-claim-wins, with the fourth endpoint explicitly accepted.

## Existing Primitives Audit

**None in this repository — it is empty.** Four primitives are inherited from measurements
made elsewhere in this workspace rather than re-derived here:

| Primitive | Source | Reuse / reshape |
|---|---|---|
| `modernc.org/sqlite` two-handle pattern | measured in `app-warehousev1`, 2026-09-06 | **Reuse as measured**: writer `_txlock=immediate` + `MaxOpenConns(1)` took read-then-write transaction failures from 257/320 to 0/320; reader `_pragma=query_only(1)` is honoured (an INSERT through it fails `attempt to write a readonly database (8)`). |
| goose driver/dialect name mismatch | `agentsmemory`, verified 2026-08-25 | **Reuse the fix**: driver registers as `sqlite`, goose dialect is `sqlite3`; a mismatch fails at startup only. |
| `foreign_keys` defaults OFF | `agentsmemory`, verified 2026-08-25 | **Reshape**: that project omitted the pragma and silently leaked orphans. Set it explicitly here. |
| datastar v1 attribute surface | `wing_craft/examples`, cached 2026-08-23 | **Reuse**: `data-init` (not `data-on-load`), `data-on:click` (colon), no `<form>`, every response 200 + HTML fragment. |

No existing OCR, queueing or auth component is available to reshape; all are new.

## Decision

Build one router process and N worker processes. The router owns a SQLite file, a blob
directory and an in-memory result map; workers own nothing durable.

**Endpoints.** The operator's three endpoints each carry both roles, discriminated by the
bearer token's role, plus one claim endpoint the operator accepted when choosing
first-claim-wins:

| Method | Path | client role | worker role |
|---|---|---|---|
| `POST` | `/upload` | multipart source file → `201 {job_id}` | JSON result or failure → `204` |
| `GET` | `/sse` | command stream + backlog of ready files | command stream + work signal |
| `GET` | `/files/{id}` | the OCR result JSON; deleted on success | the source blob; lease required |
| `POST` | `/claim` | — | leases one queued job → `200 {job_id}` or `204` |

`POST /claim` takes **no body**: a worker asks for *any* queued job. Naming a job from the
broadcast means every worker races for the same id and N−1 take a `409` per announcement,
which is the thundering herd the broadcast was meant to avoid.

**Job lifecycle**, with `router.Service` as the single writer:

```
  POST /upload (client) --> queued --POST /claim--> processing --POST /upload (worker)--> done
                              ^                          |                                  |
                              |                          | lease expired / reported failure  | GET /files/{id}
                              +--------------------------+  (attempts < max)                 v
                                                         |                              delivered
                                                         +-- attempts exhausted --> dead  (charge N,
                                                                                          drop blob+RAM)
```

Handlers call the service for writes and the repository directly for reads. That asymmetry
is the CQRS and it is the whole of the CQRS taken here.

**Storage shape is state-based, not event-sourced** (`cqrs` §0): a job row is mutable and
nobody will ask what it looked like at time T. The one exception is `credit_entries`, which
is append-only, because it is a money ledger — the canonical case where the log *is* the
domain.

**Credits are charged at delivery, exactly once.** The `-pages` ledger entry, the
`users.credits` decrement and the `done → delivered` transition happen in one immediate
transaction, so a crash cannot separate them and a re-delivery cannot double-charge.
Admission is gated at upload on `credits > 0` — the page count is unknowable before OCR
runs, so a document longer than the balance is delivered and charged in full, taking the
balance negative by at most one document. **What would falsify this clause:** a delivery
that debits twice, or a `delivered` row with no matching ledger entry. Both are checkable by
query and both are asserted in T5's tests; the criterion is valid for a single-writer router
and would not survive a second concurrent writer.

**A job names a service by label; the worker is a generic subprocess runner.** `jobs.label`
defaults to `ocr`. A worker process advertises exactly one label and holds one command;
running two services means running two processes. The worker's whole job is: claim →
download to tmpdir → `exec <cmd> -i <file> -o -` → read a JSON `[]string` from stdout →
`POST /upload` → delete the temp file → claim again. Nothing about OCR is compiled into it.

**A job is a PIPELINE of stages, declared by the client at upload.** `label` is shorthand
for a one-stage pipeline; the general form is `?pipeline=crawl,strip-html` — fetch a URL,
then strip its HTML. `jobs.pipeline` holds the labels as JSON and `jobs.stage` the current
index. When a worker completes stage *n*:

- if another stage remains, the router writes that stage's **output as the new input blob**,
  sets `label = pipeline[n+1]`, `stage = n+1`, and returns the job to `queued`. It is
  claimed by a worker serving the next label. Nothing is delivered and the client is not
  notified — a pipeline is one job with one id from the client's point of view.
- if it was the last stage, the job goes `done` and the client is told `ready`, exactly as
  a single-stage job does today.

★ **The operator's framing was "worker == client", and the implementation deliberately does
NOT make that literally true.** A worker does not get client credentials and does not upload
a new job; it posts its stage output to `/upload` exactly as it already does, and the
*router* advances the stage. This achieves what was asked — output of one service becomes
input of the next — without the consequence that a worker token, which lives on machines
anywhere on the internet, could create jobs and spend a customer's credits. The pipeline is
router-owned state, not a convention between workers.

**The bridge between stages is a blob**, because that is what the next stage's `-i` expects.
A stage returning one element writes it verbatim; a stage returning several writes them
joined by `\n`. ⚠ **This encoding is the author's choice, not the operator's**, and it is
the one part of the pipeline design worth revisiting: it suits `crawl → strip-html` (one
document in, one out) and is lossy for a stage whose elements contain newlines. It is a
one-function change if it turns out wrong.

**Cost accrues per stage and is still charged exactly once, at delivery.** Each completed
stage adds `len(output) × rate(label)` to `jobs.accrued_credits`; delivery debits that
total in the single transaction that already exists. So a crawl at rate 0 followed by OCR
at rate 1 costs what the OCR cost, the charge-once invariant is untouched, and an abandoned
pipeline costs the customer nothing.

`queued_at` is **not** reset between stages, so a job halfway through a pipeline keeps the
age it has accrued and is preferred over a freshly uploaded one. Without that, long
pipelines starve under load — every stage would go to the back of the queue. The
`expires_at` deadline likewise covers the whole pipeline, not each stage.

**A job's input is a blob, parameters, or both.** The client supplies parameters as query
string on `POST /upload`, and the file part becomes **optional** — which is what lets the
same worker binary be a crawler: `POST /upload?label=crawl&url=https://example.com` with no
body at all. `jobs.params` stores them as a JSON object.

The subprocess argv is assembled as:

```
<cmd> [-i <tmpfile>] -o - --<key> <value> …          # -i only when the job has a blob
```

⚠ **This is the one place untrusted client input reaches a process boundary**, so the rules
are tight and they are the decision, not an implementation detail:

- **Never a shell.** `exec.Command` with an explicit argv — never `sh -c`, never a format
  string. Shell metacharacters in a value are then bytes, not syntax.
- **Keys are validated** against `^[a-z][a-z0-9-]{0,31}$` and rejected at upload otherwise.
  A key becomes a flag name, so it is the half that must be constrained.
- **Values are passed as their own argv element** and never concatenated, so a value can
  never split into two arguments.
- **Residual risk, stated rather than hidden:** a *value* beginning with `-` may be read as
  a flag by the child program. We do not try to solve this generally — the child's flag
  parser is not ours to model. The operator's `--cmd` should be a program that stops
  parsing at `--`, or a two-line wrapper script. `--param-prefix` is not offered; one
  mechanism, documented.

**Which labels are valid is derived from the workers currently connected**, not from an
admin-maintained list: the valid set is the union of the labels advertised by live worker
SSE streams. Adding a service is starting a worker. The cost is that the same upload
succeeds or fails depending on who is connected, so — ⚠ **this refinement is mine, not the
operator's** — a label stays valid for a **grace window** (`--label-grace`, default 5m)
after the last worker advertising it disconnects, which absorbs a rolling restart without
reintroducing an admin screen. A label never seen is rejected at upload with `400` and the
list of labels that *are* available, so a typo fails immediately instead of sitting queued
until its deadline.

**Pricing is admin-owned, and deliberately NOT derived from the worker.** `service_rates`
is an admin-managed table of `label → credits_per_unit`, defaulting to 1 for a label with
no row, and the charge is `len(results) × rate`. With `rate = 1` this is exactly the
operator's "1 page = 1 credit". **The split is a security boundary, not an inconsistency:**
workers run on the open internet, so letting a worker advertise its own price would mean a
leaked worker token could set what customers are charged. Validity is cheap to derive and
harmless if wrong; price is neither.

**Queue order is aged priority, and a queued job has a hard deadline.** `users.priority`
is an admin-set integer, higher first, defaulting to 0 — so tiers are data, not code, and
adding one is an `UPDATE`. Strict priority alone starves the bottom tier indefinitely, so
the claim orders by an **effective** priority that rises with waiting:

```
effective = users.priority + (now - jobs.queued_at) / aging_step
ORDER BY effective DESC, jobs.id ASC          -- id is uuidv7, so this is FIFO within a tier
```

`aging_step` is one router-wide flag (`--aging-step`, default 60s), because it tunes the
*queue*, not a customer: at the default, a job that has waited ten minutes gains 10 points
of effective priority, so a `priority=0` job overtakes a fresh `priority=100` VIP job after
100 minutes of waiting. Both halves of that sentence are the knob — raise `aging_step` to
make priority stickier, lower it to make the queue fairer.

Independently, a queued job past `jobs.expires_at` transitions to **`expired`**: not
claimed, not charged, blob dropped, client told on SSE. The deadline comes from
`users.job_ttl_secs` at upload time (`0` = no deadline), so a customer who would rather
have a fast failure than a late result gets one. Expiry applies **only to `queued`** — once
a worker holds the lease the job runs to completion, because killing work already paid for
in worker time helps nobody.

Aging bounds waiting; the deadline bounds it absolutely. **What would falsify the pair:** a
job whose effective priority never rises (aging broken), or a job claimed after its
deadline (expiry racing the claim). Both are asserted in T2, and the claim statement
excludes expired rows in the same `WHERE` that selects them, so the race cannot be won by
the claim.

**Results are memory-only with a TTL**; the blob on disk is the durable copy. A sweeper
expires abandoned results and returns the job to `queued`. On boot every `processing` and
`done` job is reset to `queued`, because leases and results died with the process. Since
nothing is charged before delivery, a restart produces repeated work and never a double
charge.

**No NATS.** The `cqrs` skill's default is a dual in-process `Bus` + NATS fan-out so a
second process is a config change; with exactly one router there is no second subscriber for
NATS to reach. The seam is kept as a one-interface `Bus` so adding NATS later is a second
implementation, not a restructuring.

## Alternatives Considered

- **Embedded NATS now, as the `cqrs` skill defaults to.** Rejected: the operator chose a
  single router, so NATS would have exactly one client — its own process. The skill's own
  §0b warns that a half-migration is the worst of the three states.
- **Router-assigned push dispatch** (router picks an idle worker, sends `take <uuid>` on that
  worker's stream only). Rejected by the operator in favour of first-claim-wins. It avoids
  the claim endpoint but makes the router responsible for worker liveness and concurrency
  accounting, which the lease already handles.
- **Claim-on-GET**, folding the lease into the worker's `GET /files/{id}` to keep the
  endpoint count at three. Rejected: a GET that transitions state misleads every cache,
  proxy and retry between the worker and the router, and the operator had already accepted a
  fourth endpoint.
- **Event-sourcing the job aggregate.** Rejected per `cqrs` §0: a live UI needs a
  notification plus a read model, not a log as the system of record. Adopting one here buys
  payload versioning and snapshots in exchange for a feature nobody asked for.
- **Reserving credits at upload against an estimated page count.** Rejected: an estimate too
  low still overdraws, and one too high refuses work the customer can afford. An honest
  bounded overshoot is better than a wrong reservation.
- **Strict priority ordering with no aging.** Rejected: under sustained VIP load a
  `priority=0` customer's job is never claimed, and "never" is indistinguishable from a
  broken queue from the customer's side. Aging bounds the wait; the operator chose it
  alongside the deadline on 2026-09-15.
- **Named priority tiers** (`vip`/`standard`/`bulk`) mapped to weights in code. Rejected by
  the operator in favour of an integer: adding a tier would otherwise be a code change and
  a deploy, where an integer makes it an `UPDATE`.
- **Expiring a job that a worker is already running.** Rejected: the worker time is already
  spent, and discarding a nearly-finished result to honour a deadline wastes the one
  resource the deadline exists to protect. Expiry applies to `queued` only.
- **Per-customer aging steps.** Rejected: `aging_step` trades priority stickiness against
  queue fairness for the *whole* queue, so a per-customer value would let one customer
  redefine everyone else's wait.
- **An admin-maintained service registry** (a `services` table gating which labels exist).
  Rejected by the operator in favour of deriving validity from live workers: adding a
  service should be starting a worker, not filing a form. The grace window is what makes
  that survive a restart.
- **Free-form labels with no validation at all.** Rejected: a client typo (`strip-htm`)
  creates a queue no worker serves, and the job sits until its deadline before failing.
  Rejecting at upload with the list of available labels turns a silent 30-minute failure
  into an immediate, actionable `400`.
- **Letting a worker advertise its own `credits_per_unit`.** Rejected on security grounds,
  and this is the one place the derive-from-workers principle is deliberately not applied:
  workers run anywhere on the internet, so a leaked worker token would become the ability
  to set customer pricing. Rates stay admin-owned.
- **One worker process serving many labels from a config file.** Rejected by the operator
  in favour of one process per label: a bad command for one service cannot then take down
  a process serving others, and the flag set stays two strings.
- **Making a worker literally a client** — issuing worker tokens client rights so a worker
  uploads a new job for the next stage, as the operator's "worker == client" phrasing
  suggests. **Rejected on security grounds.** Worker tokens live on machines anywhere on
  the internet; client rights would let a leaked one create jobs and spend customers'
  credits, and the parent/child link would exist only as a convention between workers with
  nothing tying a chain to the customer who started it. Router-owned stages get the same
  capability with none of that.
- **Free-form DAG pipelines** (fan-out, conditionals, joins). Rejected as speculative: the
  operator described a linear chain, a list of labels expresses exactly that, and a DAG
  needs a completion model, partial-failure semantics and a UI nobody has asked for.
- **Charging per stage as each completes**, rather than accruing and charging at delivery.
  Rejected: it breaks the charge-once-at-delivery invariant and would charge a customer for
  a pipeline that later fails or expires before they receive anything.
- **Passing intermediate output between stages in memory** instead of writing a blob.
  Rejected: the next stage's contract is `-i <file>`, and keeping an intermediate in RAM
  would make a pipeline unrecoverable across a restart, which the blob-on-disk design
  otherwise guarantees.
- **Keeping the worker OCR-specific and adding a second binary per service type.** Rejected:
  every such binary would be the same download/exec/upload loop with a different constant,
  which is the duplication the label indirection removes.
- **GORM as the query layer**, as `effective-go` suggests for new projects. Rejected for
  this repository: the whole persistence surface is roughly a dozen statements, and raw
  `database/sql` keeps the two-handle writer/reader split explicit — which is the invariant
  the driver is being asked to enforce.
- **Persisting results to disk.** Rejected: it contradicts the operator's "results from
  memory deletes", and the blob already makes the work recoverable.

## Component / Boundary Impact

No architecture document exists (greenfield), so this ADR establishes the initial module map
rather than delta-ing one. Each component below has one reason to change:

| Component | Owns | Changes when |
|---|---|---|
| `internal/core` | domain types, sentinel errors, ports | the domain vocabulary changes |
| `internal/store` | SQLite handles, migrations, all SQL | the schema or a query changes |
| `internal/blob` | source files on disk | blob layout changes |
| `internal/results` | the in-memory result map + TTL | the delivery contract changes |
| `internal/bus` | in-process per-user fan-out | fan-out becomes cross-process |
| `internal/router` | **the single writer**: job + credit state machine | the lifecycle or metering changes |
| `internal/httpapi` | HTTP boundary, auth middleware, SSE | the wire contract changes |
| `internal/web` | admin dashboard views | the dashboard changes |
| `internal/ocr` | forking the OCR command, parsing stdout | the OCR tool contract changes |
| `internal/agent` | worker-side claim/download/run/upload loop | worker behaviour changes |

Bounded context: there is one — *OCR routing*. `credit` is an aggregate inside it, not a
separate context; splitting billing out would be speculative at this size.

## Wiring & Contract Changes

| Surface | Change | Producer | Consumer(s) |
|---------|--------|----------|-------------|
| `POST /upload` (multipart) | new | client | router |
| `POST /upload` (JSON result) | new | worker | router |
| `GET /sse` (text/event-stream) | new | router | client, worker |
| `GET /files/{id}` | new | router | client, worker |
| `POST /claim` | new | worker | router |
| `Authorization: Bearer <token>` | new | client, worker, admin | router |
| SSE events `hello`/`backlog`/`ready`/`failed`/`work`/`ping` | new | router | client, worker |
| OCR subprocess contract `cmd -i <file> -o -` → JSON `[]string` on stdout | new | worker | operator-supplied OCR tool |
| DB schema `users`, `tokens`, `jobs`, `credit_entries` | new | migration `00001` | router |
| `jobs.pipeline TEXT` (JSON array of labels) | new | client (query param) | stage advancement |
| `jobs.stage INTEGER NOT NULL DEFAULT 0` | new | router | which label is current |
| `jobs.accrued_credits INTEGER NOT NULL DEFAULT 0` | new | router | the total debited at delivery |
| `jobs.label TEXT NOT NULL DEFAULT 'ocr'` | new | client (query param) | claim filtering, worker topic |
| `jobs.params TEXT` (JSON object) | new | client (query string) | worker argv |
| `jobs.has_blob INTEGER NOT NULL` | new | router | whether `-i <file>` is passed |
| `service_rates(label, credits_per_unit)` | new | admin (dashboard) | the delivery charge |
| `GET /services` | new | router | client discovers the labels currently available |
| `GET /sse?label=<l>` (worker) | new | worker | advertises the label it serves |
| `users.priority INTEGER NOT NULL DEFAULT 0` | new | admin (dashboard) | claim ordering |
| `users.job_ttl_secs INTEGER NOT NULL DEFAULT 0` | new | admin (dashboard) | `jobs.expires_at` at upload |
| `jobs.queued_at`, `jobs.expires_at` (unix seconds) | new | router | claim ordering, expiry sweep |
| `core.JobStateExpired` | new | T1 | T2, T6, T7 |
| SSE event `failed` with `reason:"expired"` | new | router | client |
| `--db`, `--blobs`, `--addr`, `--result-ttl`, `--lease`, `--max-attempts`, `--aging-step` | new | operator | `cmd/router` |
| `--router`, `--token`, `--tmpdir`, `--ocr-cmd`, `--slots`, `--timeout` | new | operator | `cmd/worker` |

## Inter-task Contracts

| Contract | Producing task | Consuming task(s) | Breaking? |
|----------|----------------|-------------------|-----------|
| `core.Job`, `core.User`, `core.Token`, sentinel errors (T1) | T1 | T2, T3, T4, T5, T6, T7, T9 | No — additive, nothing exists yet |
| `store.Open()` read/write handles and `store.Repo` (T2) | T2 | T3, T6 | No |
| `identity.Service.Authenticate()` (T3) | T3 | T7 | No |
| `blob.Store` and `results.Store` (T4) | T4 | T6, T7 | No |
| `bus.Bus` publish/subscribe (T5) | T5 | T6, T7 | No |
| `router.Service` write API (T6) | T6 | T7, T10 | No |
| `httpapi.New()` mounted handler (T7) | T7 | T8, T9, T10 | No |
| `cmd/router` binary (T8) | T8 | T10 | No |
| `ocr.Runner.Run()` and `agent.Loop` (T9) | T9 | T9 (`cmd/worker`) | No |

## Implementation

More than three tasks, so task files are the source of truth: see
[`tasks/README.md`](tasks/README.md). Nine tasks across five waves.

## Consequences

- **Positive:** one process, one file, one writer — the failure modes are enumerable.
- **Positive:** nothing is charged for work the customer never received.
- **Positive:** a worker crash costs one lease timeout, not a lost document; a client crash
  costs nothing, because the backlog is replayed on reconnect.
- **Positive:** the `query_only(1)` read handle makes "read models do not write" a
  driver-enforced invariant rather than a review rule.
- **Negative:** the router is a single point of failure and does not scale horizontally.
- **Negative:** a restart re-OCRs in-flight work, bounded by `buffer_limit × customers`.
- **Negative:** an oversized document can take a balance negative by up to one document.
- **Negative:** `POST /upload` accepts two content types on one path depending on the
  caller's role — the cost of "both speak the same endpoints".
- **Neutral:** SQLite means the router is vertically bounded; swapping to Postgres is a
  `store` change and a DSN, not an architecture change.

## Out of Scope

- Multi-router / horizontal scaling of the router (permanent: boundary: the operator chose exactly one router process on 2026-09-15; the `bus.Bus` interface is the seam if that reverses)
- Persisting OCR results to disk (permanent: boundary: the operator specified results are deleted from memory on delivery, and the source blob already makes the work recoverable)
- Choosing or bundling an OCR engine (permanent: boundary: the operator stated they will specify the exact command; the worker forks whatever `--ocr-cmd` names)
- Customer self-service signup (permanent: boundary: the operator specified that only an admin creates users)
- Payment collection and invoicing (permanent: boundary: this ADR meters credits; how credits are bought is a separate decision nobody has asked for)
- OAuth / JWT authentication (permanent: boundary: a revocable hashed row in the same SQLite file is simpler and nothing here needs offline validation)
- CGO-linked SQLite builds (permanent: fact: the operator required SQLite without cgo, and `modernc.org/sqlite` is the CGO-free driver this workspace has measured; citation: version `modernc.org/sqlite@v1.39.0`)
- Rate limiting per token beyond the per-customer buffer limit (deferred: docs/adr/BACKLOG.md)
- TLS termination and reverse-proxy configuration (permanent: boundary: the router serves plain HTTP and is expected to sit behind a proxy the operator already runs)

## Risks

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| `_txlock=immediate` is a documented **mattn** parameter and may be ignored by `modernc.org/sqlite` | Low | High — silent write contention | Prove by behaviour change, not by docs: T1 asserts both arms (with and without) so the test cannot pass vacuously. The same proof was run in `app-warehousev1` on 2026-09-06 and is being re-run here rather than recalled. |
| goose dialect `sqlite3` vs driver name `sqlite` mismatch | Med | Med — fails at startup | T1's acceptance runs the migration, so the mismatch cannot reach a release. |
| `http.Server.WriteTimeout` silently kills long-lived SSE streams | High | High — streams die mid-session with no error | T6 clears the per-stream deadline with `http.NewResponseController` and asserts a stream survives past the server's configured `WriteTimeout`. |
| A worker holding a lease dies without reporting | Med | Med — job stuck in `processing` | Lease expiry + reaper returns it to `queued`; T5 tests expiry. |
| An oversized document overdraws a customer's balance | Med | Low | Bounded by one document, recorded in the ledger, visible on the dashboard. |
| Aging is written but never actually changes the claim order (integer division truncating to 0, or the term dropped from `ORDER BY`) | Med | High — silent return to strict priority, and the bottom tier starves exactly as it would have without aging | T2 asserts the **overtake** directly: a low-priority job queued long enough is claimed before a fresh high-priority one. A test that only checks "VIP first" passes under both arrangements and would not see this. |
| Clock skew or a non-monotonic wall clock reorders the queue or expires a fresh job | Low | Med | All time arithmetic is unix seconds computed by the router, from one `time.Now()` per claim passed in as a bind parameter — never `strftime('now')` evaluated per row mid-statement. |
| A job expires in the window between being selected and being claimed | Low | Med | The deadline predicate is in the same `WHERE` as the selection, in one statement, so there is no window. |
| **Client-supplied params reach a subprocess argv — command injection** | Med | **Critical** — arbitrary execution on every worker host | No shell anywhere on the path (`exec.Command`, explicit argv, never `sh -c`); keys validated `^[a-z][a-z0-9-]{0,31}$`; each value its own argv element. T9 asserts a value containing `; rm -rf /`, backticks and `$(…)` is delivered to the child **verbatim as one argument** and executes nothing. |
| A param value beginning with `-` is parsed as a flag by the child program | Med | Med | Not solved generally — the child's flag parser is not ours to model. Documented in the ADR and the README: use a `--cmd` that honours `--`, or a wrapper script. Stated rather than silently mitigated. |
| Deriving label validity from live workers rejects a valid upload during a rolling worker restart | Med | Med | A label stays valid for `--label-grace` (default 5m) after its last worker disconnects. ⚠ This mitigation is the author's addition, not the operator's instruction. |
| A label that no worker has **ever** advertised is accepted and the job sits until its deadline | Low | Med | Upload rejects an unknown label with `400` **and the list of available labels**, so the failure is immediate and self-describing rather than a silent 30-minute wait. |
| A worker could set what customers are charged if rates were derived from workers | Low | **High** | Rates are admin-owned in `service_rates`; the worker advertises a label only. This asymmetry with label validity is deliberate and stated in the Decision. |
| A crawler-style job with no blob is treated as a corrupt upload by code assuming a file | Med | Med | `jobs.has_blob` is explicit rather than inferred from a nullable path, and T6 asserts both a blob job and a params-only job through the whole lifecycle. |
| **A pipeline stage advances but the client is notified early**, delivering a half-processed intermediate as the final result | Med | High — the customer is charged for and receives the wrong artifact | `ready` is published **only** when `stage == len(pipeline)-1`. T6 asserts that a two-stage job publishes exactly one `ready`, after the second stage — a test that counts events, because "a ready was published" passes in both arrangements. |
| A pipeline whose next label has no live worker stalls silently mid-chain | Med | Med | The whole-pipeline `expires_at` still applies, so it fails at the deadline rather than never. Upload validates only the **first** label against live workers — a later stage's workers may legitimately start later — so this is a real residual, and the dashboard shows per-label queue depth to make it visible. |
| Pipeline jobs starve behind fresh single-stage jobs | Med | Med | `queued_at` is preserved across stages, so a half-done pipeline keeps its accrued age and is preferred. Asserted in T6 alongside the retry case, which shares the mechanism. |
| Intermediate blobs accumulate for abandoned pipelines | Low | Med | Each stage replaces the previous stage's blob and deletes it after the successful advance; the terminal paths (`delivered`/`dead`/`expired`) delete the last one. |
| A stage output containing newlines is corrupted by the `\n` join into the next stage's blob | Med | Med | Documented as the author's encoding choice rather than hidden. Single-element outputs — the common pipeline case — are written verbatim and unaffected. Revisit if a multi-element intermediate becomes real. |

| datastar v1 attributes written from model priors (`data-on-load`, hyphenated events) fail **silently** | High | Med — the dashboard looks empty with no console error | T9 uses only the cached v1 surface; the SSE subscription is opened with `data-init`. |
| Two content types on one `POST /upload` path invite a role-confusion bug | Low | High — a client could post a fabricated result | The role is read from the authenticated token, never from the body; T3 and T7 assert a client token posting a worker payload is rejected. |

## Rollback

Persistent state is created, so rollback is defined rather than `None`:

1. Stop `cmd/router` and every `cmd/worker`.
2. The schema is one migration: `goose sqlite3 <db> down` to `00000` drops `users`,
   `tokens`, `jobs`, `credit_entries`. There is no prior schema to restore to — before this
   ADR the repository had no database at all.
3. Delete the blob directory named by `--blobs`.
4. In-memory results need no rollback; they do not survive the process.

No external integration is created, so nothing outside the repository has to be reverted.
Rollback is lossless with respect to anything that existed before, because nothing did.

## Follow-ups

- [ ] Operator to supply the exact OCR command and confirm the stdout contract
      (`JSON []string`, one entry per page).
