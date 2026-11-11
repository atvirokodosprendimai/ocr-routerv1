package router_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
	"github.com/atvirokodosprendimai/ocr-routerv1/internal/router"
)

// TestReapExpiresADeactivatedOwnersQueuedJobs is F11 (REPORT 2026-11-10).
// Claims skip a deactivated owner's jobs, so with no deadline they stayed
// queued forever. The reaper's next pass expires them — uncharged, blob
// gone, owner told, as any expiry — and says why in the log; another
// customer's queued job is untouched.
func TestReapExpiresADeactivatedOwnersQueuedJobs(t *testing.T) {
	h := newHarness(t)
	rec := &recordingLogger{}
	h.svc.SetLogger(rec)
	h.worker(t, "ocr")
	off := h.user(t, "off", 100, 4, 0) // no TTL: before F11 this job never ended
	h.user(t, "on", 100, 4, 0)
	ctx := context.Background()

	gone := upload(t, h, "off", router.UploadInput{Body: strings.NewReader("x")})
	kept := upload(t, h, "on", router.UploadInput{Body: strings.NewReader("y")})
	off.Active = false
	if err := h.repo.UpdateUser(ctx, off); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	rep, err := h.svc.Reap(ctx, base.Add(time.Second))
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if rep.JobsExpired != 1 {
		t.Errorf("JobsExpired = %d, want 1", rep.JobsExpired)
	}
	if got, _ := h.repo.JobByID(ctx, gone.ID); got.State != core.JobExpired {
		t.Errorf("the deactivated owner's job is %q, want expired", got.State)
	}
	if got, _ := h.repo.JobByID(ctx, kept.ID); got.State != core.JobQueued {
		t.Errorf("an active owner's job is %q, want queued", got.State)
	}
	e, ok := rec.find(core.JobQueued, core.JobExpired)
	if !ok || e.JobID != gone.ID || e.Reason != "owner deactivated" {
		t.Errorf("expiry log = %+v (found %v), want job %s with reason \"owner deactivated\"", e, ok, gone.ID)
	}
}

// deactivatedOwner makes "off" with a TTL of ttlSecs, uploads one job, and
// deactivates them.
func deactivatedOwner(t *testing.T, h *harness, ttlSecs int, reactivate bool) core.Job {
	t.Helper()
	ctx := context.Background()
	h.worker(t, "ocr")
	off := h.user(t, "off", 100, 4, ttlSecs)
	j := upload(t, h, "off", router.UploadInput{Body: strings.NewReader("x")})
	off.Active = false
	if err := h.repo.UpdateUser(ctx, off); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if reactivate {
		off.Active = true
		if err := h.repo.UpdateUser(ctx, off); err != nil {
			t.Fatalf("UpdateUser: %v", err)
		}
	}
	return j
}

// TestReapExpiresADeactivatedOwnersJobWithADeadlineAhead: the reason is the
// owner, not the deadline, when the deadline has not come yet.
func TestReapExpiresADeactivatedOwnersJobWithADeadlineAhead(t *testing.T) {
	h := newHarness(t)
	rec := &recordingLogger{}
	h.svc.SetLogger(rec)
	j := deactivatedOwner(t, h, 3600, false)
	if _, err := h.svc.Reap(context.Background(), base.Add(time.Second)); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	e, ok := rec.find(core.JobQueued, core.JobExpired)
	if !ok || e.JobID != j.ID || e.Reason != "owner deactivated" {
		t.Errorf("expiry log = %+v (found %v), want job %s with reason \"owner deactivated\"", e, ok, j.ID)
	}
}

// TestReactivationBeforeThePassKeepsTheJobs: deactivation takes effect at the
// reap, so an owner switched back on in time keeps the queue.
func TestReactivationBeforeThePassKeepsTheJobs(t *testing.T) {
	h := newHarness(t)
	j := deactivatedOwner(t, h, 0, true)
	rep, err := h.svc.Reap(context.Background(), base.Add(time.Second))
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	got, err := h.repo.JobByID(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if rep.JobsExpired != 0 || got.State != core.JobQueued {
		t.Errorf("after reactivation: JobsExpired = %d, job %q; want 0 and queued", rep.JobsExpired, got.State)
	}
}

// TestDeactivationLeavesARunningJobAlone: only queued rows expire; a job a
// worker holds runs to completion.
func TestDeactivationLeavesARunningJobAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.worker(t, "ocr")
	off := h.user(t, "off", 100, 4, 0)
	upload(t, h, "off", router.UploadInput{Body: strings.NewReader("x")})
	c, err := h.svc.Claim(ctx, "w1", "ocr", base)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	off.Active = false
	if err := h.repo.UpdateUser(ctx, off); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if _, err := h.svc.Reap(ctx, base.Add(time.Second)); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	got, err := h.repo.JobByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.State != core.JobProcessing {
		t.Errorf("a running job of a deactivated owner is %q, want processing", got.State)
	}
}
