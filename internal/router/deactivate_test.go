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
