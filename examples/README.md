# The client protocol

How a client submits a job and gets its result back, in enough detail to write
one from scratch in any language.

There are two reference implementations to read alongside this:

| | |
|---|---|
| **Go** — [`../client`](../client) | The authoritative one. It is tested against the real router, and where this document and that package disagree, **the package is right**. |
| **Python** — [`python/client.py`](python/client.py) | Standard library only, no dependencies. Copy it into your project. |

Everything below is HTTP and server-sent events. There is no SDK you must use,
no websocket, and no polling.

---

## The shape of it

A job is submitted over HTTP and **announced over SSE**. The result is fetched
with a third request. That split is the thing to understand first, because it is
what makes every other rule follow:

```
         ┌─────────────────────────────────────────────┐
         │ 1. GET /sse        open FIRST, wait for     │
         │                    `hello`                  │
         └─────────────────────────────────────────────┘
                              │  stream stays open
                              ▼
         ┌─────────────────────────────────────────────┐
         │ 2. POST /upload    → 201 {"job_id": "..."}  │
         └─────────────────────────────────────────────┘
                              │
                              ▼
              ┌───────────────────────────────┐
              │  wait on the stream you        │
              │  already hold                  │
              └───────────────────────────────┘
                     │                   │
          event: ready              event: failed
                     │                   │
                     ▼                   ▼
    ┌───────────────────────────┐   ┌──────────────────┐
    │ 3. GET /files/{job_id}    │   │ the job died.    │
    │    → units, or raw bytes  │   │ no result exists │
    │    ⚠ charges credits      │   └──────────────────┘
    └───────────────────────────┘
```

**The SSE stream carries ids and counts, never content.** A `ready` event tells
you *that* a job finished; you fetch the bytes yourself. That is deliberate: it
is what makes two events racing for one job resolve correctly, and it keeps a
large result off a connection that has to stay responsive.

---

## ⚠ The one rule that is not optional

**Open the stream and wait for `hello` BEFORE you post the upload.**

Get this backwards and your client works perfectly in testing and hangs forever
in production, on exactly the jobs that went well.

Here is the race. If you upload first, a fast job can finish before your stream
is subscribed. The router fires `ready` into a stream nobody is holding. The
event is not queued and not resent. Your client then waits for an event that has
already happened:

```
   WRONG                                RIGHT
   ─────                                ─────
   POST /upload      ──┐                GET  /sse       ──┐
                       │ job finishes                     │ wait for `hello`
   GET /sse          ◄─┘ HERE           POST /upload    ◄─┘
   ...waits forever                     ...event arrives
```

The window is small. **That is worse than large**, not better: a big window
fails on your first test, a small one passes every test you will think to write
and then strands a customer at 3am.

**`hello` is the signal, not the TCP connect.** The router subscribes your
stream to its event bus *inside* the handler. A socket that has connected but
whose handler has not run yet is not receiving anything. So: read frames until
you see `hello`, and only then upload.

---

## Authentication

Every request carries a bearer token:

```
Authorization: Bearer ocr_c_...
```

The prefix tells you what a token is for — `ocr_c_` client, `ocr_w_` worker,
`ocr_a_` admin — but it is a readability aid, not the authority. The token's row
in the database decides what you may do. A client token cannot claim work and a
worker token cannot upload.

Tokens are shown **once**, when created, and stored only as a hash. There is no
recovery.

---

## 1 · `GET /sse` — open the stream

```http
GET /sse HTTP/1.1
Authorization: Bearer ocr_c_...
Accept: text/event-stream
```

Response headers:

```http
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
X-Accel-Buffering: no
```

Then frames arrive, in this order:

### `hello` — always first

```
event: hello
data: {"role":"client","user_id":"u_01H..."}
```

This is your go-ahead. The stream is subscribed; upload now.

### `backlog` — immediately after `hello`, for clients

```
event: backlog
data: {"jobs":[{"job_id":"0192f1a2...","units":12}]}
```

**This is what makes a dropped stream survivable, and it is the part most
hand-written clients skip.** It names jobs that are *already finished and
uncollected*. If your connection died while a job was running, no live `ready`
will ever be sent again — the event fired when you were not listening. The
backlog is how you find out.

So after uploading, check the backlog you captured at connect time **before**
waiting for a live event. If your job id is in it, go straight to collecting.

### `ready` — a job finished

```
event: ready
data: {"job_id":"0192f1a2...","units":12,"label":"ocr"}
```

### `failed` — a job died

```
event: failed
data: {"job_id":"0192f1a2...","reason":"worker exited with status 2"}
```

There is no result to fetch. Do not call `/files/{id}`.

### `ping` — keepalive, every 15 seconds

```
event: ping
data: {"t":1727600000}
```

Ignore the payload. Its value is that **a silent stream and a dead one look
different**: if pings stop arriving, the connection is gone even though the
socket may not have noticed yet.

### ⚠ Parsing: reassemble, do not read lines

An SSE frame is `event:`, then `data:`, then a **blank line**. The transport
guarantees nothing about how those land in your reads — two frames can arrive in
one TCP segment, and one frame can span two.

A reader that treats each read as a frame **drops events under exactly the load
that makes them matter**. Buffer until the blank line. Lines beginning with `:`
are comments and are ignored.

### ⚠ One stream, every job

The stream carries **every job belonging to your customer account**, not just
the one you are waiting on. If you have two in flight you will see both.

**Match on `job_id`, always.** Acting on the wrong event collects someone else's
result — charged to them — or fails on their failure.

### ⚠ Buffering proxies

A reverse proxy that buffers responses will hold an event stream indefinitely,
and your client will **hang rather than fail** — the worst shape a failure can
take. The router sends `X-Accel-Buffering: no`, which nginx honours. If your
proxy is unknown, set a timeout so a hang becomes an error.

---

## 2 · `POST /upload` — submit the job

```http
POST /upload?label=ocr&raw=0 HTTP/1.1
Authorization: Bearer ocr_c_...
Content-Type: multipart/form-data; boundary=...

(the file, under the part name "file")
```

```http
HTTP/1.1 201 Created
Content-Type: application/json

{"job_id":"0192f1a2..."}
```

### Query parameters

| Parameter | Meaning |
|---|---|
| `label` | which service runs the job |
| `pipeline` | a comma-separated chain, `ocr,translate` — each stage's output becomes the next stage's input. **Takes precedence over `label`** when both are sent |
| `raw` | `1` or `0`, see below |
| *anything else* | passed through to the worker's subprocess as a flag |

**Any parameter you invent becomes an argument to the service's subprocess.**
That is the extension point: a service that takes `--lang` is driven with
`?lang=de`. Keys are validated; an invalid one is a `400`.

### The file is optional — the crawler shape

A job with **no body at all** is legitimate and supported:

```http
POST /upload?label=crawl&url=https%3A%2F%2Fexample.com HTTP/1.1
Authorization: Bearer ocr_c_...
```

Here the job *is* its parameters and the service fetches its own input. A client
that insists on a file cannot express this. Send no body and no `Content-Type`.

⚠ **In Python, this bites you.** `urllib` stamps
`Content-Type: application/x-www-form-urlencoded` on any request whose `data` is
not `None` — empty bytes included. The router chooses its upload branch off
`Content-Type`, so that header is a claim about a body that does not exist. Pass
`data=None`, not `data=b""`.

### `raw` — send it in both modes

`raw=1` asks for a service whose output is **opaque bytes** rather than a list of
text units: an image, a PDF, a binary.

Send `raw` explicitly either way, including `raw=0`. Omitting it means the same
thing as `0`, but an explicit value turns a mismatch into a disagreement between
two *stated* positions — which is what makes the router's refusal legible.

⚠ **You cannot choose the mode.** It belongs to the service's admin-owned
record, because **the mode is a price**. Asking for `raw=1` against a units
service is a `409 Conflict`, and that is the system working.

---

## 3 · `GET /files/{job_id}` — collect the result

```http
GET /files/0192f1a2... HTTP/1.1
Authorization: Bearer ocr_c_...
```

Two possible responses:

```http
HTTP/1.1 200 OK
Content-Type: application/json

{"job_id":"0192f1a2...","units":["page one","page two"]}
```

```http
HTTP/1.1 200 OK
Content-Type: application/octet-stream

(raw bytes, verbatim)
```

### ⚠ Branch on the response, never on your request

Decide which you got by reading the **response's** `Content-Type`, not by
remembering whether you asked for `raw`.

The two disagree exactly when something is wrong, and that is the case worth
surfacing rather than misreading. A client that assumes its own request was
honoured will decode a JSON envelope as bytes and hand the caller a file
containing `{"job_id":...}` — calling it the result.

### ⚠ This call charges credits, and consumes the result

Results live **in memory** with a TTL. Fetching one charges the account and
removes it. So:

- Call it **once**, and only after `ready` (or a backlog entry) for that job.
- Never call it speculatively or in a poll loop.
- A restart loses uncollected results — the router re-queues those jobs from the
  stored source blob, so they come back, but the in-flight result does not.

---

## Errors: what to do next

The status code is a **decision**, not a taxonomy. The split is about what your
next action should be:

| Status | Meaning | Retry? |
|---|---|---|
| `400` | bad parameter, bad state | **No** — fix the request |
| `401` | bad or missing token | **No** |
| `402` | out of credits | **No** — retrying an empty balance never succeeds |
| `403` | your role may not do this | **No** |
| `404` | no such job, or not yours | **No** |
| `409` | raw/units mode mismatch | **No** — the service's mode is admin-owned |
| `429` | rate limited, or too many jobs in flight | **Yes**, after `Retry-After` |
| `5xx` | the router is unwell | **Yes**, with backoff |

⚠ **Model these as two distinct error types in your client, not one.** A single
"request failed" leaves a script unable to tell a pointless retry from a correct
one — and retrying a `402` forever is a way of not noticing you are out of
credits.

`429` covers two different limits — requests per second per token, and jobs in
flight per customer. `Retry-After` is what distinguishes them on the wire.

Every error body is `{"error":"..."}`. A `500` deliberately does **not** echo
the underlying message: an unmapped error is by definition one nobody reasoned
about, and its text could carry a path, a SQL fragment, or another customer's
data.

### The job failed vs the request failed

These are different and your client should let a caller tell them apart:

- **`failed` event** — the job ran and died. There is no result. This is not an
  HTTP error; the submission succeeded.
- **HTTP 4xx/5xx** — the request was refused.

⚠ **A failed job must never look like success.** Writing an empty output file
and exiting `0` turns a dead job into silent data loss in whatever pipeline
called you. The reference clients raise a distinct `FailedError` and exit `3`.

---

## Writing your own client: the checklist

Work through this and you will have a correct one:

- [ ] Open `/sse` and **wait for `hello`** before uploading.
- [ ] Reassemble SSE frames on the blank line; do not treat a read as a frame.
- [ ] Capture the `backlog` frame at connect and check it after uploading.
- [ ] Match **every** event on `job_id`.
- [ ] Handle `ping` by ignoring it — but notice when pings stop.
- [ ] Send `raw` explicitly in both modes.
- [ ] Send no body *and no `Content-Type`* for a params-only job.
- [ ] Branch the result on the **response's** `Content-Type`.
- [ ] Call `/files/{id}` exactly once, only after `ready`.
- [ ] Split errors into retryable and fatal, and make `failed` its own type.
- [ ] Bound the whole operation with a timeout — but a generous one, and never
      one so short it kills a legitimately long job.

---

## Running the Python example

```bash
export OCRR_TOKEN=ocr_c_...

python3 python/client.py --router https://ocr.example.com -i scan.pdf -o out.txt
python3 python/client.py --router https://ocr.example.com -i scan.pdf -o -     # stdout
python3 python/client.py --router https://ocr.example.com --label crawl --param url=https://example.com -o -
```

Exit codes match the Go CLI: `0` result written · `1` fix something · `2` retry
later · `3` the job ran and failed.

Its tests run with no dependencies and no router:

```bash
cd python && python3 test_client.py
```

They drive the real client against a stub that speaks the wire format — the
ordering rule, backlog replay, job-id isolation, the raw/units branch, frame
reassembly and the retry split. ⚠ The stub is **hand-written from the router's
source**, so the tests prove the client is self-consistent with this document.
They cannot prove this document matches the router; only the Go package's tests
do that.
