package store_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// base is a fixed instant so every age in these tests is exact rather than
// relative to a wall clock that moves mid-test.
var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

const agingStep = time.Minute

func newRepo(t *testing.T) *store.Repo {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "repo.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store.NewRepo(db)
}

func mkUser(t *testing.T, r *store.Repo, id string, priority, credits int) core.User {
	t.Helper()
	u := core.User{
		ID: id, Email: id + "@example.com", Role: core.RoleClient,
		Credits: credits, BufferLimit: 4, Priority: priority,
		Active: true, CreatedAt: base,
	}
	if err := r.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser(%s): %v", id, err)
	}
	return u
}

// mkJob inserts a queued job owned by userID, queued at the given time.
func mkJob(t *testing.T, r *store.Repo, id, userID, label string, queuedAt time.Time, expires time.Time) core.Job {
	t.Helper()
	j := core.Job{
		ID: id, UserID: userID, Filename: "f.pdf", Label: label,
		Pipeline: []string{label}, Stage: 0, Params: map[string]string{},
		HasBlob: true, State: core.JobQueued,
		QueuedAt: queuedAt, ExpiresAt: expires, CreatedAt: queuedAt, UpdatedAt: queuedAt,
	}
	if err := r.CreateJob(context.Background(), j); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
	return j
}

func TestClaimPrefersHigherPriority(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "vip", 100, 10)
	mkUser(t, r, "bulk", 0, 10)

	// Both queued at the same instant, so aging contributes equally and only the
	// base priority can decide.
	mkJob(t, r, "j-bulk", "bulk", "ocr", base, time.Time{})
	mkJob(t, r, "j-vip", "vip", "ocr", base, time.Time{})

	got, err := r.ClaimOneQueued(ctx, "ocr", "w1", base, base.Add(time.Minute), agingStep)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.ID != "j-vip" {
		t.Errorf("claimed %q, want %q — higher priority must be taken first", got.ID, "j-vip")
	}
}

// TestClaimAgingOvertakesPriority asserts the OVERTAKE itself.
//
// A test that only checks "VIP first" passes both with aging and with strict
// priority, so it cannot detect the aging term being dropped or truncated to
// zero by integer division. This one fails in that case, which is the whole
// reason it is written as an overtake rather than as a formula check.
func TestClaimAgingOvertakesPriority(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "vip", 100, 10)
	mkUser(t, r, "bulk", 0, 10)

	// bulk has waited 200 minutes; with a 1-minute step that is +200 effective
	// priority, which beats the VIP's flat 100.
	mkJob(t, r, "j-bulk-old", "bulk", "ocr", base.Add(-200*time.Minute), time.Time{})
	mkJob(t, r, "j-vip-new", "vip", "ocr", base, time.Time{})

	got, err := r.ClaimOneQueued(ctx, "ocr", "w1", base, base.Add(time.Minute), agingStep)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.ID != "j-bulk-old" {
		t.Errorf("claimed %q, want %q — a long-waiting low-priority job must overtake a "+
			"fresh high-priority one, or the bottom tier starves forever", got.ID, "j-bulk-old")
	}
}

func TestClaimAgingDoesNotOvertakeTooSoon(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "vip", 100, 10)
	mkUser(t, r, "bulk", 0, 10)

	// Only 10 minutes of waiting: +10 against the VIP's 100. The VIP still wins.
	// Without this, an aging term that was far too aggressive would still pass
	// the overtake test above.
	mkJob(t, r, "j-bulk", "bulk", "ocr", base.Add(-10*time.Minute), time.Time{})
	mkJob(t, r, "j-vip", "vip", "ocr", base, time.Time{})

	got, err := r.ClaimOneQueued(ctx, "ocr", "w1", base, base.Add(time.Minute), agingStep)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.ID != "j-vip" {
		t.Errorf("claimed %q, want %q — 10 minutes of aging must not beat 100 points of "+
			"priority at a 1-minute step", got.ID, "j-vip")
	}
}

func TestClaimIsFifoWithinATier(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)

	// Same user, same queued_at: only id order can decide, and ids are uuidv7 so
	// id order is arrival order.
	first := core.NewID()
	second := core.NewID()
	mkJob(t, r, first, "u", "ocr", base, time.Time{})
	mkJob(t, r, second, "u", "ocr", base, time.Time{})

	got, err := r.ClaimOneQueued(ctx, "ocr", "w1", base, base.Add(time.Minute), agingStep)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.ID != first {
		t.Errorf("claimed %q, want the earlier id %q — equal effective priority must be FIFO", got.ID, first)
	}
}

func TestClaimFiltersByLabel(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "vip", 100, 10)
	mkUser(t, r, "bulk", 0, 10)

	// The ocr job has far higher effective priority, and must still be invisible
	// to a strip-html worker. Label isolation is what lets one queue serve many
	// services.
	mkJob(t, r, "j-ocr", "vip", "ocr", base.Add(-500*time.Minute), time.Time{})
	mkJob(t, r, "j-strip", "bulk", "strip-html", base, time.Time{})

	got, err := r.ClaimOneQueued(ctx, "strip-html", "w1", base, base.Add(time.Minute), agingStep)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.ID != "j-strip" {
		t.Errorf("claimed %q, want %q — a worker must never receive another label's job", got.ID, "j-strip")
	}
	if got.Label != "strip-html" {
		t.Errorf("claimed label %q, want %q", got.Label, "strip-html")
	}
}

func TestClaimLabelEmptyQueue(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)
	mkJob(t, r, "j-ocr", "u", "ocr", base, time.Time{})

	_, err := r.ClaimOneQueued(ctx, "strip-html", "w1", base, base.Add(time.Minute), agingStep)
	if err != core.ErrNotFound {
		t.Errorf("claim on a label with no work = %v, want core.ErrNotFound — it must not "+
			"fall through to another label's queue", err)
	}
}

func TestClaimSkipsExpired(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "vip", 100, 10)
	mkUser(t, r, "u", 0, 10)

	// The expired job has the highest effective priority by far and must still be
	// passed over: the deadline predicate lives in the same WHERE as the
	// selection, so there is no window in which it could be claimed.
	mkJob(t, r, "j-expired", "vip", "ocr", base.Add(-999*time.Minute), base.Add(-time.Second))
	mkJob(t, r, "j-live", "u", "ocr", base, time.Time{})

	got, err := r.ClaimOneQueued(ctx, "ocr", "w1", base, base.Add(time.Minute), agingStep)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.ID != "j-live" {
		t.Errorf("claimed %q, want %q — a job past its deadline must never be claimed", got.ID, "j-live")
	}
}

func TestClaimOneQueuedIsAtomic(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)
	mkJob(t, r, "only", "u", "ocr", base, time.Time{})

	const workers = 16
	var (
		mu      sync.Mutex
		winners []string
		wg      sync.WaitGroup
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			j, err := r.ClaimOneQueued(ctx, "ocr", "w", base, base.Add(time.Minute), agingStep)
			if err == nil {
				mu.Lock()
				winners = append(winners, j.ID)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(winners) != 1 {
		t.Errorf("%d workers claimed one job and %d won: %v — first-claim-wins must yield "+
			"exactly one winner", workers, len(winners), winners)
	}
}

func TestClaimOneQueuedEmpty(t *testing.T) {
	r := newRepo(t)
	_, err := r.ClaimOneQueued(context.Background(), "ocr", "w1", base, base.Add(time.Minute), agingStep)
	if err != core.ErrNotFound {
		t.Errorf("claim on an empty queue = %v, want core.ErrNotFound (not a zero-value job)", err)
	}
}

func TestAdvanceStagePreservesQueuedAt(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)

	queued := base.Add(-30 * time.Minute)
	j := core.Job{
		ID: "p1", UserID: "u", Label: "crawl",
		Pipeline: []string{"crawl", "strip-html"}, Stage: 0,
		Params: map[string]string{}, HasBlob: true, State: core.JobQueued,
		QueuedAt: queued, CreatedAt: queued, UpdatedAt: queued,
	}
	if err := r.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := r.ClaimOneQueued(ctx, "crawl", "w1", base, base.Add(time.Minute), agingStep); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := r.AdvanceStage(ctx, "p1", "strip-html", 3, base); err != nil {
		t.Fatalf("AdvanceStage: %v", err)
	}

	got, err := r.JobByID(ctx, "p1")
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.State != core.JobQueued {
		t.Errorf("state = %q, want queued", got.State)
	}
	if got.Label != "strip-html" {
		t.Errorf("label = %q, want strip-html", got.Label)
	}
	if got.Stage != 1 {
		t.Errorf("stage = %d, want 1", got.Stage)
	}
	if got.AccruedCredits != 3 {
		t.Errorf("accrued = %d, want 3", got.AccruedCredits)
	}
	if !got.QueuedAt.Equal(queued) {
		t.Errorf("queued_at = %v, want %v — advancing a stage must NOT reset the age the job "+
			"has accrued, or long pipelines starve behind fresh work", got.QueuedAt, queued)
	}
	if got.WorkerID != "" {
		t.Errorf("worker_id = %q, want empty — the lease must be released on advance", got.WorkerID)
	}
}

func TestRequeuePreservesQueuedAt(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)
	queued := base.Add(-45 * time.Minute)
	mkJob(t, r, "j1", "u", "ocr", queued, time.Time{})

	if _, err := r.ClaimOneQueued(ctx, "ocr", "w1", base, base.Add(time.Minute), agingStep); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := r.RequeueJob(ctx, "j1", "boom", base); err != nil {
		t.Fatalf("RequeueJob: %v", err)
	}
	got, _ := r.JobByID(ctx, "j1")
	if !got.QueuedAt.Equal(queued) {
		t.Errorf("queued_at = %v, want %v — a retry must keep its accrued age", got.QueuedAt, queued)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
}

func TestExpireOverdueLeavesProcessing(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)

	// Both jobs share one deadline, in the future relative to the claim and in
	// the past relative to the sweep. Sequencing matters: the claim REFUSES an
	// already-expired job (that is TestClaimSkipsExpired), so the only way to
	// reach "processing AND past its deadline" is to lease it while it is still
	// live and then let the clock move.
	deadline := base.Add(time.Hour)
	sweepAt := base.Add(2 * time.Hour)

	mkJob(t, r, "j-queued", "u", "ocr", base.Add(-time.Hour), deadline)
	mkJob(t, r, "j-running", "u", "other", base.Add(-time.Hour), deadline)

	if _, err := r.ClaimOneQueued(ctx, "other", "w1", base, sweepAt.Add(time.Hour), agingStep); err != nil {
		t.Fatalf("claim: %v", err)
	}

	expired, err := r.ExpireOverdue(ctx, sweepAt)
	if err != nil {
		t.Fatalf("ExpireOverdue: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != "j-queued" {
		t.Fatalf("expired %d jobs (%v), want exactly j-queued", len(expired), ids(expired))
	}

	running, _ := r.JobByID(ctx, "j-running")
	if running.State != core.JobProcessing {
		t.Errorf("a PROCESSING job past its deadline became %q — work already started must run "+
			"to completion, because killing it does not recover the worker time already spent",
			running.State)
	}
}

func TestDeliverJobChargesOnceAndIsAtomic(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 100)
	mkJob(t, r, "j1", "u", "ocr", base, time.Time{})

	if _, err := r.ClaimOneQueued(ctx, "ocr", "w1", base, base.Add(time.Hour), agingStep); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := r.CompleteJob(ctx, "j1", 7, 7, base); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	if err := r.DeliverJob(ctx, "j1", "u", 7, base); err != nil {
		t.Fatalf("first DeliverJob: %v", err)
	}
	// The second delivery must charge nothing. Asserting the specific error AND
	// the ledger row count matters: a balance that merely "did not change" could
	// also mean the call failed early for an unrelated reason.
	if err := r.DeliverJob(ctx, "j1", "u", 7, base); err != core.ErrNotFound {
		t.Errorf("second DeliverJob = %v, want core.ErrNotFound", err)
	}

	u, _ := r.UserByID(ctx, "u")
	if u.Credits != 93 {
		t.Errorf("credits = %d, want 93 (100 - 7 charged exactly once)", u.Credits)
	}
	entries, err := r.Ledger(ctx, "u", 10)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger rows = %d, want 1 — a double charge would leave two", len(entries))
	}
	if entries[0].Delta != -7 || entries[0].JobID != "j1" {
		t.Errorf("ledger entry = %+v, want delta -7 for job j1", entries[0])
	}
}

func TestDeliverJobMayOverdraw(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 2) // only 2 credits
	mkJob(t, r, "j1", "u", "ocr", base, time.Time{})
	if _, err := r.ClaimOneQueued(ctx, "ocr", "w1", base, base.Add(time.Hour), agingStep); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := r.CompleteJob(ctx, "j1", 9, 9, base); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	if err := r.DeliverJob(ctx, "j1", "u", 9, base); err != nil {
		t.Fatalf("DeliverJob: %v", err)
	}
	u, _ := r.UserByID(ctx, "u")
	// The accepted overshoot, asserted rather than left to chance: the work was
	// already done and paid for in worker time, so it is delivered and the debt
	// is recorded honestly.
	if u.Credits != -7 {
		t.Errorf("credits = %d, want -7 (2 - 9): the overshoot is bounded by one job and must "+
			"be recorded rather than hidden by refusing delivery", u.Credits)
	}
}

func TestRateForLabelDefaultsToOne(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	rate, err := r.RateForLabel(ctx, "never-configured")
	if err != nil {
		t.Fatalf("RateForLabel: %v", err)
	}
	if rate != 1 {
		t.Errorf("rate for an unconfigured label = %d, want 1 — returning 0 would make every "+
			"new service silently free", rate)
	}
}

func TestSetRateRoundTrip(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	if err := r.SetRate(ctx, "ocr", 3, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}
	if err := r.SetRate(ctx, "ocr", 5, base); err != nil {
		t.Fatalf("SetRate upsert: %v", err)
	}
	rate, _ := r.RateForLabel(ctx, "ocr")
	if rate != 5 {
		t.Errorf("rate = %d, want 5 after upsert", rate)
	}
	all, err := r.ListRates(ctx)
	if err != nil {
		t.Fatalf("ListRates: %v", err)
	}
	if all["ocr"] != 5 {
		t.Errorf("ListRates[ocr] = %d, want 5", all["ocr"])
	}
}

func TestCountInFlightCountsTheRightStates(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 100)

	mkJob(t, r, "a", "u", "ocr", base, time.Time{})   // queued
	mkJob(t, r, "b", "u", "other", base, time.Time{}) // -> processing
	mkJob(t, r, "c", "u", "third", base, time.Time{}) // -> done
	if _, err := r.ClaimOneQueued(ctx, "other", "w", base, base.Add(time.Hour), agingStep); err != nil {
		t.Fatalf("claim b: %v", err)
	}
	if _, err := r.ClaimOneQueued(ctx, "third", "w", base, base.Add(time.Hour), agingStep); err != nil {
		t.Fatalf("claim c: %v", err)
	}
	if err := r.CompleteJob(ctx, "c", 1, 1, base); err != nil {
		t.Fatalf("complete c: %v", err)
	}

	n, err := r.CountInFlight(ctx, "u")
	if err != nil {
		t.Fatalf("CountInFlight: %v", err)
	}
	if n != 3 {
		t.Errorf("in-flight = %d, want 3 — a done-but-uncollected job still holds a buffer slot, "+
			"because its result is in memory and its blob is on disk", n)
	}

	if err := r.DeliverJob(ctx, "c", "u", 1, base); err != nil {
		t.Fatalf("deliver c: %v", err)
	}
	n, _ = r.CountInFlight(ctx, "u")
	if n != 2 {
		t.Errorf("in-flight after delivery = %d, want 2 — the limit is in-flight, not lifetime", n)
	}
}

func TestResetInFlightOnBoot(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 100)
	mkJob(t, r, "a", "u", "ocr", base, time.Time{})
	mkJob(t, r, "b", "u", "other", base, time.Time{})
	if _, err := r.ClaimOneQueued(ctx, "ocr", "w", base, base.Add(time.Hour), agingStep); err != nil {
		t.Fatalf("claim a: %v", err)
	}
	if _, err := r.ClaimOneQueued(ctx, "other", "w", base, base.Add(time.Hour), agingStep); err != nil {
		t.Fatalf("claim b: %v", err)
	}
	if err := r.CompleteJob(ctx, "b", 2, 2, base); err != nil {
		t.Fatalf("complete b: %v", err)
	}

	n, err := r.ResetInFlightOnBoot(ctx, base)
	if err != nil {
		t.Fatalf("ResetInFlightOnBoot: %v", err)
	}
	if n != 2 {
		t.Errorf("reset %d jobs, want 2 (one processing, one done)", n)
	}
	for _, id := range []string{"a", "b"} {
		j, _ := r.JobByID(ctx, id)
		if j.State != core.JobQueued {
			t.Errorf("job %s state = %q, want queued after boot recovery", id, j.State)
		}
	}
	// Nothing was charged: boot recovery costs repeated work, never money.
	entries, _ := r.Ledger(ctx, "u", 10)
	if len(entries) != 0 {
		t.Errorf("boot recovery wrote %d ledger entries, want 0", len(entries))
	}
}

func TestCreateUserDuplicateEmailIsConflict(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u1", 0, 0)
	dup := core.User{
		ID: "u2", Email: "u1@example.com", Role: core.RoleClient,
		BufferLimit: 4, Active: true, CreatedAt: base,
	}
	err := r.CreateUser(ctx, dup)
	if err != core.ErrConflict {
		t.Errorf("duplicate email = %v, want core.ErrConflict (matched by driver CODE, not "+
			"by message text, which breaks on a driver upgrade)", err)
	}
}

func TestJobByIDNotFound(t *testing.T) {
	r := newRepo(t)
	if _, err := r.JobByID(context.Background(), "nope"); err != core.ErrNotFound {
		t.Errorf("JobByID(missing) = %v, want core.ErrNotFound", err)
	}
}

func TestQueueDepthAndOldestByLabel(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 100)
	oldest := base.Add(-90 * time.Minute)
	mkJob(t, r, "o1", "u", "ocr", oldest, time.Time{})
	mkJob(t, r, "o2", "u", "ocr", base, time.Time{})
	mkJob(t, r, "s1", "u", "strip-html", base, time.Time{})

	depth, err := r.QueueDepthByLabel(ctx)
	if err != nil {
		t.Fatalf("QueueDepthByLabel: %v", err)
	}
	if depth["ocr"] != 2 || depth["strip-html"] != 1 {
		t.Errorf("depth = %v, want ocr:2 strip-html:1 — depths must not sum across labels", depth)
	}

	old, err := r.OldestQueuedByLabel(ctx)
	if err != nil {
		t.Fatalf("OldestQueuedByLabel: %v", err)
	}
	if !old["ocr"].Equal(oldest) {
		t.Errorf("oldest[ocr] = %v, want %v", old["ocr"], oldest)
	}
}

// TestRepoRoundTrip asserts that every write method's effect is visible through
// the matching read method.
//
// The behaviour-specific tests above each check one rule; this one checks the
// plumbing underneath them — that a value written is the value read back, with
// no field silently dropped by the column list or mangled by the unix-second
// conversions. A field added to core.Job and forgotten in scanJob or CreateJob
// fails here rather than in whichever handler first needs it.
func TestRepoRoundTrip(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	// users: create -> read
	u := core.User{
		ID: "u-rt", Email: "rt@example.com", Role: core.RoleClient,
		Credits: 42, BufferLimit: 7, Priority: 33, JobTTLSecs: 900,
		Active: true, CreatedAt: base,
	}
	if err := r.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	gotU, err := r.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if gotU.Email != u.Email || gotU.Credits != 42 || gotU.BufferLimit != 7 ||
		gotU.Priority != 33 || gotU.JobTTLSecs != 900 || !gotU.Active {
		t.Errorf("user round-trip = %+v, want %+v", gotU, u)
	}
	if !gotU.CreatedAt.Equal(base) {
		t.Errorf("created_at = %v, want %v", gotU.CreatedAt, base)
	}
	byEmail, err := r.UserByEmail(ctx, u.Email)
	if err != nil || byEmail.ID != u.ID {
		t.Errorf("UserByEmail = %+v, %v; want id %s", byEmail, err, u.ID)
	}

	// users: update -> read
	gotU.BufferLimit = 9
	gotU.Priority = -5
	gotU.JobTTLSecs = 0
	gotU.Active = false
	if err := r.UpdateUser(ctx, gotU); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	reread, _ := r.UserByID(ctx, u.ID)
	if reread.BufferLimit != 9 || reread.Priority != -5 || reread.JobTTLSecs != 0 || reread.Active {
		t.Errorf("user after update = %+v, want buffer 9 priority -5 ttl 0 inactive", reread)
	}
	if reread.Credits != 42 {
		t.Errorf("UpdateUser changed credits to %d — credits must move only through the "+
			"ledger transaction, never through a profile update", reread.Credits)
	}

	// tokens: create -> read by hash -> list -> revoke
	tok := core.Token{
		ID: "t-rt", UserID: u.ID, Role: core.RoleClient,
		Hash: "abc123", Label: "laptop", CreatedAt: base,
	}
	if err := r.CreateToken(ctx, tok); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	gotT, err := r.TokenByHash(ctx, "abc123")
	if err != nil {
		t.Fatalf("TokenByHash: %v", err)
	}
	if gotT.ID != tok.ID || gotT.UserID != u.ID || gotT.Role != core.RoleClient ||
		gotT.Label != "laptop" || gotT.Revoked {
		t.Errorf("token round-trip = %+v, want %+v", gotT, tok)
	}
	list, err := r.ListTokens(ctx, u.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTokens = %d rows, %v; want 1", len(list), err)
	}
	if err := r.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if after, _ := r.TokenByHash(ctx, "abc123"); !after.Revoked {
		t.Error("token is not marked revoked after RevokeToken")
	}

	// jobs: create -> read, including the fields that are JSON-encoded on the way
	// in and decoded on the way out.
	expires := base.Add(15 * time.Minute)
	j := core.Job{
		ID: "j-rt", UserID: u.ID, Filename: "doc.pdf", SizeByte: 4096,
		Label: "crawl", Pipeline: []string{"crawl", "strip-html", "ocr"}, Stage: 0,
		Params:  map[string]string{"url": "https://example.com", "max-depth": "2"},
		HasBlob: false, State: core.JobQueued,
		QueuedAt: base, ExpiresAt: expires, CreatedAt: base, UpdatedAt: base,
	}
	if err := r.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	gotJ, err := r.JobByID(ctx, j.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if gotJ.Filename != "doc.pdf" || gotJ.SizeByte != 4096 || gotJ.Label != "crawl" {
		t.Errorf("job scalars = %+v", gotJ)
	}
	if len(gotJ.Pipeline) != 3 || gotJ.Pipeline[2] != "ocr" {
		t.Errorf("pipeline round-trip = %v, want [crawl strip-html ocr]", gotJ.Pipeline)
	}
	if gotJ.Params["url"] != "https://example.com" || gotJ.Params["max-depth"] != "2" {
		t.Errorf("params round-trip = %v", gotJ.Params)
	}
	if gotJ.HasBlob {
		t.Error("has_blob round-tripped as true, want false — a params-only job has no blob " +
			"and that must survive the write/read cycle")
	}
	if !gotJ.ExpiresAt.Equal(expires) || !gotJ.QueuedAt.Equal(base) {
		t.Errorf("job times = queued %v expires %v, want %v / %v",
			gotJ.QueuedAt, gotJ.ExpiresAt, base, expires)
	}

	// A job with NO deadline must read back as a zero time, not as epoch 0.
	mkJob(t, r, "j-nodeadline", u.ID, "ocr", base, time.Time{})
	noDeadline, _ := r.JobByID(ctx, "j-nodeadline")
	if !noDeadline.ExpiresAt.IsZero() {
		t.Errorf("a job with no TTL read back expires_at = %v, want the zero time — NULL must "+
			"not become 1970, which would make every such job instantly overdue",
			noDeadline.ExpiresAt)
	}

	// jobs: list by user and state
	byState, err := r.JobsByUserAndState(ctx, u.ID, core.JobQueued)
	if err != nil {
		t.Fatalf("JobsByUserAndState: %v", err)
	}
	if len(byState) != 2 {
		t.Errorf("queued jobs for user = %d, want 2", len(byState))
	}
	recent, err := r.ListJobs(ctx, 10)
	if err != nil || len(recent) != 2 {
		t.Errorf("ListJobs = %d rows, %v; want 2", len(recent), err)
	}

	// credit grants outside a delivery also round-trip through the ledger.
	if err := r.AddCredits(ctx, u.ID, 100, "top-up", base); err != nil {
		t.Fatalf("AddCredits: %v", err)
	}
	afterGrant, _ := r.UserByID(ctx, u.ID)
	if afterGrant.Credits != 142 {
		t.Errorf("credits after grant = %d, want 142", afterGrant.Credits)
	}
	entries, _ := r.Ledger(ctx, u.ID, 10)
	if len(entries) != 1 || entries[0].Delta != 100 || entries[0].Reason != "top-up" {
		t.Errorf("ledger after grant = %+v, want one +100 top-up", entries)
	}
	if entries[0].JobID != "" {
		t.Errorf("grant ledger entry has job_id %q, want empty — a top-up belongs to no job",
			entries[0].JobID)
	}

	// census reads
	census, err := r.CountJobsByState(ctx)
	if err != nil {
		t.Fatalf("CountJobsByState: %v", err)
	}
	if census[core.JobQueued] != 2 {
		t.Errorf("census[queued] = %d, want 2", census[core.JobQueued])
	}
}

func ids(js []core.Job) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.ID
	}
	return out
}
