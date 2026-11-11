package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
	"github.com/atvirokodosprendimai/ocr-routerv1/internal/store"
)

// claim puts a job in `processing` under a lease, which is the only state a
// reclaim is meaningful from. Its lease token is "lease-"+worker.
func claim(t *testing.T, r *store.Repo, worker, label string) {
	t.Helper()
	claimAs(t, r, worker, "lease-"+worker, label)
}

// claimAs is claim with a chosen lease token, for the tests where one worker
// holds the same job twice under two claims (ADR-0013).
func claimAs(t *testing.T, r *store.Repo, worker, lease, label string) {
	t.Helper()
	if _, err := r.ClaimOneQueued(context.Background(), label, worker, lease,
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
	if err := r.ReclaimJob(ctx, "j-reclaim", "w1", "lease-w1", "router restarted", base); err != nil {
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

	if err := r.RequeueJob(ctx, "j-fail", "w1", "lease-w1", "exit status 9: nope", intp(9), base); err != nil {
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

	if err := r.ReclaimJob(ctx, "j-lease", "w1", "lease-w1", "worker released it", base); err != nil {
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
	if got.LeaseToken != "" {
		t.Errorf("LeaseToken = %q, want empty — the old claim's token still matches the row, "+
			"so its late result would be accepted (ADR-0013)", got.LeaseToken)
	}
}

// TestProcessingWritersLeaveADeliveredJobAlone is F4 (REPORT 2026-11-10). The
// reaper and the failure paths act on a row they read earlier; if the job was
// completed and delivered in between, their write must match nothing rather
// than move a paid job back to `queued` (charged again on the next delivery) or
// to `dead`.
func TestProcessingWritersLeaveADeliveredJobAlone(t *testing.T) {
	writers := map[string]func(r *store.Repo, id string) error{
		"RequeueJob": func(r *store.Repo, id string) error {
			return r.RequeueJob(context.Background(), id, "w1", "lease-w1", "late failure", nil, base)
		},
		"ReclaimJob": func(r *store.Repo, id string) error {
			return r.ReclaimJob(context.Background(), id, "w1", "lease-w1", "stale lease", base)
		},
		"ReleaseJob": func(r *store.Repo, id string) error {
			return r.ReleaseJob(context.Background(), id, "w1", "lease-w1", "late release", base)
		},
		"FailJobDead": func(r *store.Repo, id string) error {
			return r.FailJobDead(context.Background(), id, "w1", "lease-w1", "late death", nil, base)
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			r := newRepo(t)
			ctx := context.Background()
			mkUser(t, r, "u", 0, 10)
			mkJob(t, r, "j", "u", "ocr", base, time.Time{})
			claim(t, r, "w1", "ocr")
			if err := r.CompleteJob(ctx, "j", "w1", "lease-w1", 1, 1, base); err != nil {
				t.Fatalf("CompleteJob: %v", err)
			}
			if err := r.DeliverJob(ctx, "j", "u", 1, base); err != nil {
				t.Fatalf("DeliverJob: %v", err)
			}

			if err := write(r, "j"); !errors.Is(err, core.ErrNotFound) {
				t.Errorf("%s on a delivered job = %v, want core.ErrNotFound", name, err)
			}
			got, _ := r.JobByID(ctx, "j")
			if got.State != core.JobDelivered {
				t.Errorf("%s moved a delivered job to %q — the next delivery charges it again",
					name, got.State)
			}
		})
	}
}

// TestWritersRefuseAWorkerThatLostTheJob is F5 (REPORT 2026-11-10). The lease
// check in the router is a read; between it and the write the reaper can take
// the job back and another worker claim it. A write guarded on state alone then
// lands on the new holder's row — the old worker's output accepted, or the new
// worker's job requeued under it.
func TestWritersRefuseAWorkerThatLostTheJob(t *testing.T) {
	writers := map[string]func(r *store.Repo, id string) error{
		"CompleteJob": func(r *store.Repo, id string) error {
			return r.CompleteJob(context.Background(), id, "w1", "lease-w1", 1, 1, base)
		},
		"AdvanceStage": func(r *store.Repo, id string) error {
			return r.AdvanceStage(context.Background(), id, "w1", "lease-w1", "next", 1, base)
		},
		"RequeueJob": func(r *store.Repo, id string) error {
			return r.RequeueJob(context.Background(), id, "w1", "lease-w1", "late failure", nil, base)
		},
		"ReleaseJob": func(r *store.Repo, id string) error {
			return r.ReleaseJob(context.Background(), id, "w1", "lease-w1", "late release", base)
		},
		"FailJobDead": func(r *store.Repo, id string) error {
			return r.FailJobDead(context.Background(), id, "w1", "lease-w1", "late death", nil, base)
		},
		"ReclaimJob": func(r *store.Repo, id string) error {
			return r.ReclaimJob(context.Background(), id, "w1", "lease-w1", "stale lease", base)
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			r := newRepo(t)
			ctx := context.Background()
			mkUser(t, r, "u", 0, 10)
			mkJob(t, r, "j", "u", "ocr", base, time.Time{})
			claim(t, r, "w1", "ocr")
			if err := r.ReclaimJob(ctx, "j", "w1", "lease-w1", "lease expired", base); err != nil {
				t.Fatalf("ReclaimJob: %v", err)
			}
			claim(t, r, "w2", "ocr")

			if err := write(r, "j"); !errors.Is(err, core.ErrNotFound) {
				t.Errorf("%s by the worker that lost the job = %v, want core.ErrNotFound", name, err)
			}
			got, err := r.JobByID(ctx, "j")
			if err != nil {
				t.Fatalf("JobByID: %v", err)
			}
			if got.State != core.JobProcessing || got.WorkerID != "w2" {
				t.Errorf("%s left the job %q under %q, want processing under w2", name, got.State, got.WorkerID)
			}
		})
	}
}

// TestWritersRefuseAStaleClaimOfTheSameWorker is ADR-0013. The F5 guard above
// compares worker_id, the worker's TOKEN id, so the SAME worker token claiming
// the job again after a reclaim (another slot of one process, a second process
// on one token) passed it, and the older claim's write landed on the newer
// claim's row. Each writer must match only the current claim's lease token.
func TestWritersRefuseAStaleClaimOfTheSameWorker(t *testing.T) {
	writers := map[string]func(r *store.Repo, id string) error{
		"CompleteJob": func(r *store.Repo, id string) error {
			return r.CompleteJob(context.Background(), id, "w1", "lease-old", 1, 1, base)
		},
		"AdvanceStage": func(r *store.Repo, id string) error {
			return r.AdvanceStage(context.Background(), id, "w1", "lease-old", "next", 1, base)
		},
		"RequeueJob": func(r *store.Repo, id string) error {
			return r.RequeueJob(context.Background(), id, "w1", "lease-old", "late failure", nil, base)
		},
		"ReleaseJob": func(r *store.Repo, id string) error {
			return r.ReleaseJob(context.Background(), id, "w1", "lease-old", "late release", base)
		},
		"FailJobDead": func(r *store.Repo, id string) error {
			return r.FailJobDead(context.Background(), id, "w1", "lease-old", "late death", nil, base)
		},
		"ReclaimJob": func(r *store.Repo, id string) error {
			return r.ReclaimJob(context.Background(), id, "w1", "lease-old", "stale lease", base)
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			r := newRepo(t)
			ctx := context.Background()
			mkUser(t, r, "u", 0, 10)
			mkJob(t, r, "j", "u", "ocr", base, time.Time{})
			claimAs(t, r, "w1", "lease-old", "ocr")
			if err := r.ReclaimJob(ctx, "j", "w1", "lease-old", "lease expired", base); err != nil {
				t.Fatalf("ReclaimJob: %v", err)
			}
			claimAs(t, r, "w1", "lease-new", "ocr")

			if err := write(r, "j"); !errors.Is(err, core.ErrNotFound) {
				t.Errorf("%s under the older claim = %v, want core.ErrNotFound", name, err)
			}
			got, err := r.JobByID(ctx, "j")
			if err != nil {
				t.Fatalf("JobByID: %v", err)
			}
			if got.State != core.JobProcessing || got.WorkerID != "w1" || got.LeaseToken != "lease-new" {
				t.Errorf("%s left the job %q under %q/%q, want processing under w1/lease-new",
					name, got.State, got.WorkerID, got.LeaseToken)
			}
		})
	}
}

// TestCompleteRefusesAnExpiredLease: the final-stage and advance writes also
// check the lease itself, so a result posted after the lease lapsed is refused
// even before the reaper has run (F5).
func TestCompleteRefusesAnExpiredLease(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)
	mkJob(t, r, "j", "u", "ocr", base, time.Time{})
	claim(t, r, "w1", "ocr") // lease until base+1m
	// AT the expiry instant, not only after it: the lease is live strictly
	// before lease_expires_at, as holdsLease's After(now) says.
	if err := r.CompleteJob(ctx, "j", "w1", "lease-w1", 1, 1, base.Add(time.Minute)); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("CompleteJob at the lease's expiry = %v, want core.ErrNotFound", err)
	}
	if err := r.AdvanceStage(ctx, "j", "w1", "lease-w1", "next", 1, base.Add(time.Minute)); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("AdvanceStage at the lease's expiry = %v, want core.ErrNotFound", err)
	}
	if err := r.CompleteJob(ctx, "j", "w1", "lease-w1", 1, 1, base.Add(time.Minute-time.Second)); err != nil {
		t.Errorf("CompleteJob a second before the expiry = %v, want nil", err)
	}
}
