package store_test

import (
	"context"
	"testing"
	"time"
)

func intp(n int) *int { return &n }

// TestExitCodeRoundTripsThroughTheJobRow is ADR-0007's persistence half.
//
// ⚠ 0 AND nil ARE DIFFERENT ANSWERS and both are asserted. 0 is the code for
// SUCCESS, so a failure recorded as 0 is a job that exited cleanly and failed
// anyway — which is either a lie or a bug, and in both cases must not be what
// "no exit code" looks like.
func TestExitCodeRoundTripsThroughTheJobRow(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)

	mkJob(t, r, "j-three", "u", "ocr", base, time.Time{})
	if err := r.FailJobDead(ctx, "j-three", "exit status 3: boom", intp(3), base); err != nil {
		t.Fatalf("FailJobDead: %v", err)
	}
	got, err := r.JobByID(ctx, "j-three")
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.ExitCode == nil || *got.ExitCode != 3 {
		t.Errorf("ExitCode = %v, want 3", got.ExitCode)
	}

	// A recorded ZERO must read back as zero, not as absent. A command can exit 0
	// and still fail its contract, and that is a fact worth keeping.
	mkJob(t, r, "j-zero", "u", "ocr", base, time.Time{})
	if err := r.FailJobDead(ctx, "j-zero", "produced no output", intp(0), base); err != nil {
		t.Fatalf("FailJobDead: %v", err)
	}
	if got, _ = r.JobByID(ctx, "j-zero"); got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("a recorded exit 0 read back as %v — 0 and absent must stay distinguishable", got.ExitCode)
	}
}

// TestNoExitCodeStaysNull is the invariant the nullable column exists for.
func TestNoExitCodeStaysNull(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)
	mkJob(t, r, "j-timeout", "u", "ocr", base, time.Time{})

	if err := r.FailJobDead(ctx, "j-timeout", "timed out after 5m", nil, base); err != nil {
		t.Fatalf("FailJobDead: %v", err)
	}
	got, err := r.JobByID(ctx, "j-timeout")
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.ExitCode != nil {
		t.Errorf("a timeout recorded ExitCode = %d, want nil — a job that never exited must not "+
			"read as one that exited cleanly", *got.ExitCode)
	}
}

// TestExitCodeSurvivesARequeue covers the second writer. A code stored by only
// one of them would appear on the dead row and vanish on the retried one.
func TestExitCodeSurvivesARequeue(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)
	mkJob(t, r, "j-retry", "u", "ocr", base, time.Time{})

	if err := r.RequeueJob(ctx, "j-retry", "exit status 9: nope", intp(9), base); err != nil {
		t.Fatalf("RequeueJob: %v", err)
	}
	got, err := r.JobByID(ctx, "j-retry")
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if got.ExitCode == nil || *got.ExitCode != 9 {
		t.Errorf("ExitCode after requeue = %v, want 9 — both failure writers persist it, or a "+
			"code appears on one row and vanishes on the next", got.ExitCode)
	}
}
