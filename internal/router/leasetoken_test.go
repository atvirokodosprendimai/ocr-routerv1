package router_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
	"github.com/atvirokodosprendimai/ocr-routerv1/internal/router"
)

// warnLogger keeps the warnings and drops the transitions.
type warnLogger struct {
	mu    sync.Mutex
	warns []string
}

func (*warnLogger) Transition(router.TransitionEvent) {}

func (w *warnLogger) Warn(msg string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var b strings.Builder
	b.WriteString(msg)
	for _, a := range args {
		b.WriteString(" ")
		if s, ok := a.(string); ok {
			b.WriteString(s)
		}
	}
	w.warns = append(w.warns, b.String())
}

func (w *warnLogger) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.warns...)
}

// staleAndCurrent leaves job j claimed TWICE by one worker token: the first
// claim's lease lapses, the reaper takes it back, and the same token claims it
// again — two slots of one worker process. It returns both claims.
func staleAndCurrent(t *testing.T, h *harness, label string) (stale, cur core.Job, at time.Time) {
	t.Helper()
	ctx := context.Background()
	stale, err := h.svc.Claim(ctx, "w1", label, base)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	at = base.Add(6 * time.Minute) // past the harness's 5m lease
	if _, err := h.svc.Reap(ctx, at); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	cur, err = h.svc.Claim(ctx, "w1", label, at)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if stale.ID != cur.ID || stale.LeaseToken == "" || stale.LeaseToken == cur.LeaseToken {
		t.Fatalf("claims %s/%q and %s/%q: want one job under two distinct non-empty tokens",
			stale.ID, stale.LeaseToken, cur.ID, cur.LeaseToken)
	}
	return stale, cur, at
}

// TestStaleClaimOfTheSameWorkerIsRefused is ADR-0013. The holder check
// compared only the worker TOKEN id, so the slow slot's report landed on the
// job the fast slot of the same worker now holds. Each worker write must be
// refused under the older claim's token, say why, and leave the current claim
// in place; the current claim's own write must still work.
func TestStaleClaimOfTheSameWorkerIsRefused(t *testing.T) {
	ops := map[string]func(h *harness, id, lease string, at time.Time) error{
		"Complete": func(h *harness, id, lease string, at time.Time) error {
			return h.svc.Complete(context.Background(), "w1", id, lease, []string{"p"}, at)
		},
		"Fail": func(h *harness, id, lease string, at time.Time) error {
			return h.svc.Fail(context.Background(), "w1", id, lease, "boom", nil, at)
		},
		"ReleaseLease": func(h *harness, id, lease string, at time.Time) error {
			return h.svc.ReleaseLease(context.Background(), "w1", id, lease, at)
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.worker(t, "ocr")
			h.user(t, "u", 100, 4, 0)
			upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
			stale, cur, at := staleAndCurrent(t, h, "ocr")

			err := op(h, stale.ID, stale.LeaseToken, at)
			if !errors.Is(err, core.ErrConflict) || !strings.Contains(err.Error(), "not the current claim") {
				t.Fatalf("%s under the stale claim = %v, want core.ErrConflict naming a stale claim", name, err)
			}
			got, _ := h.repo.JobByID(context.Background(), cur.ID)
			if got.State != core.JobProcessing || got.LeaseToken != cur.LeaseToken {
				t.Fatalf("after the refused %s the job is %q under %q, want processing under the current claim",
					name, got.State, got.LeaseToken)
			}
			if err := op(h, cur.ID, cur.LeaseToken, at); err != nil {
				t.Errorf("%s under the current claim = %v, want nil", name, err)
			}
		})
	}
}

// TestStaleRawClaimIsRefused is the same rule on the raw result path, the
// second write path where a guard gets forgotten.
func TestStaleRawClaimIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	if err := h.repo.SetRate(ctx, "convert", 1, true, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}
	upload(t, h, "u", router.UploadInput{
		Pipeline: []string{"convert"}, Body: strings.NewReader("source"), Raw: true,
	})
	stale, cur, at := staleAndCurrent(t, h, "convert")

	err := h.svc.CompleteRaw(ctx, "w1", stale.ID, stale.LeaseToken, strings.NewReader("stale"), at)
	if !errors.Is(err, core.ErrConflict) {
		t.Fatalf("CompleteRaw under the stale claim = %v, want core.ErrConflict", err)
	}
	if err := h.svc.CompleteRaw(ctx, "w1", cur.ID, cur.LeaseToken, strings.NewReader("fresh"), at); err != nil {
		t.Fatalf("CompleteRaw under the current claim = %v, want nil", err)
	}
}

// TestWorkerWithoutALeaseIsAcceptedAndWarned is ADR-0013's compatibility
// path, for ONE release: a worker built before the token sends none, is
// authorised as before by its token id and the lease's expiry, and the
// operator is told which worker to upgrade.
func TestWorkerWithoutALeaseIsAcceptedAndWarned(t *testing.T) {
	h := newHarness(t)
	rec := &warnLogger{}
	h.svc.SetLogger(rec)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	ctx := context.Background()
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, "", []string{"p"}, base); err != nil {
		t.Fatalf("Complete without a lease = %v, want nil for this release", err)
	}
	warns := rec.all()
	if len(warns) != 1 || !strings.Contains(warns[0], "w1") || !strings.Contains(warns[0], j.ID) {
		t.Errorf("warnings = %q, want one naming worker w1 and job %s", warns, j.ID)
	}
}

// TestExpiredLeaseIsRefusedSayingSo: the current claim's token does not
// outlive its lease, and the refusal names the expiry rather than a bare 409.
func TestExpiredLeaseIsRefusedSayingSo(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	ctx := context.Background()
	c, err := h.svc.Claim(ctx, "w1", "ocr", base)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	err = h.svc.Complete(ctx, "w1", c.ID, c.LeaseToken, []string{"late"}, base.Add(10*time.Minute))
	if !errors.Is(err, core.ErrConflict) || !strings.Contains(err.Error(), "expired") {
		t.Errorf("Complete past the lease = %v, want core.ErrConflict saying it expired", err)
	}
}

// TestTokenlessWorkerIsStillRefusedPastItsLease: the compatibility path skips
// only the token compare, never the expiry (commit-3 review).
func TestTokenlessWorkerIsStillRefusedPastItsLease(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
	ctx := context.Background()
	c, err := h.svc.Claim(ctx, "w1", "ocr", base)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	err = h.svc.Complete(ctx, "w1", c.ID, "", []string{"late"}, base.Add(10*time.Minute))
	if !errors.Is(err, core.ErrConflict) || !strings.Contains(err.Error(), "expired") {
		t.Errorf("token-less Complete past the lease = %v, want core.ErrConflict saying it expired", err)
	}
}

// TestTokenlessStaleSlotNeverSwapsTheInput pins lockHolder's recheck against
// the SNAPSHOT's token (ADR-0013). A token-less raw upload by w1 passes the
// first check; while its body streams the lease lapses, the reaper takes the
// job back and w1 — another slot of the same worker token — claims it again.
// Re-checked against the token the worker presented (none), the stale slot
// would still look like the holder and swap its output onto the input the
// current slot is being served; re-checked against the snapshot's token it is
// refused before the swap.
func TestTokenlessStaleSlotNeverSwapsTheInput(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "convert")
	h.user(t, "u", 100, 4, 0)
	ctx := context.Background()
	for _, l := range []string{"convert", "compress"} {
		if err := h.repo.SetRate(ctx, l, 1, true, base); err != nil {
			t.Fatal(err)
		}
	}
	j := upload(t, h, "u", routerUpload([]string{"convert", "compress"}, "original", true))
	if _, err := h.svc.Claim(ctx, "w1", "convert", base); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- h.svc.CompleteRaw(ctx, "w1", j.ID, "", pr, base) }()
	defer pw.Close()
	if _, err := pw.Write([]byte("A-")); err != nil { // past its first check, inside Stage
		t.Fatal(err)
	}
	later := base.Add(6 * time.Minute)
	if _, err := h.svc.Reap(ctx, later); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Claim(ctx, "w1", "convert", later); err != nil {
		t.Fatal(err)
	}
	// Hold the single writer so a swap that happened would sit visible while
	// the doomed advance waits.
	tx, err := h.db.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE users SET credits = credits WHERE id = 'u'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write([]byte("output")); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	var err1 error
	select {
	case err1 = <-done:
	case <-time.After(2 * time.Second):
		got := readInput(t, h, j.ID)
		_ = tx.Rollback()
		<-done
		t.Fatalf("the stale slot is inside its advance; the current slot is served %q as its input", got)
	}
	got := readInput(t, h, j.ID)
	_ = tx.Rollback()
	if !errors.Is(err1, core.ErrConflict) {
		t.Errorf("the stale slot's CompleteRaw = %v, want core.ErrConflict", err1)
	}
	if got != "original" {
		t.Errorf("input after the stale slot's refusal = %q, want %q", got, "original")
	}
	assertOnlyTheJobsFiles(t, h, j.ID)
}
