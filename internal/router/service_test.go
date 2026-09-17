package router_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

type harness struct {
	svc     *router.Service
	repo    *store.Repo
	bus     *bus.Bus
	blobs   *blob.Store
	results *results.Store
}

// newHarness builds a service on the configuration almost every test wants.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, router.Config{
		Lease:        5 * time.Minute,
		MaxAttempts:  3,
		MaxReclaims:  10, // the shipped default; see cmd/router/main.go
		AgingStep:    time.Minute,
		LabelGrace:   5 * time.Minute,
		ResultTTL:    time.Hour,
		DefaultLabel: "ocr",
	})
}

// newHarnessWith is the same harness with the config named, for the tests whose
// subject IS a bound — a budget you cannot vary cannot be shown to be a budget.
func newHarnessWith(t *testing.T, cfg router.Config) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blob.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	repo := store.NewRepo(db)
	res := results.New(time.Hour)
	// Pin the result store to the SAME injected clock every other assertion uses.
	// Without this, Put stamps expiresAt from the real time.Now while the tests
	// reason in `base` — so a test that moves the clock to base+2h is comparing
	// against an expiry computed from today's date. That passed only while `base`
	// happened to be within an hour of the real clock, and went red on its own the
	// day after `base` (2026-09-15), with no code change.
	res.SetClock(func() time.Time { return base })
	b := bus.New()
	svc := router.New(repo, blobs, res, b, cfg)
	return &harness{svc: svc, repo: repo, bus: b, blobs: blobs, results: res}
}

// worker subscribes to a label so it counts as live, which is what makes that
// label valid for an upload.
func (h *harness) worker(t *testing.T, label string) func() {
	t.Helper()
	_, cancel := h.bus.Subscribe(bus.WorkerTopic(label))
	t.Cleanup(cancel)
	return cancel
}

func (h *harness) user(t *testing.T, id string, credits, bufferLimit, ttlSecs int) core.User {
	t.Helper()
	u := core.User{
		ID: id, Email: id + "@example.com", Role: core.RoleClient,
		Credits: credits, BufferLimit: bufferLimit, JobTTLSecs: ttlSecs,
		Active: true, CreatedAt: base,
	}
	if err := h.repo.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// collect drains a subscriber into a slice, so a test can COUNT events rather
// than merely notice that one arrived.
func collect(ch <-chan bus.Event) []bus.Event {
	var out []bus.Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func upload(t *testing.T, h *harness, userID string, in router.UploadInput) core.Job {
	t.Helper()
	j, err := h.svc.Upload(context.Background(), userID, in, base)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	return j
}

func TestUploadRefusesWithoutCredits(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 0, 4, 0)

	_, err := h.svc.Upload(context.Background(), "u", router.UploadInput{
		Filename: "a.pdf", Body: strings.NewReader("x"),
	}, base)
	if !errors.Is(err, core.ErrNoCredits) {
		t.Fatalf("Upload with 0 credits = %v, want core.ErrNoCredits", err)
	}
	jobs, _ := h.repo.ListJobs(context.Background(), 10)
	if len(jobs) != 0 {
		t.Errorf("a refused upload created %d job rows, want 0", len(jobs))
	}
}

func TestUploadRefusesAtBufferLimit(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 2, 0)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		upload(t, h, "u", router.UploadInput{Filename: "a.pdf", Body: strings.NewReader("x")})
	}
	_, err := h.svc.Upload(ctx, "u", router.UploadInput{
		Filename: "c.pdf", Body: strings.NewReader("x"),
	}, base)
	if !errors.Is(err, core.ErrBufferFull) {
		t.Errorf("the third upload at a limit of 2 = %v, want core.ErrBufferFull", err)
	}
}

func TestUploadBufferLimitFreesOnDelivery(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 1, 0)
	ctx := context.Background()

	j := upload(t, h, "u", router.UploadInput{Filename: "a.pdf", Body: strings.NewReader("x")})
	if _, err := h.svc.Upload(ctx, "u", router.UploadInput{Body: strings.NewReader("x")}, base); !errors.Is(err, core.ErrBufferFull) {
		t.Fatalf("second upload at a limit of 1 = %v, want core.ErrBufferFull", err)
	}

	claimed, err := h.svc.Claim(ctx, "w1", "ocr", base)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", claimed.ID, []string{"page"}, base); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Still in flight: a done-but-uncollected job holds its slot.
	if _, err := h.svc.Upload(ctx, "u", router.UploadInput{Body: strings.NewReader("x")}, base); !errors.Is(err, core.ErrBufferFull) {
		t.Errorf("upload while a result is uncollected = %v, want core.ErrBufferFull — the "+
			"result occupies memory and the blob occupies disk until it is taken", err)
	}
	if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := h.svc.Upload(ctx, "u", router.UploadInput{Body: strings.NewReader("x")}, base); err != nil {
		t.Errorf("upload after delivery = %v, want success — the limit is in-flight, not "+
			"lifetime", err)
	}
}

func TestUploadRejectsUnknownLabel(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)

	_, err := h.svc.Upload(context.Background(), "u", router.UploadInput{
		Pipeline: []string{"strip-htm"}, Body: strings.NewReader("x"),
	}, base)
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown label = %v, want core.ErrNotFound", err)
	}
	// The message must name what IS available, or a typo costs the customer a
	// silent wait until the deadline instead of an immediate, actionable answer.
	if !strings.Contains(err.Error(), "ocr") {
		t.Errorf("the refusal %q does not name the available labels", err)
	}
}

func TestUploadRejectsBadParamKey(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)

	_, err := h.svc.Upload(context.Background(), "u", router.UploadInput{
		Body:   strings.NewReader("x"),
		Params: map[string]string{"--inject": "x"},
	}, base)
	if !errors.Is(err, core.ErrInvalidParam) {
		t.Fatalf("bad param key = %v, want core.ErrInvalidParam", err)
	}
	jobs, _ := h.repo.ListJobs(context.Background(), 10)
	if len(jobs) != 0 {
		t.Errorf("a refused upload created %d rows, want 0 — validation must happen before "+
			"anything is written", len(jobs))
	}
}

func TestUploadWithoutBlobIsValid(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "crawl")
	h.user(t, "u", 100, 4, 0)

	// The crawler shape: parameters, no body at all.
	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"crawl"},
		Params:   map[string]string{"url": "https://example.com"},
	})
	if j.HasBlob {
		t.Error("a params-only job reported HasBlob — it has no file, and that is normal")
	}
	got, err := h.repo.JobByID(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.HasBlob {
		t.Error("has_blob persisted as true for a params-only job")
	}
	if got.Params["url"] != "https://example.com" {
		t.Errorf("params = %v", got.Params)
	}
	if _, err := h.svc.Claim(context.Background(), "w1", "crawl", base); err != nil {
		t.Errorf("a params-only job is not claimable: %v", err)
	}
}

func TestUploadSetsDeadlineFromUserTTL(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "ttl", 100, 4, 900)
	h.user(t, "nottl", 100, 4, 0)

	withTTL := upload(t, h, "ttl", router.UploadInput{Body: strings.NewReader("x")})
	if !withTTL.ExpiresAt.Equal(base.Add(900 * time.Second)) {
		t.Errorf("expires_at = %v, want %v", withTTL.ExpiresAt, base.Add(900*time.Second))
	}

	without := upload(t, h, "nottl", router.UploadInput{Body: strings.NewReader("x")})
	if !without.ExpiresAt.IsZero() {
		t.Errorf("expires_at = %v for a customer with TTL 0, want the zero time", without.ExpiresAt)
	}
}

func TestUploadPublishesWorkAfterPersisting(t *testing.T) {
	h := newHarness(t)
	ch, cancel := h.bus.Subscribe(bus.WorkerTopic("ocr"))
	defer cancel()
	h.user(t, "u", 100, 4, 0)

	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	events := collect(ch)
	if len(events) == 0 {
		t.Fatal("no work event published after a successful upload")
	}
	// The row must exist by the time anyone is told about it.
	if _, err := h.repo.JobByID(context.Background(), j.ID); err != nil {
		t.Errorf("the job was published but is not readable: %v", err)
	}
}

func TestClaimLeasesOneQueuedJob(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	got, err := h.svc.Claim(context.Background(), "w1", "ocr", base)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got.State != core.JobProcessing || got.WorkerID != "w1" {
		t.Errorf("claimed job = state %q worker %q, want processing/w1", got.State, got.WorkerID)
	}
	if !got.LeaseExpiresAt.Equal(base.Add(5 * time.Minute)) {
		t.Errorf("lease = %v, want %v", got.LeaseExpiresAt, base.Add(5*time.Minute))
	}
}

func TestClaimOnEmptyQueue(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	if _, err := h.svc.Claim(context.Background(), "w1", "ocr", base); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Claim on an empty queue = %v, want core.ErrNotFound", err)
	}
}

func TestCompleteRequiresLease(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	ctx := context.Background()

	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.Complete(ctx, "w2", j.ID, []string{"stolen"}, base); !errors.Is(err, core.ErrConflict) {
		t.Errorf("a worker without the lease completing = %v, want core.ErrConflict", err)
	}
	if _, ok := h.results.Peek(j.ID); ok {
		t.Error("the result was stored despite the lease check failing")
	}
}

func TestCompleteRejectsExpiredLease(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	ctx := context.Background()

	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Past the lease: the reaper may already have requeued this job and another
	// worker may be running it, so a late result must not be accepted.
	late := base.Add(10 * time.Minute)
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"late"}, late); !errors.Is(err, core.ErrConflict) {
		t.Errorf("completing past the lease = %v, want core.ErrConflict", err)
	}
}

func TestDeliverChargesOncePerUnit(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"1", "2", "3", "4", "5", "6", "7"}, base); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	res, err := h.svc.Deliver(ctx, "u", j.ID, base)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(res.Units) != 7 {
		t.Errorf("delivered %d units, want 7", len(res.Units))
	}
	u, _ := h.repo.UserByID(ctx, "u")
	if u.Credits != 93 {
		t.Errorf("credits = %d, want 93", u.Credits)
	}
}

func TestDeliverTwiceChargesOnce(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	mustRun(t, h, j.ID, []string{"a", "b"})

	if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err != nil {
		t.Fatalf("first Deliver: %v", err)
	}
	if _, err := h.svc.Deliver(ctx, "u", j.ID, base); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("second Deliver = %v, want core.ErrNotFound", err)
	}

	// ⚠ Assert the LEDGER, not just the balance. A balance that "did not change"
	// could also mean the second call failed early for an unrelated reason; the
	// row count says the charge itself happened exactly once.
	entries, err := h.repo.Ledger(ctx, "u", 10)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(entries))
	}
	if entries[0].Delta != -2 {
		t.Errorf("ledger delta = %d, want -2", entries[0].Delta)
	}
	u, _ := h.repo.UserByID(ctx, "u")
	if u.Credits != 98 {
		t.Errorf("credits = %d, want 98", u.Credits)
	}
}

// TestDeliverIsAtomicUnderRace proves a job cannot be delivered twice even when
// two clients race for it.
//
// The atomicity comes from results.Take removing and returning inside one
// critical section: only one caller gets the result, so only one reaches the
// charging transaction. Asserting the LEDGER ROW COUNT rather than the balance
// is what makes this test able to fail for the right reason — a balance that
// moved once could also mean the second call failed early for some unrelated
// reason.
func TestDeliverIsAtomicUnderRace(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	mustRun(t, h, j.ID, []string{"a", "b", "c"})

	const n = 16
	var (
		mu   sync.Mutex
		wins int
		wg   sync.WaitGroup
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Errorf("%d concurrent deliveries produced %d successes, want exactly 1", n, wins)
	}
	entries, err := h.repo.Ledger(ctx, "u", 10)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("ledger rows = %d, want exactly 1 — more than one means the same job was "+
			"charged twice, and each row would look legitimate on its own", len(entries))
	}
	u, _ := h.repo.UserByID(ctx, "u")
	if u.Credits != 97 {
		t.Errorf("credits = %d, want 97", u.Credits)
	}
}

func TestDeliverRefusesOtherUsersJob(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "a", 100, 4, 0)
	h.user(t, "b", 100, 4, 0)
	ctx := context.Background()

	j := upload(t, h, "a", router.UploadInput{Body: strings.NewReader("x")})
	mustRun(t, h, j.ID, []string{"secret"})

	if _, err := h.svc.Deliver(ctx, "b", j.ID, base); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("customer B delivering A's job = %v, want core.ErrNotFound (not Forbidden — "+
			"B must not learn the id exists)", err)
	}
	a, _ := h.repo.UserByID(ctx, "a")
	if a.Credits != 100 {
		t.Errorf("owner was charged %d credits by another customer's attempt", 100-a.Credits)
	}
	// And A can still collect it.
	if _, err := h.svc.Deliver(ctx, "a", j.ID, base); err != nil {
		t.Errorf("the owner can no longer collect after B's attempt: %v", err)
	}
}

func TestDeliverDeletesBlobAfterCommit(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("source bytes")})

	if _, err := h.blobs.Size(j.ID); err != nil {
		t.Fatalf("the blob should exist before delivery: %v", err)
	}
	mustRun(t, h, j.ID, []string{"a"})
	if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := h.blobs.Size(j.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("the blob survived delivery: %v", err)
	}
}

func TestDeliverMayOverdraw(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 2, 4, 0) // two credits
	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	mustRun(t, h, j.ID, []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"})

	if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	u, _ := h.repo.UserByID(ctx, "u")
	// The accepted overshoot, asserted rather than left to chance: the work is
	// already done and paid for in worker time, so it is delivered and the debt
	// recorded honestly rather than hidden by refusing.
	if u.Credits != -7 {
		t.Errorf("credits = %d, want -7 — the overshoot is bounded by one job and must be "+
			"recorded, not hidden", u.Credits)
	}
}

func TestFailRequeuesUntilMaxAttempts(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	// MaxAttempts is 3: two failures requeue, the third kills it.
	for i := 1; i <= 2; i++ {
		if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if err := h.svc.Fail(ctx, "w1", j.ID, "boom", nil, base); err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
		got, _ := h.repo.JobByID(ctx, j.ID)
		if got.State != core.JobQueued {
			t.Fatalf("after failure %d state = %q, want queued", i, got.State)
		}
	}
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("claim 3: %v", err)
	}
	if err := h.svc.Fail(ctx, "w1", j.ID, "boom", nil, base); err != nil {
		t.Fatalf("fail 3: %v", err)
	}
	got, _ := h.repo.JobByID(ctx, j.ID)
	if got.State != core.JobDead {
		t.Errorf("after the third failure state = %q, want dead", got.State)
	}
	if _, err := h.blobs.Size(j.ID); !errors.Is(err, core.ErrNotFound) {
		t.Error("a dead job kept its blob")
	}
	// Nothing is charged for work that never reached the customer.
	entries, _ := h.repo.Ledger(ctx, "u", 10)
	if len(entries) != 0 {
		t.Errorf("a dead job produced %d ledger entries, want 0", len(entries))
	}
}

func TestFailPreservesQueuedAt(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Inside the 5-minute lease, but at a different instant from the upload —
	// which is what makes "queued_at was not rewritten to now" meaningful.
	// Failing PAST the lease is correctly refused (TestCompleteRejectsExpiredLease),
	// so this must stay within it.
	later := base.Add(time.Minute)
	if err := h.svc.Fail(ctx, "w1", j.ID, "boom", nil, later); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	got, _ := h.repo.JobByID(ctx, j.ID)
	if !got.QueuedAt.Equal(base) {
		t.Errorf("queued_at = %v, want %v — a retry must keep the age it has accrued, or "+
			"repeatedly-failing jobs sink to the back of the queue forever", got.QueuedAt, base)
	}
}

func TestBacklogListsCollectableResults(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	h.user(t, "other", 100, 4, 0)
	ctx := context.Background()

	j1 := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	mustRun(t, h, j1.ID, []string{"a", "b"})
	j2 := upload(t, h, "other", router.UploadInput{Body: strings.NewReader("x")})
	mustRun(t, h, j2.ID, []string{"c"})

	items, err := h.svc.Backlog(ctx, "u")
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if len(items) != 1 || items[0].JobID != j1.ID || items[0].Units != 2 {
		t.Fatalf("backlog = %+v, want one entry for %s with 2 units", items, j1.ID)
	}

	// A done row whose result was swept must NOT be listed: offering it would
	// promise something the next fetch refuses.
	h.results.Drop(j1.ID)
	items, _ = h.svc.Backlog(ctx, "u")
	if len(items) != 0 {
		t.Errorf("backlog still lists %+v after its result was swept", items)
	}
}

func TestAvailableLabelsUsesGraceWindow(t *testing.T) {
	h := newHarness(t)
	cancel := h.worker(t, "ocr")

	if got := h.svc.AvailableLabels(base); len(got) != 1 || got[0] != "ocr" {
		t.Fatalf("AvailableLabels with a live worker = %v, want [ocr]", got)
	}

	cancel() // the worker disconnects, as in a rolling restart

	// Inside the grace window the label is still valid, so a restart does not
	// turn into a burst of rejected uploads.
	if got := h.svc.AvailableLabels(base.Add(time.Minute)); len(got) != 1 {
		t.Errorf("AvailableLabels 1 minute after disconnect = %v, want [ocr] — the grace "+
			"window exists so a rolling restart does not reject uploads", got)
	}
	// Past it, the label is genuinely gone.
	if got := h.svc.AvailableLabels(base.Add(10 * time.Minute)); len(got) != 0 {
		t.Errorf("AvailableLabels 10 minutes after disconnect = %v, want empty", got)
	}
}

// mustRun claims a job and completes its final stage.
func mustRun(t *testing.T, h *harness, jobID string, out []string) {
	t.Helper()
	ctx := context.Background()
	j, err := h.repo.JobByID(ctx, jobID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if _, err := h.svc.Claim(ctx, "w1", j.Label, base); err != nil {
		t.Fatalf("Claim(%s): %v", j.Label, err)
	}
	if err := h.svc.Complete(ctx, "w1", jobID, out, base); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}
