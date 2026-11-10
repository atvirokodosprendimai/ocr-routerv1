package router_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
	"github.com/atvirokodosprendimai/ocr-routerv1/internal/router"
)

// TestRawStageBridgesWithoutJoinUnits is why raw composes with pipelines at all.
//
// The bridge between stages was ALREADY a blob, so a raw stage needs no new
// mechanism — the output blob becomes the input blob. What it removes is
// joinUnits, whose newline joining is documented as lossy and would be actively
// wrong for binary.
func TestRawStageBridgesWithoutJoinUnits(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.worker(t, "compress")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	if err := h.repo.SetRate(ctx, "convert", 1, true, base); err != nil {
		t.Fatalf("SetRate convert: %v", err)
	}
	if err := h.repo.SetRate(ctx, "compress", 1, true, base); err != nil {
		t.Fatalf("SetRate compress: %v", err)
	}

	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"convert", "compress"},
		Body:     strings.NewReader("original"),
		Raw:      true,
	})
	if _, err := h.svc.Claim(ctx, "w1", "convert", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Bytes with an embedded NEWLINE and a non-UTF-8 lead: joinUnits would mangle
	// the first, and JSON the second.
	const stage1 = "\x89PNG\nsecond line\n\x1a"
	if err := h.svc.CompleteRaw(ctx, "w1", j.ID, strings.NewReader(stage1), base); err != nil {
		t.Fatalf("CompleteRaw: %v", err)
	}

	got, err := h.repo.JobByID(ctx, j.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.State != core.JobQueued || got.Label != "compress" {
		t.Fatalf("after stage 1 the job is %q under %q, want queued under compress",
			got.State, got.Label)
	}
	if !got.HasBlob {
		t.Error("the advanced job reports no blob — the next stage has nothing at -i")
	}

	// The next stage's INPUT must be stage 1's output, byte for byte.
	f, err := h.blobs.Open(j.ID)
	if err != nil {
		t.Fatalf("Open input blob: %v", err)
	}
	defer f.Close()
	in, _ := io.ReadAll(f)
	if string(in) != stage1 {
		t.Errorf("stage 2's input is % x (%d bytes), want % x (%d bytes) — the bridge must move "+
			"the bytes, not render them", in, len(in), stage1, len(stage1))
	}
}

// TestRawStageModeMismatchFailsTheJob covers the composition an operator can get
// wrong: a raw stage feeding a units one.
//
// Failing it with a named reason is the point. Left queued, the job would sit
// under a label whose every worker the mode check refuses, with nothing saying
// why — the ADR's "worker refused forever" risk, reached from the other side.
func TestRawStageModeMismatchFailsTheJob(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	if err := h.repo.SetRate(ctx, "convert", 1, true, base); err != nil {
		t.Fatalf("SetRate convert: %v", err)
	}
	if err := h.repo.SetRate(ctx, "ocr", 3, false, base); err != nil {
		t.Fatalf("SetRate ocr: %v", err)
	}

	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"convert", "ocr"},
		Body:     strings.NewReader("original"),
		Raw:      true,
	})
	if _, err := h.svc.Claim(ctx, "w1", "convert", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.CompleteRaw(ctx, "w1", j.ID, strings.NewReader("\x89PNG"), base); err != nil {
		t.Fatalf("CompleteRaw: %v", err)
	}

	got, err := h.repo.JobByID(ctx, j.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.State == core.JobQueued && got.Label == "ocr" {
		t.Fatal("the job advanced into a UNITS stage carrying raw bytes — it will queue under a " +
			"label whose every worker is refused, with nothing saying why")
	}
	if !strings.Contains(got.LastError, "ocr") {
		t.Errorf("the failure reason %q does not name the offending stage", got.LastError)
	}
}

// TestRawStageModeMismatchIsNotRetried is F12 (REPORT 2026-11-10): the
// mismatch is a configuration error no rerun can fix, so the job dies at once
// instead of rerunning the same raw stage up to MaxAttempts times.
func TestRawStageModeMismatchIsNotRetried(t *testing.T) {
	h := newHarness(t) // MaxAttempts 3
	h.worker(t, "convert")
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	if err := h.repo.SetRate(ctx, "convert", 1, true, base); err != nil {
		t.Fatalf("SetRate convert: %v", err)
	}
	if err := h.repo.SetRate(ctx, "ocr", 3, false, base); err != nil {
		t.Fatalf("SetRate ocr: %v", err)
	}
	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"convert", "ocr"}, Body: strings.NewReader("original"), Raw: true,
	})
	if _, err := h.svc.Claim(ctx, "w1", "convert", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	_ = h.svc.CompleteRaw(ctx, "w1", j.ID, strings.NewReader("\x89PNG"), base)

	got, _ := h.repo.JobByID(ctx, j.ID)
	if got.State != core.JobDead {
		t.Errorf("state after a mode mismatch = %q, want dead — a rerun cannot fix a configuration", got.State)
	}
}

// TestUnitsStageIntoRawStageFailsTheJob is F9 (REPORT 2026-11-10), the mirror
// of the raw-to-units check: a units stage feeding a raw one used to advance,
// and every raw worker of the next label then had its result refused, so the
// job burned its reclaims and died as "abandoned" with no word on why.
func TestUnitsStageIntoRawStageFailsTheJob(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	if err := h.repo.SetRate(ctx, "ocr", 1, false, base); err != nil {
		t.Fatalf("SetRate ocr: %v", err)
	}
	if err := h.repo.SetRate(ctx, "convert", 1, true, base); err != nil {
		t.Fatalf("SetRate convert: %v", err)
	}
	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"ocr", "convert"}, Body: strings.NewReader("scan"),
	})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	_ = h.svc.Complete(ctx, "w1", j.ID, []string{"text"}, base)

	got, _ := h.repo.JobByID(ctx, j.ID)
	if got.State != core.JobDead || !strings.Contains(got.LastError, "convert") {
		t.Errorf("after a units stage into a raw one: state %q, last error %q; want dead, naming convert",
			got.State, got.LastError)
	}
}

// TestRawJobsCounterIncrementsOnDelivery is the rung-4 answer.
func TestRawJobsCounterIncrementsOnDelivery(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	rec := newRecorder()
	h.svc.SetCounter(rec)

	id := rawJobDone(t, h, "convert", "\x89PNG")
	f, err := h.svc.DeliverRaw(ctx, "u", id, base)
	if err != nil {
		t.Fatalf("DeliverRaw: %v", err)
	}
	_ = f.Close()

	if got := rec.value("ocrr_raw_jobs_total", map[string]string{"label": "convert"}); got != 1 {
		t.Errorf("ocrr_raw_jobs_total{label=convert} = %d after one raw delivery, want 1", got)
	}
}
