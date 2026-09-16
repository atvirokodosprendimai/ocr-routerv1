# Task ADR-0006-T7: Let the client request raw and write the bytes it gets back unchanged

**Depends-on:** T5
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `--raw` on `cmd/client`, byte-exact output writing
**Consumes:** raw result blob + `GET /files/{id}` octet-stream response (T5)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `branching on the response Content-Type`, `the bytes reaching the file unchanged`, `the flag reaching the upload query`

## Goal

`client --raw` submits to a raw service and writes its worker's bytes to `-o` unchanged, including
to stdout.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `cmd/client/main.go` | edit | The `--raw` flag — the line that SELECTS everything below |
| `internal/client/client.go` | edit | `Input.Raw`, `Result.Raw`, `?raw=` on upload, and `collect` branching on the response content type |
| `cmd/client/output.go` | edit | `render` returns `[]byte` and passes a raw result through unchanged; `--json` refuses one |
| `internal/client/raw_test.go` | add | Wire-level tests through `Submit` |
| `cmd/client/rawoutput_test.go` | add | Output-writing tests |

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
go test ./internal/client ./cmd/client -run 'TestRawFlagReachesUploadQuery|TestCollectReadsOctetStreamAsBytes|TestCollectBranchesOnResponseNotRequest|TestRawClientWritesBytesVerbatim|TestRawToStdoutIsUncontaminated|TestUnitsClientUnchanged|TestRawJSONModeIsRefused' -count=1 -v 2>&1 | tee /tmp/acc-0006-T7.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T7.out \
  && go test ./internal/client/... ./cmd/client/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestRawClientWritesBytesVerbatim` | `cmd/client/rawoutput_test.go` | `\x89PNG\r\n\x1a\n` is written to the output file byte-for-byte, asserted on EXACT LENGTH — a trailing newline is the likeliest corruption here and a prefix match would miss it | — | S5 |
| `TestRawToStdoutIsUncontaminated` | `cmd/client/rawoutput_test.go` | `-o -` writes only the bytes, so `client … -o - \| …` stays pipeable | — | S6 |
| `TestUnitsClientUnchanged` | `cmd/client/rawoutput_test.go` | Units are still newline-joined with their trailing newline — ADR-0005 owns that shape | — | S4, S5 |
| `TestRawJSONModeIsRefused` | `cmd/client/rawoutput_test.go` | `--json` REFUSES a raw result rather than encoding it: a JSON string field cannot carry bytes, and encoding them there would reinstate the U+FFFD corruption at the last hop | — | S5 |
| `TestRawFlagReachesUploadQuery` | `internal/client/raw_test.go` | `Raw: true` produces `?raw=1` and `Raw: false` produces `?raw=0` — the units case included, or the raw case is satisfied by hardcoding | — | S2, S3 |
| `TestCollectReadsOctetStreamAsBytes` | `internal/client/raw_test.go` | An octet-stream response becomes `Result.Raw` and no units | — | S4 |
| `TestCollectBranchesOnResponseNotRequest` | `internal/client/raw_test.go` | A client that asked for raw and received units reads them as units — branching on the response, not the request | — | S4 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestRawClientWritesBytesVerbatim` |
| 2 — something selects it | `cmd/client`'s `--raw` is the only selector; `TestRawFlagReachesUploadQuery` goes red if the flag stops reaching `SubmitInput` — the mutation to record |
| 3 — the caller can discover it | `--raw`'s `Usage` string and the client README. ADR-0005 makes the client composable (`-o -`), so the flag must be discoverable from `--help` or the mode is unreachable in practice |
| 4 — it is used | Nothing measures this yet; the client emits no telemetry by design |

## Mutation Log

- 2026-09-16 · 2fae639* · mutant inconclusive · exit 1 · `internal/client/client.go` · the client branches on its OWN REQUEST, so a raw request answered with a units envelope hands the caller a file full of JSON and calls it the result · acceptance-sha256:484f4a4e7f72aeeb6de86ce0be52c9b3453f674087b0053596dee5f9bcc2df83 · covers:branching on the response Content-Type
  ```
  the fence failed on a build/parse error, not an assertion
  ```
- 2026-09-16 · 2fae639* · mutant killed · exit 1 · `internal/client/client.go` · the response type is never consulted, so a units envelope is read as raw bytes and the caller gets a file full of JSON called a result · acceptance-sha256:484f4a4e7f72aeeb6de86ce0be52c9b3453f674087b0053596dee5f9bcc2df83 · covers:branching on the response Content-Type
- 2026-09-16 · 2fae639* · mutant killed · exit 1 · `cmd/client/output.go` · a trailing newline is appended to a raw payload — the likeliest accidental corruption here, because every other output path in this CLI ends with one · acceptance-sha256:484f4a4e7f72aeeb6de86ce0be52c9b3453f674087b0053596dee5f9bcc2df83 · covers:the bytes reaching the file unchanged
- 2026-09-16 · 2fae639* · mutant killed · exit 1 · `internal/client/client.go` · the client always declares units, so --raw silently uploads to a raw service as a units client and is refused with no visible cause · acceptance-sha256:484f4a4e7f72aeeb6de86ce0be52c9b3453f674087b0053596dee5f9bcc2df83 · covers:the flag reaching the upload query

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
- 2026-09-16 · 2fae639* · exit 0 · `set -o pipefail …` · acceptance-sha256:484f4a4e7f72aeeb6de86ce0be52c9b3453f674087b0053596dee5f9bcc2df83 · ms:3000
- 2026-09-16 · 2fae639* · exit 0 · `set -o pipefail …` · acceptance-sha256:484f4a4e7f72aeeb6de86ce0be52c9b3453f674087b0053596dee5f9bcc2df83 · ms:3106
- 2026-09-16 · 2fae639* · exit 0 · `set -o pipefail …` · acceptance-sha256:484f4a4e7f72aeeb6de86ce0be52c9b3453f674087b0053596dee5f9bcc2df83 · ms:2980
- 2026-09-16 · 2fae639* · exit 0 · `set -o pipefail …` · acceptance-sha256:484f4a4e7f72aeeb6de86ce0be52c9b3453f674087b0053596dee5f9bcc2df83 · ms:3157
