# Task ADR-0001-T1: Establish the Go module, pre-resolve every dependency, and define the domain kernel

**Depends-on:** none
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `core.Job`, `core.User`, `core.Token`, `core.Role`, `core.JobState`, `core.Result`, `core.Pipeline`, `core.Params`, `core.ValidParamKey()`, sentinel errors, `core.NewID()`
**Consumes:** none
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the exit code`, `the state-transition table`

## Goal

Create the module with every dependency already resolved, and define the shared kernel of
domain types every other package depends on — so no later task ever edits `go.mod`.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `go.mod` | add | module definition; **the only task permitted to write it** |
| `go.sum` | add | pinned dependency hashes |
| `internal/core/core.go` | add | package doc, `Role`, `JobState`, `NewID` |
| `internal/core/job.go` | add | `Job`, `Result`, state transition rules |
| `internal/core/user.go` | add | `User`, `Token` |
| `internal/core/errors.go` | add | sentinel errors callers match on |
| `internal/core/core_test.go` | add | the failing tests |
| `.gitignore` | edit | ignore the built binaries and the local dev database/blobs |

Nothing selects `internal/core` yet by design — it is a leaf the other nine tasks import.
Its reachability is established by T2 onward; rung 2 below records that honestly rather
than claiming a caller that does not exist.

## Ordered Steps

1. [S1] Confirm the failing tests for this task's behaviour exist and are red — write
   `internal/core/core_test.go` asserting the state-transition table and `NewID` ordering
   first (TDD red). [proof: acceptance]
2. [S2] `go mod init github.com/atvirokodosprendimai/ocr-router`, then `go get` **every**
   dependency the whole ADR needs in one pass: `github.com/go-chi/chi/v5`,
   `github.com/a-h/templ`, `github.com/pressly/goose/v3`, `modernc.org/sqlite`,
   `github.com/urfave/cli/v3`, `github.com/starfederation/datastar-go/datastar`,
   `github.com/google/uuid`. No other task may run `go get` or `go mod tidy`.
   [proof: acceptance]
3. [S3] Define `Role` (`admin`/`client`/`worker`) and `JobState`
   (`queued`/`processing`/`done`/`delivered`/`dead`/`expired`) as string types with a
   `Valid()` method, so an unknown value from the database is a caught error rather than a
   silent default. `expired` is a queued job that passed its deadline (ADR §Decision,
   queue order) and is terminal like `delivered` and `dead`.
4. [S4] Define `Job`, `User`, `Token`, `Result` as plain structs with no behaviour beyond
   `Job.CanTransitionTo(JobState) bool` and `Job.IsLastStage() bool`, which are the two
   rules the single writer enforces. `Job` carries the routing and pipeline fields the ADR
   added on 2026-09-15: `Label`, `Pipeline []string`, `Stage int`, `Params map[string]string`,
   `HasBlob bool`, `AccruedCredits int`, `QueuedAt`, `ExpiresAt`. `User` carries `Priority`
   and `JobTTLSecs`.
5. [S5] Define sentinel errors — `ErrNotFound`, `ErrNoCredits`, `ErrBufferFull`,
   `ErrUnauthorized`, `ErrForbidden`, `ErrConflict`, `ErrInvalidState` — so every later
   package matches on a value rather than on a string. [proof: acceptance]
6. [S6] `NewID()` returns a UUIDv7 string, so `ORDER BY id` is arrival order and no
   separate sequence column is needed.
7. [S7] `ValidParamKey(k string) bool` enforces `^[a-z][a-z0-9-]{0,31}$`. It lives in the
   kernel rather than in the worker because **both** ends must agree: the router rejects a
   bad key at upload and the worker refuses to build argv from one, and two copies of a
   regex are two chances to disagree about what is safe.

## Acceptance

```bash
set -o pipefail
go build ./... \
  && go vet ./internal/core/... \
  && go test ./internal/core/... -count=1 2>&1 | tee /tmp/adr1-t1.out \
  && ! grep -qE "no tests to run|no test files|^FAIL|^--- FAIL" /tmp/adr1-t1.out
```

At authoring time `go build ./...` fails outright — there is no `go.mod` — so this fence is
red before the work and cannot pass vacuously. The `no test files` guard is what stops a
package with zero tests reading as a pass, which is the state `go test` reports as `ok`.

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestJobCanTransitionTo` | `internal/core/core_test.go` | every legal edge of the state machine is allowed and every illegal one is refused | — | S4 |
| `TestJobCanTransitionToRejectsTerminal` | `internal/core/core_test.go` | `delivered` and `dead` are terminal — nothing transitions out of them | — | S4 |
| `TestRoleValid` | `internal/core/core_test.go` | an unknown role string is invalid, not silently accepted | — | S3 |
| `TestJobStateValid` | `internal/core/core_test.go` | an unknown state string is invalid | — | S3 |
| `TestNewIDIsOrdered` | `internal/core/core_test.go` | ids minted in sequence sort ascending as strings | — | S6 |
| `TestNewIDIsUnique` | `internal/core/core_test.go` | 10k ids in a tight loop collide zero times | — | S6 |
| `TestJobIsLastStage` | `internal/core/core_test.go` | a one-stage job is last at stage 0; a two-stage job is not, and is at stage 1 | — | S4 |
| `TestValidParamKey` | `internal/core/core_test.go` | `url` and `max-depth` pass; `URL`, `1st`, `has space`, `--flag`, `a;b`, empty and a 33-char key are each rejected | — | S7 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestJobCanTransitionTo` and the seven siblings above |
| 2 — something selects it | **Nothing yet, and that is correct for a leaf kernel.** The first caller is `internal/store` in T2; the mutation that proves it is reached is recorded on T2, not here. Claiming a caller now would name a check that cannot fail. |
| 3 — the caller can discover it | n/a: no declared interface — `internal/core` is an internal Go package, discovered by import |
| 4 — it is used | Every later task imports it; `go build ./...` in T8's fence fails if it does not exist |

## Mutation Log

## Invariants

- `internal/core` imports nothing outside the standard library and `github.com/google/uuid`.
  It is the kernel every shard depends on (`cqrs` §2c.1), so a dependency here is a
  dependency everywhere.
- `core` contains no SQL, no HTTP and no I/O.
- `delivered`, `dead` and `expired` are terminal states.

## Risks

- **`go get` resolving a version that does not build on Go 1.26.** Mitigated by S2 running
  the full resolution in one pass and `go build ./...` being inside the fence.
- **Over-modelling the kernel.** Mitigated by the invariant above: structs and one
  predicate, no services.

## Stop Condition

Stop and ask if a dependency cannot be resolved at a version that builds, or if the
operator wants a different module path than
`github.com/atvirokodosprendimai/ocr-router`.

## Out of Scope

- Any SQL, HTTP handler or business logic — those are T2 onward.
- `templ generate`; no `.templ` file exists until T10.

## Verification Log
