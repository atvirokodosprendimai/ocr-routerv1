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
| `cmd/router/wire.go` | add | `buildApp(Config) (*App, error)` — the composition root, constructed once and used by BOTH `main` and the tests |
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
3. [S3] `buildApp` opens the store, runs migrations, constructs `blob`, `results`, `bus`,
   `identity` and `router.Service`, calls `RecoverOnBoot`, mounts `httpapi.New(deps)` and
   returns the handler plus its cleanup. **One place** builds the graph, and the tests use
   that place rather than assembling their own.
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
| `TestEndToEndUploadToDelivery` | `cmd/router/main_test.go` | against a server built by `buildApp`, over real HTTP: the client uploads, the worker is woken, claims, downloads the blob, posts units; the client's stream receives `ready`; the client GETs the result, is charged 3, the job is `delivered` and the blob is gone | — | S3 |
| `TestReaperRunsInBinary` | `cmd/router/main_test.go` | with a 100ms lease and a silent worker, the job returns to `queued` **without the test ever calling `Reap`** — red when the ticker is deleted, which every `internal/router` test survives because they all drive `Reap` directly | — | S4 |
| `TestRecoverOnBootRequeuesAcrossRestart` | `cmd/router/main_test.go` | upload, claim, tear the app down, rebuild on the same files → the job is `queued` and no debit was written | — | S3, S7 |
| `TestBootstrapCreatesAdminOnceAndTheTokenWorks` | `cmd/router/main_test.go` | the first bootstrap creates an admin whose printed token AUTHENTICATES against the running server, and a second bootstrap is refused | — | S6 |
| `TestSSESurvivesTheServersOwnConfig` | `cmd/router/main_test.go` | a stream on the server `main` configures stays open and receives a ping | — | S5 |
| `TestFlagDefaultsBuildAServer` | `cmd/router/main_test.go` | a minimal config with only paths builds a working handler | — | S2 |
| `TestCLIExposesEveryFlag` | `cmd/router/main_test.go` | every field in `Config` has a command-line flag — rung 3, because a setting an operator cannot reach is not configuration | — | S2 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the eight tests above |
| 2 — something selects it | `main` calls `buildHandler`; `TestReaperRunsInBinary` and `TestEndToEndUsesBuildHandler` are the checks that go red if the wiring is removed. This is the rung the whole task exists to cover for T2–T7. |
| 3 — the caller can discover it | `--help` lists every flag; the README documents bootstrap and the three client endpoints |
| 4 — it is used | it is the product; the dashboard (T10) and the worker (T9) both talk to this binary |

## Mutation Log

- 2026-09-15 · 621bae6* · mutant killed · exit 1 · `cmd/router/wire.go` · The ticker is the one thing no test in internal/router watches: every one of them drives Reap directly, so all of them pass with it deleted. · acceptance-sha256:f01dd49f5306c6fccda4765c18b578bb1a8c43334caf26a6e2d9e6c5f21de98b · covers:the reaper ticker
- 2026-09-15 · 621bae6* · mutant killed · exit 1 · `cmd/router/wire.go` · Without boot recovery every job in flight at shutdown stays processing forever: its lease belongs to a process that no longer exists and no worker will ever report on it. · acceptance-sha256:f01dd49f5306c6fccda4765c18b578bb1a8c43334caf26a6e2d9e6c5f21de98b · covers:the composition root
- 2026-09-15 · 621bae6* · mutant killed · exit 1 · `cmd/router/wire.go` · A handler that is constructed and not mounted is finished, tested and called by nothing — the defect this task exists to make visible. · acceptance-sha256:f01dd49f5306c6fccda4765c18b578bb1a8c43334caf26a6e2d9e6c5f21de98b

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
- 2026-09-15 · 621bae6* · exit 0 · `set -o pipefail …` · acceptance-sha256:f01dd49f5306c6fccda4765c18b578bb1a8c43334caf26a6e2d9e6c5f21de98b · ms:8238
- 2026-09-15 · 621bae6* · exit 0 · `set -o pipefail …` · acceptance-sha256:f01dd49f5306c6fccda4765c18b578bb1a8c43334caf26a6e2d9e6c5f21de98b · ms:5119
- 2026-09-15 · 621bae6* · exit 0 · `set -o pipefail …` · acceptance-sha256:f01dd49f5306c6fccda4765c18b578bb1a8c43334caf26a6e2d9e6c5f21de98b · ms:5394
- 2026-09-15 · 621bae6* · exit 0 · `set -o pipefail …` · acceptance-sha256:f01dd49f5306c6fccda4765c18b578bb1a8c43334caf26a6e2d9e6c5f21de98b · ms:5254
- 2026-09-15 · 621bae6* · exit 0 · `set -o pipefail …` · acceptance-sha256:f01dd49f5306c6fccda4765c18b578bb1a8c43334caf26a6e2d9e6c5f21de98b · ms:10594
