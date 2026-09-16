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
| `internal/httpapi/raw_test.go` | edit | The failing tests below |
| `internal/router/service_test.go` | edit | Admission-level tests |

## Ordered Steps

1. [S1] Write the failing tests: `TestClientRawOnUnitsLabelIsRefused`, `TestClientUnitsOnRawLabelIsRefused`, `TestRawIsNotPassedToArgv`. Confirm red. [proof: acceptance]
2. [S2] Add `"raw"` to `reservedParams` in `internal/httpapi/upload.go:15`. This is the security half
   of the step, not bookkeeping: without it `raw` reaches the subprocess as a `-raw` flag.
3. [S3] Carry the requested mode on `router.UploadInput` as a tri-state (`unset`, `units`, `raw`), so
   "the client said nothing" is distinguishable from "the client said units".
4. [S4] In `Service.Upload`, read `ServiceMode(pipeline[0])` and refuse on disagreement with
   `ErrModeMismatch`. `unset` means units, matching T2's rule for an undeclared worker.
5. [S5] Stamp `job.Raw` from the resolved mode before `CreateJob`, and never after.
6. [S6] Confirm the refusal happens before any body is read — the streaming property commit `b84f45a`
   established, which this task must not regress. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
go test ./internal/httpapi/... -run 'TestClientRawOnUnitsLabelIsRefused|TestClientUnitsOnRawLabelIsRefused|TestRawIsNotPassedToArgv' -count=1 -v 2>&1 | tee /tmp/acc-0006-T3.out \
  && ! grep -qE "no tests to run|^FAIL|^--- FAIL" /tmp/acc-0006-T3.out \
  && go test ./internal/httpapi/... ./internal/router/... -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestClientRawOnUnitsLabelIsRefused` | `internal/httpapi/raw_test.go` | `?raw=1` against a units label is 409, and no job row is created | — | S3, S4 |
| `TestClientUnitsOnRawLabelIsRefused` | `internal/httpapi/raw_test.go` | An upload with no `raw` param against a raw label is 409 — a client that has not been upgraded cannot accidentally receive bytes it will treat as text | — | S3, S4 |
| `TestRawIsNotPassedToArgv` | `internal/httpapi/raw_test.go` | `?raw=1` does not appear in the job's params, so it never becomes a subprocess flag | — | S2 |
| `TestJobStampsRawAtAdmission` | `internal/router/service_test.go` | A job admitted to a raw label reads back `Raw: true`; changing `service_rates` afterwards does not change the stored job | — | S5 |
| `TestRawRefusalReadsNoBody` | `internal/httpapi/raw_test.go` | A refused raw upload consumes no more of the body than the multipart headers — the `b84f45a` streaming property, re-asserted on the new refusal path | — | S6 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestClientRawOnUnitsLabelIsRefused` |
| 2 — something selects it | `Service.Upload` is the only admission path; deleting the `ServiceMode` comparison makes both refusal tests green-to-red, which the mutation entry must prove |
| 3 — the caller can discover it | `raw` in `reservedParams` is the declared interface — `TestRawIsNotPassedToArgv` fails if it is removed, which is the same line that stops it reaching argv |
| 4 — it is used | Nothing measures this yet — `ocrr_raw_jobs` is T6's |

## Mutation Log

## Invariants

- A client can never *set* a label's mode, only agree with it. Same boundary as T2 from the other side.
- `raw` never reaches subprocess argv. It is a router-reserved word exactly as `label` and
  `pipeline` are.
- Admission still refuses before reading the request body.
- Only `pipeline[0]` is checked, consistent with ADR-0001's rule that later stages may have no live
  worker yet. A later raw stage is validated when the job advances into it (T6).

## Risks

- The tri-state is easy to collapse into a bool during implementation, which silently makes
  "unset" mean "whatever the label says" and reopens the escalation from the client side.
  `TestClientUnitsOnRawLabelIsRefused` is the test that catches exactly that collapse.

## Stop Condition

Stop if the owner overrules the three-party agreement in the ADR's boxed note — this task's whole
subject is the client third of it, and a two-party design deletes the task rather than shrinking it.

## Out of Scope

- The worker half of the match — T2.
- Anything about what a raw result looks like — T4 and T5.

## Verification Log
