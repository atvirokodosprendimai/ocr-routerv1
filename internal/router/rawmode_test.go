package router_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// TestJobStampsRawAtAdmission is the immutability half of ADR-0006's mode.
//
// The stamp exists so that a job carries the mode it was ADMITTED under. If the
// mode were looked up at delivery instead, an administrator editing a service
// would silently reprice every job already in flight — in either direction, and
// with no record of what the customer actually agreed to.
func TestJobStampsRawAtAdmission(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	if err := h.repo.SetRate(ctx, "convert", 1, true, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}

	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"convert"},
		Body:     strings.NewReader("x"),
		Raw:      true,
	})
	if !j.Raw {
		t.Fatal("the admitted job was not stamped raw")
	}

	// The administrator changes their mind AFTER the job is queued.
	if err := h.repo.SetRate(ctx, "convert", 1, false, base); err != nil {
		t.Fatalf("SetRate back to units: %v", err)
	}

	got, err := h.repo.JobByID(ctx, j.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if !got.Raw {
		t.Error("a job in flight changed mode when the SERVICE was edited — its price and its " +
			"output shape must both be fixed at admission")
	}
}

// TestUploadRefusesModeMismatch covers the admission refusal at the service
// layer, below the HTTP boundary — the handler is not the only caller of Upload.
func TestUploadRefusesModeMismatch(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	if err := h.repo.SetRate(ctx, "ocr", 3, false, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}

	_, err := h.svc.Upload(ctx, "u", router.UploadInput{
		Pipeline: []string{"ocr"},
		Body:     strings.NewReader("x"),
		Raw:      true,
	}, base)
	if !errors.Is(err, core.ErrModeMismatch) {
		t.Fatalf("Upload with a mismatched mode = %v, want ErrModeMismatch", err)
	}

	jobs, _ := h.repo.ListJobs(ctx, 10)
	if len(jobs) != 0 {
		t.Errorf("a refused upload created %d rows, want 0", len(jobs))
	}
}
