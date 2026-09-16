# Task ADR-0006-T3: Match the client's requested mode against the admin record and stamp it on the job

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** M (multi-file)
**Owner:** unassigned
**Produces:** `jobs.raw` stamped at admission, `?raw=1` as a reserved upload param
**Consumes:** `Repo.ServiceMode(ctx, label)` (T1), `core.Job.Raw` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the comparison against the admin record`, `raw being reserved rather than passed to argv`, `the stamp being taken at admission`

## Goal

`POST /upload?raw=1` is admitted only when the first stage's label is a raw service, and the job
carries that mode immutably from the moment it is created.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/httpapi/upload.go` | edit | `raw` joins `reservedParams`; the requested mode rides `router.UploadInput` |
| `internal/router/service.go` | edit | `Upload` compares the request against `Repo.ServiceMode` for `pipeline[0]` and stamps `job.Raw` |
| `internal/httpapi/rawupload_test.go` | add | The client-side admission tests |
| `internal/httpapi/helpers_test.go` | add | `newCountingServer` / `decodeJSON`, extracted so the streaming assertion is written once and used by both the `b84f45a` test and this task's |
| `internal/router/rawmode_test.go` | add | The service-layer admission tests — the handler is not Upload's only caller |

## Ordered Steps

1. [S1] Write the failing tests: `TestClientRawOnUnitsLabelIsRefused`, `TestClientUnitsOnRawLabelIsRefused`, `TestRawIsNotPassedToArgv`. Confirm red. [proof: acceptance]
2. [S2] Add `"raw"` to `reservedParams` in `internal/httpapi/upload.go:15`. This is the security half
   of the step, not bookkeeping: without it `raw` reaches the subprocess as a `-raw` flag.
3. [S3] ⚠ **AMENDED DURING EXECUTION — a plain bool, not a tri-state.** The task called for
   `unset` / `units` / `raw` so that "the client said nothing" stayed distinguishable from "the
   client said units". Enumerating the four cases shows the distinction is unobservable: on a units
   service both are admitted, on a raw service both are refused. A state nothing can act on is a
   state that can only be got wrong, so `UploadInput.Raw` is a bool and `rawParam` — the same parser
   the worker side uses — maps absent to units.
4. [S4] In `Service.Upload`, read `ServiceMode(pipeline[0])` and refuse on disagreement with
   `ErrModeMismatch`. `unset` means units, matching T2's rule for an undeclared worker.
5. [S5] Stamp `job.Raw` from the resolved mode before `CreateJob`, and never after.
6. [S6] Confirm the refusal happens before any body is read — the streaming property commit `b84f45a`
   established, which this task must not regress. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/httpapi/... ./internal/router/... -run 'TestClientRawOnUnitsLabelIsRefused|TestClientUnitsOnRawLabelIsRefused|TestRawIsNotPassedToArgv|TestRawRefusalReadsNoBody|TestJobStampsRawAtAdmission|TestUploadRefusesModeMismatch' -count=1 -v 2>&1 | tee /tmp/acc-0006-T3.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T3.out \
  && go test ./internal/httpapi/... ./internal/router/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestClientRawOnUnitsLabelIsRefused` | `internal/httpapi/rawupload_test.go` | `?raw=1` against a units label is 409, and no job row is created | — | S3, S4 |
| `TestClientUnitsOnRawLabelIsRefused` | `internal/httpapi/rawupload_test.go` | Both an absent `raw` and an explicit `raw=0` against a raw label are 409 — the two spellings the amended S3 deliberately treats as one request | — | S3, S4 |
| `TestRawIsNotPassedToArgv` | `internal/httpapi/rawupload_test.go` | `?raw=1` does not appear in the job's params, so it never becomes a subprocess flag, while an ordinary param survives | — | S2 |
| `TestRawRefusalReadsNoBody` | `internal/httpapi/rawupload_test.go` | A refused mode mismatch consumes no more of a 1 MiB body than the multipart headers — the `b84f45a` streaming property re-asserted on the new refusal path | — | S6 |
| `TestJobStampsRawAtAdmission` | `internal/router/rawmode_test.go` | A job admitted to a raw service stays raw after the SERVICE is edited back to units — the stamp is immutable | — | S5 |
| `TestUploadRefusesModeMismatch` | `internal/router/rawmode_test.go` | `Service.Upload` itself refuses with `ErrModeMismatch` and writes no row — the handler is not the only caller | — | S4 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestClientRawOnUnitsLabelIsRefused` |
| 2 — something selects it | `Service.Upload` is the only admission path; deleting the `ServiceMode` comparison makes both refusal tests green-to-red, which the mutation entry must prove |
| 3 — the caller can discover it | `raw` in `reservedParams` is the declared interface — `TestRawIsNotPassedToArgv` fails if it is removed, which is the same line that stops it reaching argv |
| 4 — it is used | Nothing measures this yet — `ocrr_raw_jobs` is T6's |

## Mutation Log

- 2026-09-16 · 0a14eee* · mutant killed · exit 1 · `internal/router/service.go` · a client may request any mode for any service, so a units client is handed bytes and a raw client is billed per unit · acceptance-sha256:7b164f39dd5916388821677719c363bf425e8be1d3c3e3162ef78cc8cd9f0c00 · covers:the comparison against the admin record
- 2026-09-16 · 0a14eee* · mutant killed · exit 1 · `internal/httpapi/upload.go` · raw stops being reserved and reaches the job params, so it arrives on the worker subprocess as a -raw flag · acceptance-sha256:7b164f39dd5916388821677719c363bf425e8be1d3c3e3162ef78cc8cd9f0c00 · covers:raw being reserved rather than passed to argv
- 2026-09-16 · 0a14eee* · mutant killed · exit 1 · `internal/router/service.go` · the job is never stamped raw, so every downstream branch reads it as a units job · acceptance-sha256:7b164f39dd5916388821677719c363bf425e8be1d3c3e3162ef78cc8cd9f0c00 · covers:the stamp being taken at admission

## Invariants

- A client can never *set* a label's mode, only agree with it. Same boundary as T2 from the other side.
- `raw` never reaches subprocess argv. It is a router-reserved word exactly as `label` and
  `pipeline` are.
- Admission still refuses before reading the request body.
- Only `pipeline[0]` is checked, consistent with ADR-0001's rule that later stages may have no live
  worker yet. A later raw stage is validated when the job advances into it (T6).

## Risks

- The tri-state the task originally specified was dropped (see S3). The risk it named — "unset
  silently means whatever the label says" — is guarded by `TestClientUnitsOnRawLabelIsRefused`,
  which exercises the absent spelling explicitly rather than only `raw=0`.

## Stop Condition

Stop if the owner overrules the three-party agreement in the ADR's boxed note — this task's whole
subject is the client third of it, and a two-party design deletes the task rather than shrinking it.

## Out of Scope

- The worker half of the match — T2.
- Anything about what a raw result looks like — T4 and T5.

## Verification Log
- 2026-09-16 · 0a14eee* · exit 1 · `set -o pipefail …` · acceptance-sha256:d9c5d5bb309aec1d5fe6ccfdd831f91e092d49f718f2ff91c9b5919f79d0f2d7 · ms:2172
  ```
  --- last 10 line(s) of stdout (of 14 after folding 14 raw)
      rawupload_test.go:45: /upload?label=convert = 201, want 409 — a client that did not ask for raw must not be handed bytes
      rawupload_test.go:45: /upload?label=convert&raw=0 = 201, want 409 — a client that did not ask for raw must not be handed bytes
  --- FAIL: TestClientUnitsOnRawLabelIsRefused (0.02s)
  === RUN   TestRawIsNotPassedToArgv
      rawupload_test.go:75: `raw` reached the job's params — it would become a subprocess flag
      rawupload_test.go:81: the job was not stamped raw despite being admitted to a raw service
  --- FAIL: TestRawIsNotPassedToArgv (0.01s)
  FAIL
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/httpapi	0.559s
  FAIL
  ```
- 2026-09-16 · 0a14eee* · exit 0 · `set -o pipefail …` · acceptance-sha256:7b164f39dd5916388821677719c363bf425e8be1d3c3e3162ef78cc8cd9f0c00 · ms:6282
- 2026-09-16 · 0a14eee* · exit 0 · `set -o pipefail …` · acceptance-sha256:7b164f39dd5916388821677719c363bf425e8be1d3c3e3162ef78cc8cd9f0c00 · ms:5335
- 2026-09-16 · 0a14eee* · exit 0 · `set -o pipefail …` · acceptance-sha256:7b164f39dd5916388821677719c363bf425e8be1d3c3e3162ef78cc8cd9f0c00 · ms:5047
