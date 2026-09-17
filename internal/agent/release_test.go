package agent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/agent"
	"github.com/atvirokodosprendimai/ocr-router/internal/runner"
)

// startAgentReturning is startAgent with the one thing shutdown tests need: a
// channel that closes when Run RETURNS. Without it a test can only time cancel(),
// which never blocks whatever the agent does afterwards.
func startAgentReturning(t *testing.T, f *fakeRouter, cmd string) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	a := agent.New(agent.Config{
		RouterURL: srv.URL, Token: "t", Label: "test", TmpDir: t.TempDir(), Slots: 1,
	}, runner.Runner{
		Cmd: cmd, Timeout: 10 * time.Second, MaxOutput: 1 << 20,
		Env: []string{"PATH=/usr/bin:/bin"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = a.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

// releases returns the job ids the fake router has been asked to release.
func (f *fakeRouter) releases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.released...)
}

// TestWorkerReleasesOnShutdown: the lease goes back the moment the operator
// stops the worker, rather than in five minutes' time.
func TestWorkerReleasesOnShutdown(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("job-hold", "src")

	// A command that outlives the cancellation, so the agent is genuinely
	// holding the job when it is told to stop.
	_, _, cancel := startAgent(t, f, script(t, "sleep 5; echo '[\"a\"]'"), 1)
	waitFor(t, func() bool { return f.claims.Load() > 0 }, 3*time.Second, "the agent never claimed")
	// Give process() time to register the job as in-flight before shutdown.
	time.Sleep(200 * time.Millisecond)

	cancel()
	waitFor(t, func() bool { return len(f.releases()) > 0 }, 3*time.Second,
		"the agent exited without handing its lease back — the job then waits out a full lease "+
			"before anybody can run it, which is the delay this route exists to remove")

	got := f.releases()
	if got[0] != "job-hold" {
		t.Errorf("released %q, want job-hold", got[0])
	}
	if len(got) != 1 {
		t.Errorf("released %d times, want 1 — a release per held job, not per retry", len(got))
	}
}

// TestReleaseFailureIsNotFatal: shutdown must not block on a best-effort call.
//
// The safety net is the lease, so the worst case of a failed release is exactly
// today's behaviour — while a shutdown that HANGS turns a fast restart into a
// slow one, which is the problem this task exists to remove.
func TestReleaseFailureIsNotFatal(t *testing.T) {
	f := newFakeRouter()
	f.releaseStatus = http.StatusNotFound
	f.enqueue("job-hold", "src")

	cancel, done := startAgentReturning(t, f, script(t, "sleep 5; echo '[\"a\"]'"))
	waitFor(t, func() bool { return f.claims.Load() > 0 }, 3*time.Second, "the agent never claimed")
	time.Sleep(200 * time.Millisecond)

	// ⚠ The assertion is on Run RETURNING, not on cancel returning — cancel never
	// blocks, so a test that timed it would pass against an agent that hangs
	// forever in its shutdown path.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not exit while the router was refusing its release — a " +
			"best-effort call is blocking shutdown, which is the slow restart this task removes")
	}
}
