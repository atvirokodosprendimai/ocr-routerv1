# ocr-routerv1

A job router: customers upload work over an HTTP API, workers anywhere on the
internet claim it by **label**, and results are metered in credits.

**OCR is the default label, not the product.** A worker is a generic subprocess
runner — it downloads an input, forks whatever command you configured, and posts
the command's stdout back. `ocr` is simply the label it serves by default.

The design and its reasoning are in
[`docs/adr/0001-ocr-router-architecture.md`](docs/adr/0001-ocr-router-architecture.md).

## Running it

```sh
go build -o router ./cmd/router

# Create the first administrator. This is the only way into a fresh system:
# only an admin can create users, and a new database has none.
./router admin bootstrap --email you@example.com --db ocr.db --blobs ./blobs
# → token: ocr_a_…    (shown ONCE; it is stored only as a hash)

./router --db ocr.db --blobs ./blobs --addr :8080
```

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
```

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
  not.
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
