# Task ADR-0001-T8: Wire the composition root into cmd/router and prove the whole path end to end

**Depends-on:** T7
**Covers:** none — no spec
**Estimated scope:** L (cross-boundary)
**Owner:** unassigned
**Produces:** the `cmd/router` binary, `router.Config`, the admin bootstrap command
**Consumes:** `httpapi.New()` (T7), `router.Service` (T6), `store.Open()` (T2), `identity.Service` (T3), `blob.Store` and `results.Store` (T4), `bus.Bus` (T5)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the composition root`, `the reaper ticker`, `the graceful shutdown`

## Goal

Assemble every component into a running binary, and prove with one test that a file
uploaded by a real client is claimed by a real worker, OCR'd, delivered and charged.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `cmd/router/main.go` | add | flags, **the composition root**, HTTP server, shutdown |
| `cmd/router/wire.go` | add | `buildHandler(Config) (http.Handler, func(), error)` — constructed once, called by both `main` and the tests |
| `cmd/router/main_test.go` | add | composition-root and end-to-end tests |
| `README.md` | edit | how to run the router, mint the first admin token, and the three client URLs |

`wire.go` exists so the end-to-end test builds the graph **the same way the binary does**.
A test that assembles its own dependency graph cannot see a composition-root defect, and
that is the class of bug no unit test in T2–T7 can reach.

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write the
   end-to-end test asserting upload → claim → complete → deliver moves the balance, before
   any implementation (TDD red). [proof: acceptance]
2. [S2] `urfave/cli/v3` flags: `--addr` (`:8080`), `--db`, `--blobs`, `--result-ttl` (1h),
   `--lease` (5m), `--max-attempts` (3), `--aging-step` (60s), `--reap-interval` (30s),
   `--max-upload` (64MiB). Every one has a default that works.
3. [S3] `buildHandler` opens the store, runs migrations, constructs `blob`, `results`,
   `bus`, `identity` and `router.Service`, calls `RecoverOnBoot`, and returns
   `httpapi.New(deps)` plus a cleanup func. **One place** builds the graph.
4. [S4] `main` starts the reaper ticker calling `router.Reap(now)` every `--reap-interval`,
   in a goroutine cancelled by the server's context.
5. [S5] The `http.Server` sets `ReadHeaderTimeout` but **leaves `WriteTimeout` at zero**,
   and the SSE handler clears its per-stream deadline anyway (T7 S9) — belt and braces,
   because a later operator adding `WriteTimeout` for good reasons must not break streams.
6. [S6] `ocr-router admin bootstrap --email <e>` creates the first admin and prints its
   token once. Without it the system has no way in: only an admin can create users, and
   there is no admin until one is made outside the API.
7. [S7] Graceful shutdown on `SIGINT`/`SIGTERM`: stop accepting, cancel stream contexts,
   `Shutdown` with a timeout, then the cleanup func. In-memory results are lost by design —
   their jobs return to `queued` on the next boot (T6 S8).

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go vet ./... \
  && test -z "$(gofmt -l . )" \
  && go test ./cmd/router/... -count=1 -race 2>&1 | tee /tmp/adr1-t8.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t8.out \
  && go test ./internal/... -count=1 2>&1 | tee /tmp/adr1-t8r.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t8r.out
```

The end-to-end package runs first and alone so it can carry the verdict; every internal
package runs second as regression. This is the one task whose fence is repository-wide,
because assembling everything is what it proves. Red at authoring: `cmd/router` does not
exist.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestEndToEndUploadToDelivery` | `cmd/router/main_test.go` | against a server built by `buildHandler`: a client uploads, a worker claims, downloads the blob, posts pages, the client's SSE stream receives `ready`, the client GETs the result and the balance drops by the page count | — | S3 |
| `TestEndToEndUsesBuildHandler` | `cmd/router/main_test.go` | the test server is built by the same `buildHandler` `main` calls — asserted by construction, and the reason the composition root is covered at all | — | S3 |
| `TestReaperRunsInBinary` | `cmd/router/main_test.go` | with a tiny `--reap-interval` and an expired lease, the job returns to `queued` **without the test calling `Reap`** — goes red when the ticker in S4 is deleted, which every `internal/router` test would survive | — | S4 |
| `TestRecoverOnBootRequeuesAcrossRestart` | `cmd/router/main_test.go` | build, upload, claim, tear down, rebuild on the same file → the job is `queued` and nothing was charged | — | S3, S7 |
| `TestBootstrapCreatesAdminOnce` | `cmd/router/main_test.go` | the command prints a token and creates exactly one admin; a second run on the same email is refused | — | S6 |
| `TestBootstrapTokenAuthenticates` | `cmd/router/main_test.go` | the printed token actually authenticates against the built handler — the token is useless if it does not, and printing one proves nothing | — | S6 |
| `TestSSESurvivesDefaultServerConfig` | `cmd/router/main_test.go` | a stream on the server `main` configures stays open past `--reap-interval` and receives a ping | — | S5 |
| `TestFlagDefaultsAreUsable` | `cmd/router/main_test.go` | with no flags but `--db`/`--blobs` pointed at a temp dir, the handler builds and serves | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the eight tests above |
| 2 — something selects it | `main` calls `buildHandler`; `TestReaperRunsInBinary` and `TestEndToEndUsesBuildHandler` are the checks that go red if the wiring is removed. This is the rung the whole task exists to cover for T2–T7. |
| 3 — the caller can discover it | `--help` lists every flag; the README documents bootstrap and the three client endpoints |
| 4 — it is used | it is the product; the dashboard (T10) and the worker (T9) both talk to this binary |

## Mutation Log

## Invariants

- The dependency graph is built in exactly one place, and the tests use it.
- `WriteTimeout` is zero on the server, and the SSE handler clears its deadline regardless.
- Nothing is charged during boot recovery.
- The bootstrap command is the only way to create the first admin, and it prints the token
  exactly once.

## Risks

- **A test that builds its own graph proves nothing about the binary.** This is the whole
  reason for `wire.go`; `TestEndToEndUsesBuildHandler` is the assertion that keeps it true
  as the test file grows.
- **`TestReaperRunsInBinary` can be made vacuous** by a test that calls `Reap` itself for
  speed. It must not — the ticker is the subject.
- **Repository-wide `gofmt -l`** in the fence will fail on any unformatted file anywhere,
  including ones this task did not touch. That is intended at the integration task, and it
  is the only fence here that is repository-wide.

## Stop Condition

Stop and report if the end-to-end test needs more than `httptest` and a temp directory —
needing a live external service would mean a component reached outside the process, which
contradicts the single-binary design.

## Out of Scope

- The worker binary — T9's.
- The dashboard — T10 mounts onto this router afterwards.
- Packaging, systemd units, containers (deferred: docs/adr/BACKLOG.md).

## Verification Log
