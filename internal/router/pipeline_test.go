package router_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// TestPipelineAdvancesWithoutNotifyingClient COUNTS the ready events.
//
// ⚠ The obvious assertion — "a ready was published after the last stage" —
// passes in both arrangements, including the broken one where every stage
// notifies. Counting is what distinguishes them, and the failure it catches is
// the worst one available: the client collects a half-processed intermediate,
// believing it is the finished result, and is charged for it.
func TestPipelineAdvancesWithoutNotifyingClient(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "crawl")
	h.worker(t, "strip-html")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	client, cancel := h.bus.Subscribe(bus.UserTopic("u"))
	defer cancel()

	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"crawl", "strip-html"},
		Params:   map[string]string{"url": "https://example.com"},
	})

	// Stage 0.
	if _, err := h.svc.Claim(ctx, "w1", "crawl", base); err != nil {
		t.Fatalf("claim crawl: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"<html>hi</html>"}, base); err != nil {
		t.Fatalf("complete crawl: %v", err)
	}

	if events := collect(client); len(events) != 0 {
		t.Errorf("completing stage 0 published %d client events (%+v), want 0 — a pipeline is "+
			"ONE job from the client's point of view", len(events), events)
	}

	mid, _ := h.repo.JobByID(ctx, j.ID)
	if mid.State != core.JobQueued {
		t.Errorf("after stage 0 state = %q, want queued", mid.State)
	}
	if mid.Label != "strip-html" || mid.Stage != 1 {
		t.Errorf("after stage 0 label/stage = %q/%d, want strip-html/1", mid.Label, mid.Stage)
	}

	// Stage 1.
	if _, err := h.svc.Claim(ctx, "w2", "strip-html", base); err != nil {
		t.Fatalf("claim strip-html: %v", err)
	}
	if err := h.svc.Complete(ctx, "w2", j.ID, []string{"hi"}, base); err != nil {
		t.Fatalf("complete strip-html: %v", err)
	}

	events := collect(client)
	readies := 0
	for _, e := range events {
		if e.Kind == bus.KindReady {
			readies++
		}
	}
	if readies != 1 {
		t.Errorf("the whole pipeline published %d ready events, want exactly 1 (after the "+
			"LAST stage only)", readies)
	}
}

func TestPipelineIntermediateBecomesNextInput(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "crawl")
	h.worker(t, "strip-html")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"crawl", "strip-html"},
		Params:   map[string]string{"url": "https://example.com"},
	})
	if j.HasBlob {
		t.Fatal("a crawler job started with a blob")
	}

	if _, err := h.svc.Claim(ctx, "w1", "crawl", base); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"<html>page</html>"}, base); err != nil {
		t.Fatalf("complete: %v", err)
	}

	f, err := h.blobs.Open(j.ID)
	if err != nil {
		t.Fatalf("stage 0's output is not readable as stage 1's input: %v", err)
	}
	defer f.Close()
	got, _ := os.ReadFile(f.Name())
	if string(got) != "<html>page</html>" {
		t.Errorf("intermediate blob = %q, want the single element verbatim", got)
	}

	// A job that began with no file now HAS one, and the next worker must be
	// told to pass -i. Inferring that from the filename would be guessing.
	mid, _ := h.repo.JobByID(ctx, j.ID)
	if !mid.HasBlob {
		t.Error("has_blob is still false after a stage produced output — the next stage's " +
			"worker would not be told to pass -i")
	}
}

func TestPipelineMultiElementIntermediateIsJoined(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "a")
	h.worker(t, "b")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"a", "b"}, Body: strings.NewReader("src"),
	})
	if _, err := h.svc.Claim(ctx, "w1", "a", base); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"one", "two"}, base); err != nil {
		t.Fatalf("complete: %v", err)
	}

	f, _ := h.blobs.Open(j.ID)
	defer f.Close()
	got, _ := os.ReadFile(f.Name())
	if string(got) != "one\ntwo" {
		t.Errorf("multi-element intermediate = %q, want %q — this encoding is a documented "+
			"choice and is lossy if an element itself contains newlines", got, "one\ntwo")
	}
}

func TestPipelinePreservesQueuedAtAcrossStages(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "a")
	h.worker(t, "b")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"a", "b"}, Body: strings.NewReader("src"),
	})
	if _, err := h.svc.Claim(ctx, "w1", "a", base); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Inside the 5-minute lease, but not at `base` — enough to show queued_at is
	// not rewritten to the completion time. Completing past the lease is
	// correctly refused, so this cannot use a larger offset.
	later := base.Add(time.Minute)
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"x"}, later); err != nil {
		t.Fatalf("complete: %v", err)
	}

	mid, _ := h.repo.JobByID(ctx, j.ID)
	if !mid.QueuedAt.Equal(base) {
		t.Errorf("queued_at after a stage advance = %v, want %v — a half-finished pipeline "+
			"must keep its accrued age, or every stage goes to the back of the queue and long "+
			"pipelines starve under load", mid.QueuedAt, base)
	}
}

// TestPipelineAccruesPerStageRate is the metering test for a multi-stage job.
func TestPipelineAccruesPerStageRate(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "crawl")
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	// crawl is free; ocr costs 2 per unit.
	if err := h.repo.SetRate(ctx, "crawl", 0, base); err != nil {
		t.Fatalf("SetRate crawl: %v", err)
	}
	if err := h.repo.SetRate(ctx, "ocr", 2, base); err != nil {
		t.Fatalf("SetRate ocr: %v", err)
	}

	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"crawl", "ocr"},
		Params:   map[string]string{"url": "https://example.com"},
	})

	if _, err := h.svc.Claim(ctx, "w1", "crawl", base); err != nil {
		t.Fatalf("claim crawl: %v", err)
	}
	// One crawled document, at rate 0 → accrues nothing.
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"<html/>"}, base); err != nil {
		t.Fatalf("complete crawl: %v", err)
	}
	if _, err := h.svc.Claim(ctx, "w2", "ocr", base); err != nil {
		t.Fatalf("claim ocr: %v", err)
	}
	// Three pages at rate 2 → 6.
	if err := h.svc.Complete(ctx, "w2", j.ID, []string{"p1", "p2", "p3"}, base); err != nil {
		t.Fatalf("complete ocr: %v", err)
	}

	done, _ := h.repo.JobByID(ctx, j.ID)
	if done.AccruedCredits != 6 {
		t.Errorf("accrued = %d, want 6 (crawl 1x0 + ocr 3x2)", done.AccruedCredits)
	}

	if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	u, _ := h.repo.UserByID(ctx, "u")
	if u.Credits != 94 {
		t.Errorf("credits = %d, want 94 — a pipeline is billed ONCE, for the sum across its "+
			"stages at each stage's own rate", u.Credits)
	}
	entries, _ := h.repo.Ledger(ctx, "u", 10)
	if len(entries) != 1 || entries[0].Delta != -6 {
		t.Errorf("ledger = %+v, want exactly one -6 entry", entries)
	}
}

func TestSingleStageUsesDefaultRateOfOne(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	mustRun(t, h, j.ID, []string{"a", "b", "c"})
	if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	u, _ := h.repo.UserByID(ctx, "u")
	if u.Credits != 97 {
		t.Errorf("credits = %d, want 97 — an unconfigured service costs 1 per unit, which is "+
			"the operator's original '1 page = 1 credit'", u.Credits)
	}
}
