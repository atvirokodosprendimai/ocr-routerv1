package router_test

import (
	"context"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// rawJobDone admits a raw job, leases it, and completes it with body.
func rawJobDone(t *testing.T, h *harness, label, body string) string {
	t.Helper()
	ctx := context.Background()
	if err := h.repo.SetRate(ctx, label, 5, true, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}
	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{label}, Body: strings.NewReader("src"), Raw: true,
	})
	if _, err := h.svc.Claim(ctx, "w1", label, base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.CompleteRaw(ctx, "w1", j.ID, strings.NewReader(body), base); err != nil {
		t.Fatalf("CompleteRaw: %v", err)
	}
	return j.ID
}

// TestRawJobCostsOneCreditRegardlessOfSize is ADR-0006's pricing rule.
//
// ⚠ The assertion is on the NUMBER, not on "a charge happened". The obvious
// wrong implementation lets a raw job fall through to len(units) * rate with an
// empty unit list, which charges ZERO — a failure in the customer's favour that
// nothing would ever report.
func TestRawJobCostsOneCreditRegardlessOfSize(t *testing.T) {
	for _, size := range []int{1, 1 << 20} {
		h := newHarness(t)
		h.worker(t, "convert")
		h.user(t, "u", 100, 4, 0)
		ctx := context.Background()

		id := rawJobDone(t, h, "convert", strings.Repeat("x", size))
		before, err := h.repo.UserByID(ctx, "u")
		if err != nil {
			t.Fatalf("UserByID: %v", err)
		}
		f, err := h.svc.DeliverRaw(ctx, "u", id, base)
		if err != nil {
			t.Fatalf("DeliverRaw: %v", err)
		}
		_ = f.Close()

		after, err := h.repo.UserByID(ctx, "u")
		if err != nil {
			t.Fatalf("UserByID: %v", err)
		}
		if spent := before.Credits - after.Credits; spent != 1 {
			t.Errorf("a %d-byte raw job cost %d credit(s), want exactly 1 — the service's rate is 5 "+
				"per unit and a raw job has no units, so both 0 and 5 are wrong", size, spent)
		}
	}
}

// TestFailedRawJobCostsNothing keeps the charge-on-delivery rule: a job that
// dies costs the customer nothing.
func TestFailedRawJobCostsNothing(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	if err := h.repo.SetRate(ctx, "convert", 5, true, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}
	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"convert"}, Body: strings.NewReader("src"), Raw: true,
	})
	for i := 0; i < 3; i++ {
		if _, err := h.svc.Claim(ctx, "w1", "convert", base); err != nil {
			break
		}
		if err := h.svc.Fail(ctx, "w1", j.ID, "boom", base); err != nil {
			t.Fatalf("Fail: %v", err)
		}
	}

	u, err := h.repo.UserByID(ctx, "u")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if u.Credits != 100 {
		t.Errorf("a raw job that never delivered cost %d credit(s), want 0", 100-u.Credits)
	}
}

// TestUnitsPricingUnchanged is the other half: this ADR must not move the price
// of anything that was not raw.
func TestUnitsPricingUnchanged(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	if err := h.repo.SetRate(ctx, "ocr", 3, false, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src"), Pipeline: []string{"ocr"}})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"a", "b", "c", "d"}, base); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	before, _ := h.repo.UserByID(ctx, "u")
	if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	after, _ := h.repo.UserByID(ctx, "u")
	if spent := before.Credits - after.Credits; spent != 12 {
		t.Errorf("a 4-unit job at rate 3 cost %d, want 12 — units pricing must be untouched", spent)
	}
}
