# ADR-0005 tasks

Derived index — the task files are the source of truth. Execute in order.

| Task | Goal | Status | Depends-on | Acceptance (first command) |
|------|------|--------|------------|----------------------------|
| T1 | The protocol — stream first, then upload, then wait | pending | none | `go test ./internal/client/... -race` |
| T2 | The binary a person runs — flags, progress, exit codes | pending | T1 | `go test ./cmd/client/... -race` |

## Order

    T1 ── T2

T1 is the protocol with no opinions about terminals; T2 is the binary with no opinions about HTTP.
The same split `internal/agent` and `cmd/worker` already use.

## The one ordering that matters

```
open GET /sse  →  wait for hello  →  POST /upload  →  wait for ready(job_id)  →  GET /files/{id}
```

⚠ **Uploading first is what everyone writes, and it is racy.** A fast job can finish between the
upload returning and the stream being established; the `ready` event fires into a stream nobody is
holding, and the client waits forever for an event that already happened. The window is small —
which is worse than large, because it passes every casual test and then strands the caller in
production on exactly the jobs that went well.

Waiting for `hello` is part of the fix, not decoration: a dialled connection the handler has not yet
accepted is not subscribed to the bus, so "connected" is not the same as "subscribed".

## Exit codes name an ACTION

A script branches on what to do next, not on what went wrong:

| 0 | wrote the result | carry on |
|---|---|---|
| **1** | you must fix something — bad token, no credits, missing file | do not retry |
| **2** | try again later — rate limited, router down, timed out | retry with backoff |
| **3** | the job ran and failed — dead or expired | investigate |

⚠ **Exit 0 on a failed job is the failure that matters most.** A client that writes an empty file
and exits 0 because "the request succeeded" turns a dead job into silent data loss in whatever
pipeline called it.

## The risk this record does not solve

**Behind a reverse proxy that buffers responses, this client hangs rather than failing.** SSE is the
right transport for the router's design and the wrong one to be certain about in an unknown
deployment. `--timeout` bounds it; a `--poll` fallback is deferred rather than dismissed, and both
task files carry a Stop Condition asking the operator about their proxy before this ships.

## Convention

Status here is derived from each task's `## Verification Log`: a task may be marked `done` only once
`adr-verify` has recorded an exit-0 entry whose digest matches the task's current Acceptance fence,
plus at least one killed mutant bound to the same digest. Do not hand-edit either log.
