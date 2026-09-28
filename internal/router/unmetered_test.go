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
