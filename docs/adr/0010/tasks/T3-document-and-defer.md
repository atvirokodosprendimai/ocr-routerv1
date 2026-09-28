# Task ADR-0010-T3: Say what the counters mean, and file what this record deliberately did not build

**Depends-on:** T1, T2
**Covers:** none — no spec
**Estimated scope:** S (single file)
**Owner:** unassigned
**Produces:** none
**Consumes:** `views.Dashboard.Usage` and the `#usage` fragment (T2)
**Data dependency:** hermetic
**Proof map:** v1
**Rests-on:** none — the fence asserts one thing, that each claim is present where it was promised

## Goal

The README says what each window and each bucket counts, and the backlog carries the five things this
record deferred plus the partial take-up of the charts entry.

## Affected Files

| File | Change | Why |
|------|--------|-----|
| `README.md` | edit | the operator reading it must learn the COHORT RULE — that a window's numbers describe the jobs pushed then, not the jobs that finished then — because the obvious reading is the other one |
| `docs/adr/BACKLOG.md` | edit | five new deferred entries, and the charts entry annotated as PARTLY taken up: counts are a query, charts still need a retained series |

## Ordered Steps

1. [S1] Write the failing check first: the fence below greps for each claim and is red now. [proof: acceptance]
2. [S2] Document the four windows, naming the previous-month boundary as the previous COMPLETE calendar
   month in UTC, and say that rolling 31d and that column are different questions. [proof: acceptance]
3. [S3] Document the four buckets and the cohort rule, including that `dead` and `expired` are kept
   apart because one is a failing command and the other is a service nobody served, and that
   `pushed − the rest` is what is still in flight. [proof: acceptance]
4. [S4] Say that the counters are live over the existing stream and that the editable table is
   deliberately not, so nobody later "fixes" the asymmetry. [proof: acceptance]
5. [S5] File the five deferrals at their destination naming ADR-0010, and annotate the charts entry as
   partly taken up rather than moving it — the charting half is still open. [proof: acceptance]
6. [S6] Re-run `adr-debt docs/adr` and confirm nothing this record deferred is unreceipted. [proof: acceptance]

## Acceptance

```bash
set -o pipefail
grep -q 'previous complete calendar month' README.md && \
  grep -q 'still in flight' README.md && \
  grep -qi 'expired' README.md && \
  grep -c 'ADR-0010' docs/adr/BACKLOG.md | grep -qE '^[5-9]|^[0-9]{2,}' && \
  adr-debt docs/adr
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

- Every `(deferred: …)` entry ADR-0010 writes exists at its destination, naming ADR-0010.
- The charts entry stays in **Open**: this record took up counts, not charts.

## Risks

- A grep fence passes on a word's presence, not a sentence's quality. The cohort rule is the one claim
  where a vague sentence is actively misleading, so it is named in S3 for a reviewer to read.

## Stop Condition

Stop if `adr-debt` reports UNRECEIPTED for this record — a pointer written without the entry it points
at passes every other check there is.

## Out of Scope

- Any code change. T1 and T2 own the behaviour.

## Verification Log
