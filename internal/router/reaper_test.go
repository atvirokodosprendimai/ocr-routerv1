package router_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

func TestReapRequeuesExpiredLease(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// The worker went silent; its lease lapses.
	after := base.Add(10 * time.Minute)
	rep, err := h.svc.Reap(ctx, after)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if rep.LeasesExpired != 1 {
		t.Errorf("LeasesExpired = %d, want 1", rep.LeasesExpired)
	}

	got, _ := h.repo.JobByID(ctx, j.ID)
	if got.State != core.JobQueued {
		t.Errorf("state after a lapsed lease = %q, want queued", got.State)
	}
	// ADR-0008: the lapsed lease costs a RECLAIM, not an attempt. This assertion
	// read `Attempts != 1` until 2026-09-17, which is the behaviour the record
	// removed — the worker vanished, its command never failed.
	if got.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", got.Attempts)
	}
	if got.Reclaims != 1 {
		t.Errorf("reclaims = %d, want 1", got.Reclaims)
	}
	if got.WorkerID != "" {
		t.Errorf("worker_id = %q, want empty", got.WorkerID)
	}
}

func TestReapExpiresOverdueQueued(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 60) // a 60-second deadline
	ctx := context.Background()

	client, cancel := h.bus.Subscribe(bus.UserTopic("u"))
	defer cancel()

	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	drain(client)

	after := base.Add(5 * time.Minute)
	rep, err := h.svc.Reap(ctx, after)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if rep.JobsExpired != 1 {
		t.Errorf("JobsExpired = %d, want 1", rep.JobsExpired)
	}

	got, _ := h.repo.JobByID(ctx, j.ID)
	if got.State != core.JobExpired {
		t.Errorf("state = %q, want expired", got.State)
	}
	if _, err := h.blobs.Size(j.ID); !errors.Is(err, core.ErrNotFound) {
		t.Error("an expired job kept its blob")
	}
	// Nothing is charged for work the customer never received.
	entries, _ := h.repo.Ledger(ctx, "u", 10)
	if len(entries) != 0 {
		t.Errorf("an expired job produced %d ledger entries, want 0", len(entries))
	}

	// The owner must be TOLD. An expiry that fails silently is indistinguishable
	// from a job still waiting, which is the worst of both.
	var reason string
	for _, e := range collect(client) {
		if e.Kind == bus.KindFailed && e.JobID == j.ID {
			reason = e.Reason
		}
	}
	if reason != "expired" {
		t.Errorf("client was not told the job expired (reason %q)", reason)
	}
}

// TestReapLeavesProcessingPastDeadline is the counterpart that stops expiry
// being widened into something harmful.
func TestReapLeavesProcessingPastDeadline(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 60)
	ctx := context.Background()

	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Past the deadline but still INSIDE the lease: the worker is running.
	after := base.Add(2 * time.Minute)
	if _, err := h.svc.Reap(ctx, after); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	got, _ := h.repo.JobByID(ctx, j.ID)
	if got.State != core.JobProcessing {
		t.Errorf("a PROCESSING job past its deadline became %q — work already started must run "+
			"to completion, because discarding it does not recover the worker time spent",
			got.State)
	}
}

func TestReapSweepsResultsAndRequeues(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	mustRun(t, h, j.ID, []string{"a"})

	// Move the result store's clock past the TTL. The job is `done` with a
	// result nobody collected.
	h.results.SetClock(func() time.Time { return base.Add(2 * time.Hour) })

	rep, err := h.svc.Reap(ctx, base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if rep.ResultsSwept != 1 {
		t.Errorf("ResultsSwept = %d, want 1", rep.ResultsSwept)
	}

	got, _ := h.repo.JobByID(ctx, j.ID)
	if got.State != core.JobQueued {
		t.Errorf("state after the result was swept = %q, want queued — the blob is still on "+
			"disk, so the work is redone rather than the job being stranded in done forever",
			got.State)
	}
	entries, _ := h.repo.Ledger(ctx, "u", 10)
	if len(entries) != 0 {
		t.Errorf("sweeping charged %d ledger entries, want 0", len(entries))
	}
}

// TestReapAbandonsAfterMaxReclaims: the bound moved from attempts to reclaims in
// ADR-0008, but it is still a bound — a job that keeps losing its worker must
// still reach a terminal state.
func TestReapAbandonsAfterMaxReclaims(t *testing.T) {
	h := newHarnessWith(t, router.Config{
		Lease: 5 * time.Minute, MaxAttempts: 3, MaxReclaims: 3,
		AgingStep: time.Minute, LabelGrace: 5 * time.Minute,
		ResultTTL: time.Hour, DefaultLabel: "ocr",
	})
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})

	// Three lapsed leases, one per reclaim.
	for i := 1; i <= 3; i++ {
		at := base.Add(time.Duration(i) * 10 * time.Minute)
		if _, err := h.svc.Claim(ctx, "w1", "ocr", at); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if _, err := h.svc.Reap(ctx, at.Add(10*time.Minute)); err != nil {
			t.Fatalf("reap %d: %v", i, err)
		}
	}
	got, _ := h.repo.JobByID(ctx, j.ID)
	if got.State != core.JobDead {
		t.Errorf("state after three lapsed leases = %q, want dead", got.State)
	}
}

func TestRecoverOnBootRequeues(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	processing := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	done := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("y")})

	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if _, err := h.svc.Claim(ctx, "w2", "ocr", base); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if err := h.svc.Complete(ctx, "w2", done.ID, []string{"a"}, base); err != nil {
		t.Fatalf("complete: %v", err)
	}

	n, err := h.svc.RecoverOnBoot(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("RecoverOnBoot: %v", err)
	}
	if n != 2 {
		t.Errorf("recovered %d jobs, want 2 (one processing, one done)", n)
	}
	for _, id := range []string{processing.ID, done.ID} {
		got, _ := h.repo.JobByID(ctx, id)
		if got.State != core.JobQueued {
			t.Errorf("job %s state = %q, want queued after boot recovery", id, got.State)
		}
		// The blob is the durable copy and must survive, or the work cannot be
		// redone and the document is simply lost.
		if _, err := h.blobs.Size(id); err != nil {
			t.Errorf("job %s lost its blob during boot recovery: %v", id, err)
		}
	}
	entries, _ := h.repo.Ledger(ctx, "u", 10)
	if len(entries) != 0 {
		t.Errorf("boot recovery wrote %d ledger entries, want 0 — a restart costs repeated "+
			"work, never money", len(entries))
	}
}

func TestRecoverOnBootWakesWorkers(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	workers, cancel := h.bus.Subscribe(bus.WorkerTopic("ocr"))
	defer cancel()

	if _, err := h.svc.RecoverOnBoot(ctx, base.Add(time.Hour)); err != nil {
		t.Fatalf("RecoverOnBoot: %v", err)
	}
	if len(collect(workers)) == 0 {
		t.Error("boot recovery requeued work but woke nobody — the recovered jobs would sit " +
			"until the next upload or reconnect happened to nudge them")
	}
}

func TestReapOnEmptySystem(t *testing.T) {
	h := newHarness(t)
	rep, err := h.svc.Reap(context.Background(), base)
	if err != nil {
		t.Fatalf("Reap on an empty system: %v", err)
	}
	if rep.LeasesExpired+rep.JobsExpired+rep.ResultsSwept != 0 {
		t.Errorf("Reap on an empty system reported %+v", rep)
	}
}

func drain(ch <-chan bus.Event) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
