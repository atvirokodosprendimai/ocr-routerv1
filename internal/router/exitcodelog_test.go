package router_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// TestFailureLogCarriesTheExitCode is ADR-0007's log half.
//
// ⚠ A SUCCESS LINE MUST NOT CARRY ONE, and that is asserted too. Without the
// negative half, "the failure line has an exit code" is satisfied by a
// implementation that puts one on every line — which would make every delivered
// job look like it exited 0, and 0 is a real answer.
func TestFailureLogCarriesTheExitCode(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	rec := &recordingLogger{}
	h.svc.SetLogger(rec)

	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	code := 3
	if err := h.svc.Fail(ctx, "w1", j.ID, "exit status 3: boom", &code, base); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	ev, ok := rec.find(core.JobProcessing, core.JobQueued)
	if !ok {
		t.Fatal("no processing→queued transition was logged")
	}
	if ev.ExitCode == nil || *ev.ExitCode != 3 {
		t.Errorf("the failure line carried ExitCode = %v, want 3", ev.ExitCode)
	}

	// The claim line is a success and must carry none.
	if claim, ok := rec.find(core.JobQueued, core.JobProcessing); ok && claim.ExitCode != nil {
		t.Errorf("a successful transition carried ExitCode = %d — every delivered job would then "+
			"look like it exited 0", *claim.ExitCode)
	}
}

// TestReaperRecordsNoExitCode: a lease reclaimed from a vanished worker never ran
// to an exit, so inventing a code would be inventing a fact.
func TestReaperRecordsNoExitCode(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()

	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := h.svc.Reap(ctx, base.Add(time.Hour)); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	got, err := h.repo.JobByID(ctx, j.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.ExitCode != nil {
		t.Errorf("a reclaimed lease recorded ExitCode = %d, want nil — the worker vanished, it "+
			"did not exit", *got.ExitCode)
	}
}
