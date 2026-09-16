# Task ADR-0006-T4: Let a worker emit raw bytes and post them without passing through a string

**Depends-on:** T2
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `--raw` on `cmd/worker`, `runner.Run` raw arm, worker raw result wire shape (`POST /upload?job_id=<id>`, `Content-Type: application/octet-stream`)
**Consumes:** `Service.CheckWorkerMode(ctx, label, raw)` (T2), `?raw=` on `/sse` and `/claim` (T2)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `stdout bypassing parseUnits`, `the bytes never becoming a string`, `the flag reaching both declarations`, `the output cap failing rather than truncating`

## Goal

A worker started `--raw` skips the JSON contract, declares raw when it declares its label, and POSTs
its subprocess's stdout as an opaque body.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `cmd/worker/main.go` | edit | The `--raw` flag — the line that SELECTS every behaviour below; without it nothing here is reachable. Also prints `mode=` on the startup line, so a refused worker is diagnosable from what it already logs |
| `internal/runner/runner.go` | edit | `exec` extracted from `Run`, so `Run` and the new `RunRaw` share one fork/timeout/process-group/cap path and differ only in how stdout is read. Also makes `limitedWriter` REPORT overflow instead of truncating silently |
| `internal/agent/agent.go` | edit | `Config.Raw`, the mode declared on both the subscribe and the claim URL, and `reportRaw` posting an octet-stream body |
| `internal/runner/raw_test.go` | add | Runner-level tests |
| `internal/agent/raw_test.go` | add | Agent-level tests |
| `internal/agent/agent_test.go` | edit | `fakeRouter` records the declared mode and any octet-stream body, and branches on Content-Type as the real router's worker arm does |
| `cmd/worker/main_test.go` | edit | `buildRunner`'s new parameter |

## Ordered Steps

1. [S1] Write the failing tests: `TestRawRunnerReturnsBytesVerbatim` (a command emitting the 8-byte PNG magic number round-trips byte-for-byte) and `TestRawWorkerPostsOctetStream`. Confirm red. [proof: acceptance]
2. [S2] Add `--raw` to `cmd/worker`, default false, and thread it into the agent config.
3. [S3] Give `runner.Run` a raw arm that returns `stdout.Bytes()` untouched. Do NOT widen
   `parseUnits` — the units contract is not this ADR's to change, and a shared function with a mode
   argument is how two contracts become one confused one.
4. [S4] Declare `raw` on the agent's SSE subscribe and claim URLs, using T2's wire contract. A
   `--raw` worker on a units label must fail at startup with the router's 409, not loop.
5. [S5] Post a raw success as `POST /upload?job_id=<id>` with `Content-Type:
   application/octet-stream` and the bytes as the body, streamed rather than buffered.
6. [S6] Leave the FAILURE path on JSON for both modes — a failure is a reason string, which is text
   in every mode, and giving failures two encodings would double the router's parse surface for no
   gain. [proof: acceptance]
7. [S7] Enforce `--max-output` on the raw arm exactly as on the units arm. An unbounded raw body is
   a worker-side memory hole that the units contract currently closes by accident. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/runner ./internal/agent -run 'TestRawRunnerReturnsBytesVerbatim|TestRawRespectsMaxOutput|TestRawArmDoesNotParseUnits|TestUnitsArmUnchanged|TestRawWorkerPostsOctetStream|TestRawWorkerDeclaresMode|TestUnitsWorkerDeclaresUnits|TestRawFailureIsStillJSON|TestUnitsWorkerUnchanged' -count=1 -v 2>&1 | tee /tmp/acc-0006-T4.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T4.out \
  && go test ./internal/runner/... ./internal/agent/... ./cmd/worker/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestRawRunnerReturnsBytesVerbatim` | `internal/runner/raw_test.go` | A command printing `\x89PNG\r\n\x1a\n` yields those exact 8 bytes — the defect this ADR exists for, asserted at the first hop | — | S3 |
| `TestRawArmDoesNotParseUnits` | `internal/runner/raw_test.go` | The raw arm accepts stdout that is not JSON at all, so the two output contracts stay apart | — | S3 |
| `TestUnitsArmUnchanged` | `internal/runner/raw_test.go` | A units worker still refuses non-JSON stdout and still QUOTES what was produced — the message that made the original defect diagnosable | — | S3 |
| `TestRawRespectsMaxOutput` | `internal/runner/raw_test.go` | A raw command over `--max-output` FAILS the job rather than delivering a silently shortened payload | — | S7 |
| `TestRawWorkerPostsOctetStream` | `internal/agent/raw_test.go` | The result POST carries `application/octet-stream`, `?job_id=`, a body byte-equal to stdout, and NO JSON report beside it | — | S5 |
| `TestRawWorkerDeclaresMode` | `internal/agent/raw_test.go` | `--raw` reaches BOTH the subscribe and the claim URL | — | S2, S4 |
| `TestUnitsWorkerDeclaresUnits` | `internal/agent/raw_test.go` | A units worker declares `raw=0` on both — without this, the test above is satisfied by hardcoding `raw=1` | — | S2, S4 |
| `TestRawFailureIsStillJSON` | `internal/agent/raw_test.go` | A failing raw job posts the JSON failure shape and no octet-stream body | — | S6 |
| `TestUnitsWorkerUnchanged` | `internal/agent/raw_test.go` | A worker without `--raw` posts exactly what it posted before this ADR | — | S3 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestRawRunnerReturnsBytesVerbatim` |
| 2 — something selects it | `cmd/worker`'s `--raw` flag is the only selector; `TestRawWorkerDeclaresMode` goes red if the flag stops reaching the agent config, which is the mutation to record |
| 3 — the caller can discover it | `--raw`'s `Usage` string and the worker README. An operator reads `--help`; if the flag is undocumented the mode is unreachable in practice even though it works |
| 4 — it is used | Nothing measures this yet — `ocrr_raw_jobs` is T6's |

## Mutation Log

- 2026-09-16 · e39597e* · mutant killed · exit 1 · `internal/runner/runner.go` · the raw arm applies the units JSON contract, so a binary payload is rejected outright and a JSON-shaped one is silently re-encoded · acceptance-sha256:c68f76f6b51685b5a5daf3280a4c000f6daa22cd58589d6b45038f081f1c29c5 · covers:stdout bypassing parseUnits
- 2026-09-16 · e39597e* · mutant killed · exit 1 · `internal/agent/agent.go` · the raw body is announced as JSON, so the router routes it into the units decoder and the payload is corrupted or rejected · acceptance-sha256:c68f76f6b51685b5a5daf3280a4c000f6daa22cd58589d6b45038f081f1c29c5 · covers:the bytes never becoming a string
- 2026-09-16 · e39597e* · mutant killed · exit 1 · `internal/agent/agent.go` · the claim path stops declaring the mode, so a raw worker is registered as units on the path that takes the lease · acceptance-sha256:c68f76f6b51685b5a5daf3280a4c000f6daa22cd58589d6b45038f081f1c29c5 · covers:the flag reaching both declarations
- 2026-09-16 · e39597e* · mutant killed · exit 1 · `internal/runner/runner.go` · an over-cap payload is silently TRUNCATED and delivered as though whole — indistinguishable from a short result, which for a raw service means a corrupted file stored and billed · acceptance-sha256:c68f76f6b51685b5a5daf3280a4c000f6daa22cd58589d6b45038f081f1c29c5 · covers:the output cap failing rather than truncating

## Invariants

- A raw worker's stdout is never converted to `string` anywhere on the path, and never passed to
  `encoding/json`. This is the invariant the ADR exists to establish; a `string(b)` round trip is
  safe in Go but invites the next author to marshal it.
- `parseUnits` and the units contract are byte-for-byte unchanged.
- `--max-output` bounds both arms.
- A failure is JSON in both modes.

## Risks

- `string(stdout)` is a lossless conversion in Go, so a reviewer may "simplify" the raw arm back
  through a string without any test going red. `TestRawRunnerReturnsBytesVerbatim` only catches the
  JSON hop, not the string hop — the invariant above is the guard, and it is prose. Named rather
  than claimed as covered.

## Stop Condition

Stop if the agent cannot stream the body without first materialising it (the runner already buffers
stdout to enforce `--max-output`, so the body is in memory at the worker by design). That is a
worker-side bound the ADR accepts; if it turns out the router needs the worker to stream, the
`--max-output` design is the thing to revisit, not this task.

## Out of Scope

- What the router does with the bytes — T5.
- Pricing — T6.

## Verification Log
- 2026-09-16 · e39597e* · exit 0 · `set -o pipefail …` · acceptance-sha256:c68f76f6b51685b5a5daf3280a4c000f6daa22cd58589d6b45038f081f1c29c5 · ms:11762
- 2026-09-16 · e39597e* · exit 0 · `set -o pipefail …` · acceptance-sha256:c68f76f6b51685b5a5daf3280a4c000f6daa22cd58589d6b45038f081f1c29c5 · ms:10972
- 2026-09-16 · e39597e* · exit 0 · `set -o pipefail …` · acceptance-sha256:c68f76f6b51685b5a5daf3280a4c000f6daa22cd58589d6b45038f081f1c29c5 · ms:11446
- 2026-09-16 · e39597e* · exit 0 · `set -o pipefail …` · acceptance-sha256:c68f76f6b51685b5a5daf3280a4c000f6daa22cd58589d6b45038f081f1c29c5 · ms:10779
