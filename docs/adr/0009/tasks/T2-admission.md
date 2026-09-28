# Task ADR-0009-T2: Admit an unmetered customer whose balance is not positive

**Depends-on:** T1
**Covers:** none — no spec
**Estimated scope:** S (single file)
**Owner:** unassigned
**Produces:** none
**Consumes:** `core.User.Unmetered` (T1)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** `the unmetered exemption`, `the Active check that is NOT exempted`

## Goal

`Upload` stops returning `core.ErrNoCredits` to a customer marked unmetered, and keeps returning it to
everyone else.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `internal/router/service.go` | edit | the admission condition at `Upload`; this is the only place `ErrNoCredits` is produced |
| `internal/router/unmetered_test.go` | add | the exemption, its boundary, and the two things it must NOT exempt |

## Ordered Steps

1. [S1] Write the failing tests first: an unmetered customer with a zero balance uploads successfully;
   a metered customer with a zero balance still gets `core.ErrNoCredits`. The first is red.
2. [S2] Change the gate to `if !u.Unmetered && u.Credits <= 0`, and extend the comment above it — it
   currently explains why admission is gated on ANY credit rather than the eventual cost, and now has
   to say that an unmetered customer has no balance to gate on at all.
3. [S3] Add the two negative tests that keep the exemption narrow: an unmetered customer that is
   INACTIVE is still refused (`core.ErrForbidden`), and an unmetered customer at its buffer limit is
   still refused (`core.ErrBufferFull`). Neither is about credits, and an exemption written one line
   too high would swallow both.

## Acceptance

```bash
set -o pipefail
go test ./internal/router/ -run 'Unmetered' -count=1 2>&1 | tee /tmp/adr9t2a.out && \
  ! grep -qE "no tests to run|^FAIL|^--- FAIL|warning: no tests" /tmp/adr9t2a.out && \
  go test ./internal/router/ -count=1
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| `TestAnUnmeteredCustomerUploadsWithNoCredits` | `internal/router/unmetered_test.go` | balance 0 and `Unmetered` true is admitted | — | S1, S2 |
| `TestUnmeteredDoesNotExemptAMeteredCustomer` | `internal/router/unmetered_test.go` | the exemption did not become unconditional — red if the `!u.Unmetered` term is dropped | — | S1, S2 |
| `TestAnUnmeteredCustomerIsStillRefusedWhenInactive` | `internal/router/unmetered_test.go` | `ErrForbidden` still wins; red if the exemption is placed above the `Active` check | — | S3 |
| `TestAnUnmeteredCustomerIsStillBoundedByItsBufferLimit` | `internal/router/unmetered_test.go` | `ErrBufferFull` still applies — unmetered is about price, not about how much work may be in flight | — | S3 |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | `TestAnUnmeteredCustomerUploadsWithNoCredits` |
| 2 — something selects it | the condition in `Upload`, which every upload runs; the mutation for this task deletes the `!u.Unmetered` term and `TestUnmeteredDoesNotExemptAMeteredCustomer` must go red |
| 3 — the caller can discover it | n/a: no declared interface — a client sees a `201` instead of a `402` and needs to know nothing |
| 4 — it is used | `ocrr_jobs_total{state="queued"}` already counts admitted work; nothing distinguishes an unmetered upload, and nothing needs to |

## Mutation Log

- 2026-09-28 · ebde216* · mutant killed · exit 1 · `internal/router/service.go` · the exemption term is dropped, so an unmetered customer is refused again — kills TestAnUnmeteredCustomerUploadsWithNoCredits · acceptance-sha256:e16ac51638c00e0713734ecf3cbfc1e34a2dcf44b449a6ad4f1ed0cf407c41fe · covers:the unmetered exemption
- 2026-09-28 · ebde216* · mutant killed · exit 1 · `internal/router/service.go` · the exemption is written one line too high, so being unmetered walks past deactivation — the failure the position of this term exists to prevent · acceptance-sha256:e16ac51638c00e0713734ecf3cbfc1e34a2dcf44b449a6ad4f1ed0cf407c41fe · covers:the Active check that is NOT exempted

## Invariants

- `!u.Active` still refuses before credits are considered.
- The buffer limit still applies to an unmetered customer.
- `core.ErrNoCredits` keeps its meaning and its 402 mapping for every metered customer.

## Risks

- An exemption written above the `Active` check would make deactivation useless for exactly the
  customers whose work is free. S3's test is the guard, and it is why that test exists rather than
  being left to review.

## Stop Condition

Stop if a second producer of `core.ErrNoCredits` turns up outside `Upload` — the exemption would then
be in two places, and two places is a decision about where admission lives.

## Out of Scope

- The delivery side. T3 owns what an unmetered job is charged.
- Any cap on how much an unmetered customer may consume. (deferred: `docs/adr/BACKLOG.md`)

## Verification Log
- 2026-09-28 · ebde216* · exit 1 · `set -o pipefail …` · acceptance-sha256:e16ac51638c00e0713734ecf3cbfc1e34a2dcf44b449a6ad4f1ed0cf407c41fe · ms:1763
  ```
  --- last 7 line(s) of stdout
  --- FAIL: TestAnUnmeteredCustomerUploadsWithNoCredits (0.01s)
      unmetered_test.go:38: Upload for an unmetered customer with 0 credits = no credits, want success
  --- FAIL: TestAnUnmeteredCustomerIsStillBoundedByItsBufferLimit (0.00s)
      unmetered_test.go:102: Upload: no credits
  FAIL
  FAIL	github.com/atvirokodosprendimai/ocr-router/internal/router	0.478s
  FAIL
  ```
- 2026-09-28 · ebde216* · exit 0 · `set -o pipefail …` · acceptance-sha256:e16ac51638c00e0713734ecf3cbfc1e34a2dcf44b449a6ad4f1ed0cf407c41fe · ms:3068
- 2026-09-28 · ebde216* · exit 0 · `set -o pipefail …` · acceptance-sha256:e16ac51638c00e0713734ecf3cbfc1e34a2dcf44b449a6ad4f1ed0cf407c41fe · ms:1881
