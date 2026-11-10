package router_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/router"
)

func routerUpload(pipeline []string, body string, raw bool) router.UploadInput {
	return router.UploadInput{Pipeline: pipeline, Body: strings.NewReader(body), Raw: raw}
}

// failWritesWhere installs a trigger that aborts any UPDATE of jobs matching
// cond, which is how these tests make a guarded write lose at the exact point
// another worker's win would make it lose.
func failWritesWhere(t *testing.T, h *harness, cond string) {
	t.Helper()
	if _, err := h.db.Write.Exec(`CREATE TRIGGER lose BEFORE UPDATE ON jobs WHEN ` + cond +
		` BEGIN SELECT RAISE(ABORT, 'lost the race'); END`); err != nil {
		t.Fatalf("CREATE TRIGGER: %v", err)
	}
}

func readInput(t *testing.T, h *harness, id string) string {
	t.Helper()
	f, err := h.blobs.Open(id)
	if err != nil {
		t.Fatalf("Open input: %v", err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b)
}

// TestFailedAdvanceKeepsTheInput is F6 (REPORT 2026-11-10): a stage's output
// must not replace the job's input until the row has advanced. If the advance
// loses, the next claimant must run this stage on the original input, not on
// this stage's own output.
func TestFailedAdvanceKeepsTheInput(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "a")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	j := upload(t, h, "u", routerUpload([]string{"a", "b"}, "original", false))
	if _, err := h.svc.Claim(ctx, "w1", "a", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	failWritesWhere(t, h, "NEW.stage > OLD.stage")

	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"stage a output"}, base); err == nil {
		t.Fatal("Complete succeeded although the advance was refused")
	}
	if got := readInput(t, h, j.ID); got != "original" {
		t.Errorf("input after a refused advance = %q, want %q", got, "original")
	}
}

// TestFailedRawAdvanceKeepsTheInput is F6 on the raw bridge.
func TestFailedRawAdvanceKeepsTheInput(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	for _, l := range []string{"convert", "compress"} {
		if err := h.repo.SetRate(ctx, l, 1, true, base); err != nil {
			t.Fatalf("SetRate: %v", err)
		}
	}
	j := upload(t, h, "u", routerUpload([]string{"convert", "compress"}, "original", true))
	if _, err := h.svc.Claim(ctx, "w1", "convert", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	failWritesWhere(t, h, "NEW.stage > OLD.stage")

	if err := h.svc.CompleteRaw(ctx, "w1", j.ID, strings.NewReader("converted"), base); err == nil {
		t.Fatal("CompleteRaw succeeded although the advance was refused")
	}
	if got := readInput(t, h, j.ID); got != "original" {
		t.Errorf("input after a refused advance = %q, want %q", got, "original")
	}
}
