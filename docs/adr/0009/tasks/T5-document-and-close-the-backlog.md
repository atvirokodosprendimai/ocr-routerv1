# Task ADR-0009-T5: Say what unmetered means where an operator will read it, and close the backlog entry

**Depends-on:** T1, T2, T3, T4
**Covers:** none — no spec
**Estimated scope:** S (single file)
**Owner:** unassigned
**Produces:** none
**Consumes:** `ocrr_credits_waived_total` (T3), `POST /admin/users/{id}/unmetered` (T4)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** none — the fence asserts one thing, that each claim is present where it was promised

## Goal

The README explains what an unmetered customer is and what the two credit counters mean, and the
backlog's **Open** entry moves to **Taken up** naming this record.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `README.md` | edit | an operator reading it must learn that a customer can be unmetered, how to do it, and that `ocrr_credits_debited_total` no longer answers "what did the work cost" on its own |
| `docs/adr/BACKLOG.md` | edit | move *"Unmetered customers — `-1` credits means infinite"* from **Open** to **Taken up**, naming ADR-0009 and saying what the record decided that the entry did not anticipate |

## Ordered Steps

1. [S1] Write the failing check first: the acceptance fence below greps for each claim and is red now,
   because none of the text exists. [proof: acceptance]
2. [S2] Add the README section: what unmetered means, that it is admin-set from the Customers table,
   that admission and the debit are the only two things it changes, and that deactivation and the
   buffer limit still apply. Say plainly that `-1` is not stored anywhere — the request was phrased
   that way and a reader will look for it. [proof: acceptance]
3. [S3] Document both counters beside each other: `ocrr_credits_debited_total` = credits actually
   taken from a balance; `ocrr_credits_waived_total` = what an unmetered delivery would have cost. A
   dashboard summing only the first under-reports consumption, deliberately. [proof: acceptance]
4. [S4] Move the backlog entry to **Taken up**, naming ADR-0009 and recording what the entry got
   right (the sentinel/flag conflict, the accrual question) and what it did not anticipate — that
   `Repo.DeliverJob`'s existing `charge != 0` guard means the store needed no change at all.
   [proof: acceptance]
5. [S5] Re-run `adr-debt docs/adr` and confirm this record's deferrals all have receipts at their
   destination: a pointer to `BACKLOG.md` is not the same as an entry in it, and this ADR files four.
   [proof: acceptance]

## Acceptance

```bash
set -o pipefail
grep -q 'unmetered' README.md && \
  grep -q 'ocrr_credits_waived_total' README.md && \
  grep -q 'ocrr_credits_debited_total' README.md && \
  grep -q 'ADR-0009' docs/adr/BACKLOG.md && \
  ! grep -A2 '^## Open' docs/adr/BACKLOG.md | grep -q 'Unmetered customers' && \
  "$(git rev-parse --show-toplevel)/../quality-harness/plugin/bin/adr-debt" docs/adr
```

## Tests

| Test name | File | Verifies | Covers | Steps |
|-----------|------|----------|--------|-------|
| — | — | documentation; the acceptance fence is the check | — | — |

## Reachability

| Rung | How this task shows it |
|------|------------------------|
| 1 — exists | the fence greps for each claim |
| 2 — something selects it | the README is the repository's front door; `adr-debt` reads BACKLOG.md and reports an unreceipted deferral |
| 3 — the caller can discover it | n/a: no declared interface |
| 4 — it is used | nothing measures whether documentation is read |

## Mutation Log

## Invariants

- Every `(deferred: …)` entry this ADR writes exists at its destination, naming ADR-0009.
- The backlog's **Open** section no longer carries the unmetered entry.

## Risks

- A grep-based fence passes on the presence of a word rather than on the quality of a sentence. That
  is what it can check; the sentences are a review matter and are named in the steps so a reviewer
  knows what to read for.

## Stop Condition

Stop if `adr-debt` reports an UNRECEIPTED deferral from this ADR — that means a pointer was written
without the entry it points at, which is the failure the deferral grammar exists to catch.

## Out of Scope

- Any code change. T1–T4 own the behaviour.

## Verification Log
