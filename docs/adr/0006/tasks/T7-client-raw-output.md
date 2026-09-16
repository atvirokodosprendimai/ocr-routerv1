# Task ADR-0006-T7: Let the client request raw and write the bytes it gets back unchanged

**Depends-on:** T5
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `--raw` on `cmd/client`, byte-exact output writing
**Consumes:** raw result blob + `GET /files/{id}` octet-stream response (T5)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `branching on the response Content-Type`, `the bytes never becoming a string`, `the flag reaching the upload query`

## Goal

`client --raw` submits to a raw service and writes its worker's bytes to `-o` unchanged, including
to stdout.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `cmd/client/main.go` | edit | The `--raw` flag — the line that SELECTS everything below |
| `internal/client/client.go` | edit | `?raw=1` on upload; `collect` branches on the response `Content-Type` |
| `cmd/client/output.go` | edit | Writes bytes for a raw result instead of newline-joined units |
| `internal/client/client_test.go` | edit | The failing tests below |
| `cmd/client/output_test.go` | edit | Output-writing tests |

## Ordered Steps

1. [S1] Write the failing tests: `TestRawClientWritesBytesVerbatim` and `TestRawFlagReachesUploadQuery`. Confirm red. [proof: acceptance]
2. [S2] Add `--raw` to `cmd/client` and carry it into `client.SubmitInput`.
3. [S3] Send `?raw=1` on upload. A mode mismatch comes back 409 and must surface as a readable
   error naming the label, not a generic conflict — the client is where an operator learns the
   service is not raw.
4. [S4] In `collect`, branch on the response `Content-Type`: `application/octet-stream` reads the
   body as bytes, anything else decodes the existing units JSON. Branch on what the server SAID,
   not on what the client asked for — those disagree exactly when something is wrong, and that is
   the case worth reporting rather than silently misreading.
5. [S5] Write a raw result with `io.Copy` to the output file or stdout, never through a string, and
   never with the newline joining ADR-0005 specified for units.
6. [S6] Keep `-o -` working: a raw result to stdout must not have progress output interleaved into
   it, which ADR-0005 already guarantees by putting progress on stderr. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/client/... ./cmd/client/... -run 'TestRawClientWritesBytesVerbatim|TestRawFlagReachesUploadQuery|TestRawToStdoutIsUncontaminated' -count=1 -v 2>&1 | tee /tmp/acc-0006-T7.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T7.out \
  && go test ./internal/client/... ./cmd/client/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestRawClientWritesBytesVerbatim` | `cmd/client/output_test.go` | An octet-stream response containing `\x89PNG\r\n\x1a\n` is written to the output file byte-for-byte, with no trailing newline added | — | S4, S5 |
| `TestRawFlagReachesUploadQuery` | `internal/client/client_test.go` | `--raw` produces `?raw=1`; without it the query carries no `raw` | — | S2, S3 |
| `TestRawToStdoutIsUncontaminated` | `cmd/client/output_test.go` | `-o -` with a raw result writes only the bytes to stdout; progress is on stderr | — | S6 |
| `TestModeMismatchErrorNamesTheLabel` | `internal/client/client_test.go` | A 409 from the router surfaces as an error naming the label and the mode, not a bare "conflict" | — | S3 |
| `TestUnitsClientUnchanged` | `cmd/client/output_test.go` | A units result is still newline-joined exactly as ADR-0005 specified | — | S4, S5 |
| `TestCollectBranchesOnResponseNotRequest` | `internal/client/client_test.go` | A client that asked for raw but received a units JSON response reports the disagreement rather than misreading it | — | S4 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestRawClientWritesBytesVerbatim` |
| 2 — something selects it | `cmd/client`'s `--raw` is the only selector; `TestRawFlagReachesUploadQuery` goes red if the flag stops reaching `SubmitInput` — the mutation to record |
| 3 — the caller can discover it | `--raw`'s `Usage` string and the client README. ADR-0005 makes the client composable (`-o -`), so the flag must be discoverable from `--help` or the mode is unreachable in practice |
| 4 — it is used | Nothing measures this yet; the client emits no telemetry by design |

## Mutation Log

## Invariants

- A raw result is never converted to `string` and never has a newline appended. ADR-0005's
  newline-joined output is a units contract and stays one.
- `collect` branches on the RESPONSE content type, never on the request flag.
- Progress stays on stderr in both modes, so `-o -` stays composable.

## Risks

- Appending a trailing newline is the single most likely accidental corruption here, because every
  other output path in this CLI ends with one. `TestRawClientWritesBytesVerbatim` asserts exact
  length rather than a prefix match, which is what catches it.

## Stop Condition

Stop if ADR-0005's output contract turns out to specify a trailing newline for ALL results rather
than for units results; that would be a conflict between two accepted records and is the owner's to
resolve, not this task's to paper over.

## Out of Scope

- Anything server-side — T5 owns the response.
- Progress reporting changes of any kind (permanent: boundary: ADR-0005 owns the client's progress contract and this ADR does not touch it).

## Verification Log
