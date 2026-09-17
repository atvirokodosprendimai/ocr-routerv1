package router_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// abandon claims the job and then lets the lease lapse, which is what a router
// restart or a killed worker looks like from the reaper's side.
func abandon(t *testing.T, h *harness, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.svc.Claim(ctx, "w1", "ocr", at); err != nil {
		t.Fatalf("Claim at %v: %v", at, err)
	}
	if _, err := h.svc.Reap(ctx, at.Add(10*time.Minute)); err != nil {
		t.Fatalf("Reap at %v: %v", at, err)
	}
}

// TestReclaimDoesNotSpendAnAttempt is the line this whole record turns on.
func TestReclaimDoesNotSpendAnAttempt(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	abandon(t, h, base)

	got, _ := h.repo.JobByID(context.Background(), j.ID)
	if got.State != core.JobQueued {
		t.Fatalf("state after a lapsed lease = %q, want queued", got.State)
	}
	if got.Attempts != 0 {
		t.Errorf("attempts = %d, want 0 — the worker vanished, its command never failed, so "+
			"nothing of the retry budget should have been spent", got.Attempts)
	}
	if got.Reclaims != 1 {
		t.Errorf("reclaims = %d, want 1 — the abandonment must be counted somewhere, or the "+
			"poison-pill bound has nothing to count", got.Reclaims)
	}
}

// TestThreeRestartsDoNotKillAJob is M's actual case, reported 2026-09-16: a
// worker restarted mid-process and the client hung, because the job had silently
// spent an attempt for something that was not a failure.
//
// MaxAttempts is 3 in this harness, so before ADR-0008 the third restart killed
// the job outright.
func TestThreeRestartsDoNotKillAJob(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	for i := 1; i <= 3; i++ {
		abandon(t, h, base.Add(time.Duration(i)*time.Hour))
	}

	got, _ := h.repo.JobByID(context.Background(), j.ID)
	if got.State != core.JobQueued {
		t.Fatalf("state after three restarts = %q, want queued — restarting the router three "+
			"times must not kill work whose command never ran badly once", got.State)
	}
	// Still runnable: the next worker to connect can claim it.
	if _, err := h.svc.Claim(context.Background(), "w2", "ocr", base.Add(5*time.Hour)); err != nil {
		t.Errorf("the survivor could not be claimed: %v — a job that is queued and unclaimable "+
			"is the same hang with a different state", err)
	}
}

// TestReclaimBudgetIsStillBounded: unbounded requeue is worse than the defect,
// because a poison-pill job that kills every worker it touches would have no
// terminal state at all.
func TestReclaimBudgetIsStillBounded(t *testing.T) {
	h := newHarnessWith(t, router.Config{
		Lease: 5 * time.Minute, MaxAttempts: 3, MaxReclaims: 2,
		AgingStep: time.Minute, LabelGrace: 5 * time.Minute,
		ResultTTL: time.Hour, DefaultLabel: "ocr",
	})
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	for i := 1; i <= 2; i++ {
		abandon(t, h, base.Add(time.Duration(i)*time.Hour))
	}

	got, _ := h.repo.JobByID(context.Background(), j.ID)
	if got.State != core.JobDead {
		t.Fatalf("state at the reclaim bound = %q, want dead — an unbounded reclaim loop has "+
			"no terminal state, which is worse than the defect being fixed", got.State)
	}
	// The reason must name WHICH budget ran out: "abandoned too many times" and
	// "attempts exhausted" are different facts about the job, and an operator
	// reading one must not conclude the other.
	if !strings.Contains(got.LastError, "abandon") {
		t.Errorf("terminal reason = %q, want one naming abandonment — otherwise a job killed by "+
			"flaky infrastructure reads as a job whose command kept failing", got.LastError)
	}
}

// TestFailureStillSpendsAnAttempt is the guard against the natural
// simplification: one counter with a flag, which re-merges exactly what this
// record separates.
func TestFailureStillSpendsAnAttempt(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	code := 9
	if err := h.svc.Fail(ctx, "w1", j.ID, "exit status 9: nope", &code, base); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	got, _ := h.repo.JobByID(ctx, j.ID)
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — a command that actually failed must still spend the "+
			"retry budget, or this change makes genuine failures free", got.Attempts)
	}
	if got.Reclaims != 0 {
		t.Errorf("reclaims = %d, want 0 — a reported failure is not an abandonment, and counting "+
			"it as both makes neither number mean anything", got.Reclaims)
	}
}
