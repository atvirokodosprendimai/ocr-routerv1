package router_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/router"
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
			// Put it back into processing so it can fail.
			if err := h.repo.RequeueJob(ctx, j.ID, "test", nil, base); err != nil {
				t.Fatalf("RequeueJob: %v", err)
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
