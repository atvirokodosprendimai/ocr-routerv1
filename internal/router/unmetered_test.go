package router_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// unmetered marks a customer exempt from the balance, the way an administrator
// does: through the stored flag, never by writing a value into credits.
func unmetered(t *testing.T, h *harness, u core.User) core.User {
	t.Helper()
	u.Unmetered = true
	if err := h.repo.UpdateUser(context.Background(), u); err != nil {
		t.Fatalf("UpdateUser marking %s unmetered: %v", u.ID, err)
	}
	return u
}

// TestAnUnmeteredCustomerUploadsWithNoCredits is ADR-0009's served-path change.
//
// A customer who must never be refused had to be topped up by hand for ever,
// because admission gates on a positive balance and the flat `-1` that was asked
// for cannot be stored in a ledger column.
func TestAnUnmeteredCustomerUploadsWithNoCredits(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	unmetered(t, h, h.user(t, "u", 0, 4, 0))

	job, err := h.svc.Upload(context.Background(), "u", router.UploadInput{
		Filename: "a.pdf", Body: strings.NewReader("x"),
	}, base)
	if err != nil {
		t.Fatalf("Upload for an unmetered customer with 0 credits = %v, want success", err)
	}
	if job.State != core.JobQueued {
		t.Errorf("job state = %q, want %q", job.State, core.JobQueued)
	}
}

// TestUnmeteredDoesNotExemptAMeteredCustomer is the other half, and it is what
// makes the exemption an exemption rather than a removal.
//
// It is red if the `!u.Unmetered` term is dropped from the condition — which is
// this task's mutation, because dropping it leaves every test above green.
func TestUnmeteredDoesNotExemptAMeteredCustomer(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 0, 4, 0) // metered, no balance

	_, err := h.svc.Upload(context.Background(), "u", router.UploadInput{
		Filename: "a.pdf", Body: strings.NewReader("x"),
	}, base)
	if !errors.Is(err, core.ErrNoCredits) {
		t.Fatalf("Upload by a METERED customer with 0 credits = %v, want core.ErrNoCredits — "+
			"the exemption became unconditional and nobody is billed any more", err)
	}
}

// TestAnUnmeteredCustomerIsStillRefusedWhenInactive keeps the exemption below the
// deactivation check.
//
// ⚠ An exemption written one line too high would make `Disable` useless for
// exactly the customers whose work is free, and every other test in this file
// would stay green. Deactivation is the blunt instrument ADR-0001 named; being
// unmetered is not a way past it.
func TestAnUnmeteredCustomerIsStillRefusedWhenInactive(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	u := unmetered(t, h, h.user(t, "u", 0, 4, 0))

	u.Active = false
	if err := h.repo.UpdateUser(context.Background(), u); err != nil {
		t.Fatalf("UpdateUser deactivating: %v", err)
	}

	_, err := h.svc.Upload(context.Background(), "u", router.UploadInput{
		Filename: "a.pdf", Body: strings.NewReader("x"),
	}, base)
	if !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("Upload by a DEACTIVATED unmetered customer = %v, want core.ErrForbidden", err)
	}
}

// TestAnUnmeteredCustomerIsStillBoundedByItsBufferLimit keeps the exemption to
// what it is about.
//
// Unmetered is a statement about PRICE. The buffer limit is a statement about how
// much of the worker pool one customer may hold at once, and a customer who pays
// nothing has no claim to more of it than anyone else.
func TestAnUnmeteredCustomerIsStillBoundedByItsBufferLimit(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	unmetered(t, h, h.user(t, "u", 0, 2, 0))
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		upload(t, h, "u", router.UploadInput{Filename: "a.pdf", Body: strings.NewReader("x")})
	}
	_, err := h.svc.Upload(ctx, "u", router.UploadInput{
		Filename: "c.pdf", Body: strings.NewReader("x"),
	}, base)
	if !errors.Is(err, core.ErrBufferFull) {
		t.Errorf("the third upload by an unmetered customer at a limit of 2 = %v, "+
			"want core.ErrBufferFull", err)
	}
}

// deliveredUnitsJob runs one units job of `pages` pages all the way to the point
// of delivery and returns its id. The default rate is 1 credit per unit, so the
// accrued cost is `pages`.
func deliveredUnitsJob(t *testing.T, h *harness, userID string, pages int) string {
	t.Helper()
	ctx := context.Background()
	j := upload(t, h, userID, router.UploadInput{Body: strings.NewReader("src")})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	units := make([]string, pages)
	for i := range units {
		units[i] = "page"
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, units, base); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return j.ID
}

// TestAnUnmeteredDeliveryDebitsNothingAndStillAccrues is the central claim of
// ADR-0009, and it is asserted on BOTH tables because they answer different
// questions: `users.credits` is what we may bill, `jobs.accrued_credits` is what
// the work cost. "We cannot bill it" and "we cannot see what it cost" are
// different statements, and the record chose the first without the second.
func TestAnUnmeteredDeliveryDebitsNothingAndStillAccrues(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	unmetered(t, h, h.user(t, "u", 100, 4, 0))
	ctx := context.Background()

	id := deliveredUnitsJob(t, h, "u", 3)
	if _, err := h.svc.Deliver(ctx, "u", id, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	u, err := h.repo.UserByID(ctx, "u")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if u.Credits != 100 {
		t.Errorf("credits = %d after an unmetered delivery, want 100 — the balance moved", u.Credits)
	}

	job, err := h.repo.JobByID(ctx, id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job.AccruedCredits != 3 {
		t.Errorf("accrued_credits = %d, want 3 — waiving the CHARGE must not stop the job "+
			"recording what the work cost, or an unmetered customer's usage becomes invisible",
			job.AccruedCredits)
	}
}

// TestAnUnmeteredDeliveryWritesNoLedgerRow is the half a balance assertion cannot
// see.
//
// A zero-delta `credit_entries` row would keep the balance correct and make the
// audit claim a movement that never happened. Nothing moved, so the ledger has
// nothing to record.
func TestAnUnmeteredDeliveryWritesNoLedgerRow(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	unmetered(t, h, h.user(t, "u", 100, 4, 0))
	ctx := context.Background()

	id := deliveredUnitsJob(t, h, "u", 3)
	if _, err := h.svc.Deliver(ctx, "u", id, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	entries, err := h.repo.Ledger(ctx, "u", 100)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	for _, e := range entries {
		if e.JobID == id {
			t.Errorf("the ledger holds an entry for an unmetered delivery (delta %d, reason %q) — "+
				"an audit of movements must not record one that did not happen", e.Delta, e.Reason)
		}
	}
}

// TestUnmeteredDoesNotWaiveAMeteredDelivery keeps the waiver conditional. It is
// red if the charge becomes 0 for everyone, which is the mutation for this task.
func TestUnmeteredDoesNotWaiveAMeteredDelivery(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0) // metered
	ctx := context.Background()

	id := deliveredUnitsJob(t, h, "u", 3)
	if _, err := h.svc.Deliver(ctx, "u", id, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	u, err := h.repo.UserByID(ctx, "u")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if u.Credits != 97 {
		t.Errorf("credits = %d after a METERED 3-unit delivery, want 97 — nobody is being "+
			"billed any more", u.Credits)
	}
}

// TestAnUnmeteredRawDeliveryWaivesTheFlatCredit covers the second delivery path.
//
// ⚠ Asserted as the NUMBERS, following ADR-0006's own lesson on this exact code:
// a raw job that falls through to the accrued per-unit total charges ZERO, which
// is a failure in the customer's favour that nothing anywhere reports. Here the
// zero is intended — so the waived counter is what distinguishes "waived on
// purpose" from "priced at nothing by accident".
func TestAnUnmeteredRawDeliveryWaivesTheFlatCredit(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	unmetered(t, h, h.user(t, "u", 100, 4, 0))
	ctx := context.Background()
	rec := newRecorder()
	h.svc.SetCounter(rec)

	id := rawJobDone(t, h, "convert", "\x89PNG")
	f, err := h.svc.DeliverRaw(ctx, "u", id, base)
	if err != nil {
		t.Fatalf("DeliverRaw: %v", err)
	}
	_ = f.Close()

	u, err := h.repo.UserByID(ctx, "u")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if u.Credits != 100 {
		t.Errorf("credits = %d after an unmetered RAW delivery, want 100", u.Credits)
	}
	if got := rec.value("ocrr_credits_debited_total", nil); got != 0 {
		t.Errorf("ocrr_credits_debited_total = %d, want 0 — nothing was taken from a balance", got)
	}
	if got := rec.value("ocrr_credits_waived_total", nil); got != 1 {
		t.Errorf("ocrr_credits_waived_total = %d, want 1 — a raw job's flat credit was waived "+
			"and the waiver has to be visible", got)
	}
}

// TestWaivedAndDebitedAreNeverBothCounted is the invariant that keeps the two
// series readable: one delivery moves exactly one of them, by the amount the
// other did not get.
func TestWaivedAndDebitedAreNeverBothCounted(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "metered", 100, 4, 0)
	unmetered(t, h, h.user(t, "free", 100, 4, 0))
	ctx := context.Background()
	rec := newRecorder()
	h.svc.SetCounter(rec)

	meteredID := deliveredUnitsJob(t, h, "metered", 3)
	if _, err := h.svc.Deliver(ctx, "metered", meteredID, base); err != nil {
		t.Fatalf("Deliver metered: %v", err)
	}
	freeID := deliveredUnitsJob(t, h, "free", 5)
	if _, err := h.svc.Deliver(ctx, "free", freeID, base); err != nil {
		t.Fatalf("Deliver unmetered: %v", err)
	}

	if got := rec.value("ocrr_credits_debited_total", nil); got != 3 {
		t.Errorf("ocrr_credits_debited_total = %d, want 3 — only the metered delivery's cost", got)
	}
	if got := rec.value("ocrr_credits_waived_total", nil); got != 5 {
		t.Errorf("ocrr_credits_waived_total = %d, want 5 — only the unmetered delivery's cost", got)
	}
}
