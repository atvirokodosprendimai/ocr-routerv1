# ocr-routerv1

A job router: customers upload work over an HTTP API, workers anywhere on the
internet claim it by **label**, and results are metered in credits.

**OCR is the default label, not the product.** A worker is a generic subprocess
runner — it downloads an input, forks whatever command you configured, and posts
the command's stdout back. `ocr` is simply the label it serves by default.

The design and its reasoning are in
[`docs/adr/0001-ocr-router-architecture.md`](docs/adr/0001-ocr-router-architecture.md).

## First run

```bash
# 1. Create the first administrator. Omit --password to be prompted instead;
#    a password on the command line is in your shell history and in `ps`.
router admin bootstrap --email you@example.com --password '<12+ characters>'
#    → prints a bearer token, once. Save it: the API needs it, and it is not
#      recoverable afterwards.

# 2. Run the router behind TLS.
router --addr :8080

# 3. Open https://your-host/admin and sign in with the email and password.
```

⚠ **TLS is required to sign in.** The session cookie is `Secure`, so a browser
discards it over plain `http://` — the login would appear to succeed and then
loop back to the form forever. The login page refuses plain HTTP outright and
says so, rather than letting that happen silently. If TLS terminates in a proxy,
that proxy must set `X-Forwarded-Proto: https`.

**For local development without TLS**, add `--insecure-cookies`:

```bash
router --addr 127.0.0.1:8080 --insecure-cookies
# → ⚠ --insecure-cookies is SET: the session cookie has no Secure attribute …
```

It drops `Secure` and nothing else — `HttpOnly`, `SameSite=Strict` and the
`/admin` path scope are unchanged, because the transport is a separate question
from the cookie's reach. The router prints a warning on every boot while it is
set, since a dangerous flag documented only in `--help` is one somebody turns on
for an afternoon and leaves on. **Never set it on anything reachable from a
network**: the session cookie then travels in clear text, and anyone who sees it
is an administrator.

Change a password later with `router admin set-password --email you@example.com`.
There is no reset-by-email flow: this system has no email channel, and an
operator with shell access has this command.

## Signing in

| | |
|---|---|
| **Who can** | Administrators only. Clients and workers have no password and no UI — the API's credential is the bearer token |
| **Session length** | 12 hours, absolute. It is **not** extended by use, so a tab left open still ends on time |
| **Signing out** | Revokes server-side, so it ends the session in **every** tab. That is deliberate, not a bug |
| **Wrong password** | One generic message, whatever the cause. The router will not tell a caller which emails are registered |

## Running it

```sh
go build -o router ./cmd/router

# Create the first administrator. This is the only way into a fresh system:
# only an admin can create users, and a new database has none.
./router admin bootstrap --email you@example.com --db ocr.db --blobs ./blobs
# → token: ocr_a_…    (shown ONCE; it is stored only as a hash)

./router --db ocr.db --blobs ./blobs --addr :8080
```

## The client

`cmd/client` submits one document and blocks until the result lands, so a
customer does not have to implement the four-step protocol below by hand.

```bash
export OCRR_TOKEN=ocr_c_…          # never --token in a shell you keep history for
client --router https://ocr.example.com -i scan.pdf -o result.txt
# uploading…
# waiting for 0192f1a2…
# done
```

| | |
|---|---|
| `-i FILE` | the document. Omit it for a params-only job — `--param url=…` — where the service fetches its own input |
| `-o FILE` | where to write the result. `-` is **stdout**, so `client … -o - \| wc -l` composes |
| `--label` / `--pipeline a,b,c` | which service, or an ordered chain. `--pipeline` wins |
| `--param k=v` | repeatable; becomes a subprocess flag on the worker |
| `--json` | the result envelope instead of newline-joined text |
| `--quiet` | no progress; errors still print |
| `--timeout` | give up after this long. **Zero — the default — waits forever**, because a queued job behind a busy pool is supposed to take a while |

**Progress goes to stderr, always**, which is what makes `-o -` safe to pipe. It
rewrites one line in place on a terminal and prints one plain line per step when
it is not.

### Exit codes say what to do next

| | | |
|---|---|---|
| **0** | the result was written | carry on |
| **1** | fix something — bad token, no credits, missing file, bad flag | do not retry |
| **2** | try again later — rate limited, router down, timed out | retry with backoff |
| **3** | the job ran and **failed** — dead or expired | investigate; the reason is on stderr |

⚠ **A failed job never exits 0 and never writes an output file.** An empty file
plus a success code turns a dead job into silent data loss in whatever pipeline
called it.

⚠ **This client needs SSE to arrive promptly.** It holds `GET /sse` open for the
life of the job. Behind a reverse proxy that **buffers** responses it will hang
rather than fail, which is the worst shape a failure can take — use `--timeout`
if your proxy is unknown.

## The API

Every request carries `Authorization: Bearer <token>`. The token's row decides
what you may do — the `ocr_a_` / `ocr_c_` / `ocr_w_` prefix is a human-readable
label for judging a leak, and carries no authority.

| Method | Path | As a client | As a worker |
|---|---|---|---|
| `POST` | `/upload` | multipart `file`, plus query params → `201 {job_id}` | JSON result → `204` |
| `GET` | `/sse` | command stream: `hello`, `backlog`, `ready`, `failed`, `ping` | `?label=<l>` → `hello`, `work`, `ping` |
| `GET` | `/files/{id}` | the result JSON; **deleted and charged on success** | the source file; requires holding the lease |
| `POST` | `/claim` | — | `?label=<l>` → `200 {job_id,…}` or `204` when idle |
| `GET` | `/services` | the labels currently available | — |
| `GET` | `/healthz` | **no token** — `200 {"ok":true,…}` or `503` naming what broke | same |
| `GET`/`POST` | `/admin/login` | **no token** — the dashboard's sign-in page | — |

### Uploading

```sh
# A file through the default service
curl -H "Authorization: Bearer $CLIENT" -F file=@scan.pdf http://localhost:8080/upload

# A specific service, with parameters for its subprocess
curl -H "Authorization: Bearer $CLIENT" -F file=@page.html \
     'http://localhost:8080/upload?label=strip-html'

# No file at all — the crawler shape. Everything but `label` and `pipeline`
# becomes a parameter passed to the worker's subprocess.
curl -X POST -H "Authorization: Bearer $CLIENT" \
     'http://localhost:8080/upload?label=crawl&url=https://example.com&max-depth=2'

# A pipeline: each stage's output becomes the next stage's input.
curl -X POST -H "Authorization: Bearer $CLIENT" \
     'http://localhost:8080/upload?pipeline=crawl,strip-html&url=https://example.com'
```

A pipeline is **one job** from your side: one id, one `ready`, one charge.

### Collecting

Open `GET /sse` and wait for `ready`, then `GET /files/{id}`. The result is JSON
`{"job_id":…, "units":[…]}` and is **deleted from memory** once served.

You do not have to be listening when the job finishes: every stream begins with
a `backlog` event listing everything waiting for you.

## Credits

One output unit costs one credit by default — the operator's "1 page = 1
credit". A per-service rate can change that (`service_rates`), and a pipeline
accrues each stage at its own rate.

You are charged **once, when you collect the result**, in the same transaction
that marks the job delivered. Nothing is charged for a job that fails, expires,
or is never collected. A document larger than your remaining balance is still
delivered, taking the balance negative by at most that one job.

Uploads are refused with `402` when your balance is not positive, and with `429`
when you already have `buffer_limit` jobs in flight.

## Priority and deadlines

Each customer has an integer `priority` (higher first) and an optional
`job_ttl_secs`. The queue orders by an **aged** priority:

```
effective = priority + (now - queued_at) / aging_step
```

so a long-waiting low-priority job eventually overtakes a fresh high-priority
one and nothing starves. A job still queued past its deadline is `expired`,
uncharged. A job a worker has already started runs to completion regardless.

## Flags

```
--addr           listen address                                   (:8080)
--db             SQLite path                                      (ocr-router.db)
--blobs          directory for source files                       (blobs)
--result-ttl     how long an uncollected result is held           (1h)
--lease          how long a worker holds a job                    (5m)
--max-attempts   attempts before a job is abandoned               (3)
--aging-step     waiting time worth one point of priority         (1m)
--label-grace    how long a service outlives its last worker      (5m)
--reap-interval  how often expiries and sweeps run                (30s)
--max-upload     maximum upload size in bytes                     (64MiB)
--default-label  the service used when none is named              (ocr)
--metrics-addr   PRIVATE listener for /metrics                    (127.0.0.1:9090)
--rate-client    client requests/second per token, 0 = off        (10)
--rate-worker    worker requests/second per token, 0 = off        (30)
--rate-admin     admin requests/second per token, 0 = off         (30)
--rate-burst     requests allowed at once before the rate applies (20)
--rate-idle      how long a silent token's bucket is kept         (10m)
--log-level      debug, info, warn or error                       (info)
--log-format     json or text                                     (json)
```

## Rate limits

Limits are **per bearer token**, not per customer — `buffer_limit` already meters
per customer, one axis over, and the threat this addresses is a *leaked
credential*: a per-user limit would let a compromised token eat the legitimate
one's allowance.

They are an **abuse ceiling, not a quota.** The defaults sit far above any
realistic integration; they exist so one token cannot monopolise the single
write connection, not to shape what customers may do. Over the limit you get:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 2

{"error":"rate limited"}
```

`Retry-After` is always at least 1 second and is the real time until your next
token. Set any `--rate-*` flag to `0` to disable limiting for that role — that is
the rollback, and it needs no redeploy.

⚠ **Unauthenticated requests are not rate limited here.** A caller with no token
is rejected before any database work, and defending the socket against a
credential-less flood is a reverse proxy's job. ⚠ **The limiter is per process**,
so if you ever run more than one router the effective limit is multiplied by the
replica count.

## Logs

`log/slog` JSON on stdout. Two shapes.

One line per request, covering the 401s and 429s too:

```json
{"time":"2026-09-15T12:00:00Z","level":"INFO","msg":"request","method":"POST",
 "route":"/files/{id}","status":200,"duration":12000000,
 "user_id":"0192f…","token_id":"0192a…","role":"client"}
```

One line per job state transition, which is what makes a single failure
readable:

```json
{"time":"2026-09-15T12:05:00Z","level":"INFO","msg":"transition",
 "job":{"id":"0192f…","user_id":"0192a…","label":"crawl","params":["depth","url"]},
 "from":"queued","to":"processing","actor":"worker","attempt":0,"stage":0,
 "in_state":300000000000,"worker_id":"w-3"}
```

`in_state` is how long the job spent in the state it just left — the field that
turns *"the job died"* into *"it sat queued for five minutes and then failed in
two seconds"*. It is computed from stored timestamps, so it stays correct across
a restart.

⚠ **Job parameter VALUES are never logged — only their keys.** This is a
guarantee you can rely on, and it is enforced by the shape of the code rather
than by convention: no function in the logging package accepts a value, so no
call site can leak one by forgetting. A crawler's `?url=` may carry credentials,
and `params` will show `["url"]` and never its contents. Bearer tokens are never
logged either; `token_id` is a database key, not the secret.

The route **pattern** is logged rather than the path, so `/files/{id}` groups in
an aggregator instead of producing one unique line per job.

Use `--log-format text` for a human at a terminal. The three startup lines stay
plain `fmt.Printf` deliberately, so a misconfigured logger cannot make the
process look dead at boot.

## Health and metrics

`GET /healthz` is on the main listener and is the **only** unauthenticated route
in the process, because a load balancer cannot hold a bearer token. It does real
work: a `SELECT 1` through the read handle, and a create-write-sync in the blob
directory. A full or read-only volume is a failure a `SELECT` would not catch —
the database can be perfectly healthy while every upload is about to fail.

```json
{"ok": false, "version": "v1.2.3", "failed": ["blobs"]}   // 503
```

⚠ **It deliberately ignores worker availability.** Zero workers for a label means
the queue is filling and the router is fine. Reporting that as unhealthy would
make an orchestrator restart the one component still working — and the restart
would fix nothing, so it would do it again. That signal is a metric and an alert,
not a liveness input.

`GET /metrics` is Prometheus text format on a **separate listener**, bound to
loopback by default. Queue depth and throughput say how much work each customer
is pushing, and the main listener faces the internet. The obvious alternative —
`/metrics` behind the admin token — works, but it puts a credential that creates
users and mints tokens into a scrape config, which is the least-guarded file in
most deployments. Point `--metrics-addr` elsewhere and exposing it becomes a
deliberate act in your proxy.

| Metric | Type | What it answers |
|---|---|---|
| `ocrr_workers_live{label}` | gauge | ★ **is anybody serving this service?** A label at 0 queues silently until its jobs hit their deadline; nothing else reports it |
| `ocrr_queue_oldest_age_seconds{label}` | gauge | ★ a better stall signal than depth — depth sits low while one job is wedged |
| `ocrr_queue_depth{label}` | gauge | backlog per service |
| `ocrr_jobs_current{state}` | gauge | census by state |
| `ocrr_results_in_memory` | gauge | uncollected results held in RAM |
| `ocrr_jobs_total{state}` | counter | terminal outcomes |
| `ocrr_stage_advances_total` | counter | pipeline movement; a pipeline that stopped advancing looks exactly like a slow one |
| `ocrr_reaper_actions_total{action}` | counter | requeues, expiries and sweeps — a reaper that silently stopped is otherwise invisible |
| `ocrr_credits_debited_total` | counter | should agree with the ledger |

Metric label **names** are restricted in code to `label`, `state` and `action`.
A user id or email would be unbounded, and unbounded label values are how a
metrics endpoint kills the scraper it feeds; the per-customer question is
answered by the authenticated dashboard.

Alert on `ocrr_workers_live == 0` for a label with a non-zero
`ocrr_queue_depth`, and on `ocrr_queue_oldest_age_seconds` above whatever your
deployment considers late. No thresholds ship here: a threshold is valid for a
deployment, and this repository does not know yours.

## Operational notes

- **The router is a single process.** Clients and workers scale out; it does
  not. The rate limiter lives in that process's memory, so running two routers
  would silently double every limit — that is the first thing to change if
  replication is ever on the table.
- **Results live only in memory.** A restart returns in-flight jobs to the
  queue and they are redone from the source file on disk. Since nothing is
  charged before collection, a restart costs repeated work and never money.
- **Which services exist is derived from connected workers.** Starting a worker
  is what makes its label available; it stays available for `--label-grace`
  after the last one disconnects, so a rolling restart does not reject uploads.
  `GET /services` is the authoritative answer.
- **Do not put a `WriteTimeout` on the server.** It applies to the whole
  response, which for an SSE stream means the connection dies mid-session with
  nothing in the logs. The stream handler clears its own deadline, but the
  server should not set one either.
