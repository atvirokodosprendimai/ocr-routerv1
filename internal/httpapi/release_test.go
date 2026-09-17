package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// otherWorkerToken mints a SECOND worker, so the guard can be shown to refuse
// the right one rather than everything.
func (e *env) otherWorkerToken(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ap, err := e.ident.Authenticate(ctx, "Bearer "+e.adminTok, base)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	u, err := e.ident.CreateUser(ctx, ap, "w2@example.com", core.RoleWorker, base)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, tok, err := e.ident.MintToken(ctx, ap, u.ID, core.RoleWorker, "w2", base)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	return tok
}

// TestReleaseRequeuesWithoutSpendingEitherBudget is the whole point of the route.
//
// A cooperative handover is evidence of nothing wrong: the operator stopped the
// worker. Charging it to either counter would make an orderly restart look like
// something the job did.
func TestReleaseRequeuesWithoutSpendingEitherBudget(t *testing.T) {
	e := newEnv(t)
	id := e.unitsJobInFlight(t, "ocr")

	resp := e.do(t, "POST", "/release?job_id="+id, e.workerTok, nil, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("release = %d, want 204", resp.StatusCode)
	}

	job, err := e.repo.JobByID(context.Background(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job.State != core.JobQueued {
		t.Errorf("state after release = %q, want queued", job.State)
	}
	if job.Attempts != 0 {
		t.Errorf("attempts = %d, want 0 — a handover is not a failure", job.Attempts)
	}
	if job.Reclaims != 0 {
		t.Errorf("reclaims = %d, want 0 — the worker handed the job back rather than "+
			"abandoning it, and spending the abandonment budget for an orderly restart is "+
			"the same mistake at one remove", job.Reclaims)
	}
	if job.WorkerID != "" {
		t.Errorf("worker_id = %q, want empty", job.WorkerID)
	}
}

// TestReleaseWithoutTheLeaseIsRefused asserts BOTH directions: a guard that
// refuses everything would pass the first half alone.
func TestReleaseWithoutTheLeaseIsRefused(t *testing.T) {
	e := newEnv(t)
	id := e.unitsJobInFlight(t, "ocr")
	other := e.otherWorkerToken(t)

	if resp := e.do(t, "POST", "/release?job_id="+id, other, nil, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("a worker that does not hold the lease released it: %d, want 409 — any worker "+
			"could then requeue a job another is running", resp.StatusCode)
	}
	if resp := e.do(t, "POST", "/release?job_id="+id, e.workerTok, nil, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("the LEASE HOLDER was refused: %d, want 204 — a guard that refuses everyone "+
			"is not a guard, it is an outage", resp.StatusCode)
	}
}

// TestReleasedJobIsImmediatelyClaimable is the user-visible point: seconds
// instead of a full lease.
func TestReleasedJobIsImmediatelyClaimable(t *testing.T) {
	e := newEnv(t)
	id := e.unitsJobInFlight(t, "ocr")

	if resp := e.do(t, "POST", "/release?job_id="+id, e.workerTok, nil, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("release = %d, want 204", resp.StatusCode)
	}
	c := e.do(t, "POST", "/claim?label=ocr&raw=0", e.otherWorkerToken(t), nil, "")
	if c.StatusCode != http.StatusOK {
		t.Fatalf("claim after release = %d, want 200 — the released job is not back in the "+
			"queue, so the restart still costs a full lease", c.StatusCode)
	}
}

// TestLateResultAfterReleaseIsRefused is why the release clears worker_id: the
// released worker's subprocess can still be running.
func TestLateResultAfterReleaseIsRefused(t *testing.T) {
	e := newEnv(t)
	id := e.unitsJobInFlight(t, "ocr")

	if resp := e.do(t, "POST", "/release?job_id="+id, e.workerTok, nil, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("release = %d, want 204", resp.StatusCode)
	}
	late := e.do(t, "POST", "/upload", e.workerTok,
		bytes.NewReader([]byte(`{"job_id":"`+id+`","units":["a","b"]}`)), "application/json")
	if late.StatusCode != http.StatusConflict {
		t.Errorf("a late result from the released worker = %d, want 409 — it would otherwise "+
			"land on a job another worker now holds", late.StatusCode)
	}
}
