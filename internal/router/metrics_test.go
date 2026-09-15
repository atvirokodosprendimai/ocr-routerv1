package router_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/monitor"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// recorder is a Counter that remembers what it was told, so a test can assert
// the SERIES rather than merely that something was incremented.
//
// It is concurrent-safe because Reap and the delivery path both increment, and
// the race detector is part of this task's acceptance.
type recorder struct {
	mu  sync.Mutex
	got map[string]int64
}

func newRecorder() *recorder { return &recorder{got: map[string]int64{}} }

func (r *recorder) Inc(name string, labels map[string]string) { r.Add(name, labels, 1) }

func (r *recorder) Add(name string, labels map[string]string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got[seriesKey(name, labels)] += n
}

func (r *recorder) value(name string, labels map[string]string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got[seriesKey(name, labels)]
}

func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// TestCountersIncrementOnTransitions is the proof that S8's call sites exist.
//
// The registry can be perfect and wired into the binary while router.Service
// increments nothing, and a counter nobody increments is always zero — which
// reads exactly like a healthy quiet system. This drives the real transitions.
func TestCountersIncrementOnTransitions(t *testing.T) {
	h := newHarness(t)
	rec := newRecorder()
	h.svc.SetCounter(rec)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)

	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})

	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"p1", "p2"}, base); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if got := rec.value(monitor.MetricJobsTotal, map[string]string{"state": "delivered"}); got != 1 {
		t.Errorf(`ocrr_jobs_total{state="delivered"} = %d after one delivery, want 1`, got)
	}
}

// TestCreditsDebitedCounterMatchesLedger — a counter that disagrees with the
// books is worse than no counter, because it is used to reconcile them.
func TestCreditsDebitedCounterMatchesLedger(t *testing.T) {
	h := newHarness(t)
	rec := newRecorder()
	h.svc.SetCounter(rec)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)

	ctx := context.Background()
	pagesPerJob := []int{3, 1, 5}
	for _, pages := range pagesPerJob {
		j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})
		if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		units := make([]string, pages)
		for i := range units {
			units[i] = "page"
		}
		if err := h.svc.Complete(ctx, "w1", j.ID, units, base); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
	}

	entries, err := h.repo.Ledger(ctx, "u", 100)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	var debited int64
	for _, e := range entries {
		if e.Delta < 0 {
			debited += int64(-e.Delta)
		}
	}
	if debited != 9 {
		t.Fatalf("the ledger recorded %d debited credits, want 9 — the fixture itself is wrong",
			debited)
	}

	if got := rec.value(monitor.MetricCreditsDebited, nil); got != debited {
		t.Errorf("ocrr_credits_debited_total = %d but the ledger says %d. A metric used to "+
			"reconcile the books must agree with them", got, debited)
	}
}

// TestReaperActionsCounted makes a reaper that silently stopped visible.
//
// A stalled reaper produces no errors and no log line: jobs simply stop coming
// back. The counter is the only thing that distinguishes "nothing expired" from
// "nothing is being swept".
func TestReaperActionsCounted(t *testing.T) {
	h := newHarness(t)
	rec := newRecorder()
	h.svc.SetCounter(rec)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)

	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Past the lease: the reaper must take the job back.
	rep, err := h.svc.Reap(ctx, base.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if rep.LeasesExpired == 0 {
		t.Fatalf("the fixture did not expire a lease, so this test proves nothing about the "+
			"counter: %+v", rep)
	}

	var total int64
	rec.mu.Lock()
	for k, v := range rec.got {
		if strings.HasPrefix(k, monitor.MetricReaperActions) {
			total += v
		}
	}
	rec.mu.Unlock()

	if total == 0 {
		t.Errorf("Reap expired %d lease(s) and moved no %s series — a reaper that stops "+
			"sweeping is invisible without this", rep.LeasesExpired, monitor.MetricReaperActions)
	}
	_ = j
}

// TestServiceWithoutACounterStillWorks pins the nop default.
//
// Every test written before T11 constructs a Service with no counter. If the
// absent counter panicked, this package would be a minefield; if it silently
// dropped a REQUIRED dependency instead, the binary could ship unmetered. The
// default is deliberate, and this is where that is written down.
func TestServiceWithoutACounterStillWorks(t *testing.T) {
	h := newHarness(t) // no SetCounter call
	h.worker(t, "ocr")
	h.user(t, "u", 10, 4, 0)

	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim with no counter attached: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"p"}, base); err != nil {
		t.Fatalf("Complete with no counter attached: %v", err)
	}
}

// TestSetCounterNilIsSafe — a caller passing nil gets the nop, not a panic on
// the first transition, which would be a crash in production and nowhere else.
func TestSetCounterNilIsSafe(t *testing.T) {
	h := newHarness(t)
	h.svc.SetCounter(nil)
	h.worker(t, "ocr")
	h.user(t, "u", 10, 4, 0)

	upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})
	if _, err := h.svc.Claim(context.Background(), "w1", "ocr", base); err != nil {
		t.Fatalf("Claim after SetCounter(nil): %v", err)
	}
}
