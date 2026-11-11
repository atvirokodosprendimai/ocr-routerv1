package agent_test

import (
	"slices"
	"testing"
	"time"
)

func (f *fakeRouter) leases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.leasesSeen...)
}

// TestWorkerSendsTheLeaseBack is ADR-0013 on the worker side: the token from
// the claim answer goes back on the input fetch and on the result.
func TestWorkerSendsTheLeaseBack(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("job-u", "src")
	startAgent(t, f, script(t, `echo '["a"]'`), 1)
	f.events <- "work"
	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second, "no report")

	got := f.leases()
	for _, want := range []string{"files lease-job-u", "report lease-job-u"} {
		if !slices.Contains(got, want) {
			t.Errorf("requests carried %q, want %q among them", got, want)
		}
	}
}

// TestRawWorkerSendsTheLeaseBack: the raw result carries it in the query,
// beside the job id.
func TestRawWorkerSendsTheLeaseBack(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("job-r", "src")
	startModeAgent(t, f, script(t, `printf 'x'`), true)
	f.events <- "work"
	waitFor(t, func() bool { b, _ := f.rawSnapshot(); return len(b) == 1 }, 5*time.Second, "no raw result")

	if got := f.leases(); !slices.Contains(got, "raw lease-job-r") {
		t.Errorf("requests carried %q, want \"raw lease-job-r\" among them", got)
	}
}

// TestReleaseSendsTheLease: the shutdown handover names the claim it hands back.
func TestReleaseSendsTheLease(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("job-hold", "src")
	_, _, cancel := startAgent(t, f, script(t, "sleep 5; echo '[\"a\"]'"), 1)
	waitFor(t, func() bool { return f.claims.Load() > 0 }, 3*time.Second, "the agent never claimed")
	time.Sleep(200 * time.Millisecond)
	cancel()
	waitFor(t, func() bool { return len(f.releases()) > 0 }, 3*time.Second, "no release")

	if got := f.leases(); !slices.Contains(got, "release lease-job-hold") {
		t.Errorf("requests carried %q, want \"release lease-job-hold\" among them", got)
	}
}

// TestWorkerOnAnOlderRouterSendsNoLease: a router that gave no token gets none
// back, and the job still completes — the new worker works against the old
// router.
func TestWorkerOnAnOlderRouterSendsNoLease(t *testing.T) {
	f := newFakeRouter()
	f.noLease = true
	f.enqueue("job-old", "src")
	startAgent(t, f, script(t, `echo '["a"]'`), 1)
	f.events <- "work"
	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second, "no report")

	for _, s := range f.leases() {
		if s != "files " && s != "report " {
			t.Errorf("a request carried %q against a router that gave no lease, want none", s)
		}
	}
}
