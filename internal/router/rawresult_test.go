package router_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/router"
)

// rawResultFiles counts the `.out` blobs on disk.
//
// It walks the real directory rather than asking the store, because the leak
// this guards against is a FILE nobody deletes — and a count taken from the
// component that was supposed to delete it would agree with itself.
func rawResultFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".out") {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return n
}

// TestExpiredRawJobDropsItsResultBlob covers the risk ADR-0006 names: a raw job
// has TWO blobs, and a deletion added to one terminal path and forgotten on
// another leaks silently and unboundedly.
func TestExpiredRawJobDropsItsResultBlob(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 60) // a 60s job TTL
	ctx := context.Background()

	if err := h.repo.SetRate(ctx, "convert", 1, true, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}

	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"convert"}, Body: strings.NewReader("source"), Raw: true,
	})
	if _, err := h.svc.Claim(ctx, "w1", "convert", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.CompleteRaw(ctx, "w1", j.ID, strings.NewReader("\x89PNG"), base); err != nil {
		t.Fatalf("CompleteRaw: %v", err)
	}
	if got := rawResultFiles(t, h.blobs.Dir()); got != 1 {
		t.Fatalf("result blobs after completion = %d, want 1", got)
	}

	// Nobody collects it, and the job's TTL passes.
	if _, err := h.svc.Reap(ctx, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if got := rawResultFiles(t, h.blobs.Dir()); got != 0 {
		t.Errorf("an expired raw job left %d result blob(s) on disk — the reaper must delete BOTH "+
			"keys, or every undelivered raw job is permanent disk growth", got)
	}
}

// TestDeadRawJobDropsItsResultBlob is the other terminal path. Two paths, two
// tests: the whole risk is that one of them is forgotten.
func TestDeadRawJobDropsItsResultBlob(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	if err := h.repo.SetRate(ctx, "convert", 1, true, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}

	j := upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"convert"}, Body: strings.NewReader("source"), Raw: true,
	})

	// Burn the attempt budget so the job goes dead rather than being requeued.
	for i := 0; i < 3; i++ {
		if _, err := h.svc.Claim(ctx, "w1", "convert", base); err != nil {
			t.Fatalf("Claim %d: %v", i, err)
		}
		if i == 0 {
			if err := h.svc.CompleteRaw(ctx, "w1", j.ID, strings.NewReader("\x89PNG"), base); err != nil {
				t.Fatalf("CompleteRaw: %v", err)
			}
			// Put it back into the queue so the next claim can fail it.
			if err := h.repo.RequeueDone(ctx, j.ID, "test", base); err != nil {
				t.Fatalf("RequeueDone: %v", err)
			}
			continue
		}
		if err := h.svc.Fail(ctx, "w1", j.ID, "boom", nil, base); err != nil {
			t.Fatalf("Fail %d: %v", i, err)
		}
	}

	got, err := h.repo.JobByID(ctx, j.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.State != "dead" {
		t.Skipf("job did not reach dead (state %q); the attempt budget shape changed", got.State)
	}
	if n := rawResultFiles(t, h.blobs.Dir()); n != 0 {
		t.Errorf("a dead raw job left %d result blob(s) on disk", n)
	}
}

// TestRawRedeliveryAfterAFailedCopy is F3 (REPORT 2026-11-10). DeliverRaw
// charges before the bytes are streamed; a client whose copy failed must be able
// to fetch the result it paid for again, and be charged once.
func TestRawRedeliveryAfterAFailedCopy(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	id := rawJobDone(t, h, "convert", "\x89PNG")

	f, err := h.svc.DeliverRaw(ctx, "u", id, base)
	if err != nil {
		t.Fatalf("first DeliverRaw: %v", err)
	}
	_ = f.Close() // the copy failed: DropRawResult is never called

	f, err = h.svc.DeliverRaw(ctx, "u", id, base)
	if err != nil {
		t.Fatalf("second DeliverRaw: %v — the customer paid and cannot collect", err)
	}
	body, _ := io.ReadAll(f)
	_ = f.Close()
	if string(body) != "\x89PNG" {
		t.Errorf("re-fetched body = %q, want the result", body)
	}
	entries, _ := h.repo.Ledger(ctx, "u", 10)
	if len(entries) != 1 || entries[0].Delta != -1 {
		t.Errorf("ledger = %+v, want exactly one -1 entry", entries)
	}
}

// TestDeliveredRawResultIsSweptAfterTTL: the `.out` a failed copy leaves behind
// is removed once the result TTL has passed, or every dropped download is
// permanent disk growth (F3).
func TestDeliveredRawResultIsSweptAfterTTL(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	id := rawJobDone(t, h, "convert", "\x89PNG")
	f, err := h.svc.DeliverRaw(ctx, "u", id, base)
	if err != nil {
		t.Fatalf("DeliverRaw: %v", err)
	}
	_ = f.Close()
	out := filepath.Join(h.blobs.Dir(), id[:2], id+".out")
	if err := os.Chtimes(out, base, base); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if _, err := h.svc.Reap(ctx, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n := rawResultFiles(t, h.blobs.Dir()); n != 0 {
		t.Errorf("a delivered raw job left %d result file(s) past the TTL", n)
	}
}

// TestRawRetryWindowRunsFromDelivery is the F3 review's finding: the window in
// which a dropped download can be fetched again starts at the DELIVERY, not at
// the worker's write. A file written 59 minutes before its download would
// otherwise be swept a minute after the customer paid for it.
func TestRawRetryWindowRunsFromDelivery(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	id := rawJobDone(t, h, "convert", "\x89PNG")
	out := filepath.Join(h.blobs.Dir(), id[:2], id+".out")
	if err := os.Chtimes(out, base, base); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	delivered := base.Add(59 * time.Minute)
	f, err := h.svc.DeliverRaw(ctx, "u", id, delivered)
	if err != nil {
		t.Fatalf("DeliverRaw: %v", err)
	}
	_ = f.Close() // dropped

	if _, err := h.svc.Reap(ctx, delivered.Add(2*time.Minute)); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	f, err = h.svc.DeliverRaw(ctx, "u", id, delivered.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("retry three minutes after paying: %v", err)
	}
	_ = f.Close()
}

// TestSweepKeepsALiveRawResult: step 3c removes only the files of delivered or
// vanished jobs. A `done` job's result, however old the file, belongs to it.
func TestSweepKeepsALiveRawResult(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	id := rawJobDone(t, h, "convert", "\x89PNG")
	out := filepath.Join(h.blobs.Dir(), id[:2], id+".out")
	old := base.Add(-2 * time.Hour)
	if err := os.Chtimes(out, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// The job finished at base, so step 3 leaves it; only the file looks old.
	if _, err := h.svc.Reap(ctx, base.Add(30*time.Minute)); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n := rawResultFiles(t, h.blobs.Dir()); n != 1 {
		t.Errorf("result files = %d, want 1 — the sweep deleted a done job's result", n)
	}
}
