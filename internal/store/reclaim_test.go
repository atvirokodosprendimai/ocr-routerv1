package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// claim puts a job in `processing` under a lease, which is the only state a
// reclaim is meaningful from.
func claim(t *testing.T, r *store.Repo, worker, label string) {
	t.Helper()
	if _, err := r.ClaimOneQueued(context.Background(), label, worker,
		base, base.Add(time.Minute), agingStep); err != nil {
		t.Fatalf("ClaimOneQueued: %v", err)
	}
}

// TestReclaimIncrementsReclaimsNotAttempts is the whole decision of ADR-0008,
// asserted on BOTH columns.
//
// ⚠ Moving only one of them is the defect. A reclaim that also spends an attempt
// makes a router restart look like the worker's command failing, and three
// restarts kill a job that never ran badly once.
func TestReclaimIncrementsReclaimsNotAttempts(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)
	mkJob(t, r, "j-reclaim", "u", "ocr", base, time.Time{})
	claim(t, r, "w1", "ocr")

	before, err := r.JobByID(ctx, "j-reclaim")
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if err := r.ReclaimJob(ctx, "j-reclaim", "router restarted", base); err != nil {
		t.Fatalf("ReclaimJob: %v", err)
	}
	got, err := r.JobByID(ctx, "j-reclaim")
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.Reclaims != before.Reclaims+1 {
		t.Errorf("Reclaims = %d, want %d — the abandonment was not counted at all",
			got.Reclaims, before.Reclaims+1)
	}
	if got.Attempts != before.Attempts {
		t.Errorf("Attempts moved from %d to %d on a reclaim — an abandoned lease spent the "+
			"job's failure budget, so restarting the router three times kills work that "+
			"never failed once", before.Attempts, got.Attempts)
	}
}

// TestRequeueIncrementsAttemptsNotReclaims is the mirror. Without it, "the two
// counters are separate" is asserted in one direction only, and an
// implementation that increments both on every path passes.
func TestRequeueIncrementsAttemptsNotReclaims(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)
	mkJob(t, r, "j-fail", "u", "ocr", base, time.Time{})
	claim(t, r, "w1", "ocr")

	if err := r.RequeueJob(ctx, "j-fail", "exit status 9: nope", intp(9), base); err != nil {
		t.Fatalf("RequeueJob: %v", err)
	}
	got, err := r.JobByID(ctx, "j-fail")
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 — a real failure must still spend the retry budget",
			got.Attempts)
	}
	if got.Reclaims != 0 {
		t.Errorf("Reclaims = %d, want 0 — a command that failed was not abandoned, and "+
			"counting it as both makes neither number mean anything", got.Reclaims)
	}
}

// TestReclaimClearsTheLease: the previous holder must not be able to land a late
// result on a job somebody else now owns.
func TestReclaimClearsTheLease(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)
	mkJob(t, r, "j-lease", "u", "ocr", base, time.Time{})
	claim(t, r, "w1", "ocr")

	if err := r.ReclaimJob(ctx, "j-lease", "worker released it", base); err != nil {
		t.Fatalf("ReclaimJob: %v", err)
	}
	got, err := r.JobByID(ctx, "j-lease")
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.State != "queued" {
		t.Errorf("State = %q, want queued — a reclaimed job nobody requeues is stranded", got.State)
	}
	if got.WorkerID != "" {
		t.Errorf("WorkerID = %q, want empty — the old holder still owns the row, so its late "+
			"result would be accepted", got.WorkerID)
	}
	if !got.LeaseExpiresAt.IsZero() {
		t.Errorf("LeaseExpiresAt = %v, want zero — a live lease on a queued job is a row two "+
			"workers can hold", got.LeaseExpiresAt)
	}
}
