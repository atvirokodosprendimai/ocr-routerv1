package agent_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/agent"
	"github.com/atvirokodosprendimai/ocr-router/internal/runner"
)

// pngMagic is deliberately not valid UTF-8 — see internal/runner/raw_test.go.
const pngMagic = "\x89PNG\r\n\x1a\n"

// startModeAgent is startAgent with the declared mode made explicit, so a test
// can assert what a raw worker and a units worker each send.
func startModeAgent(t *testing.T, f *fakeRouter, cmd string, raw bool) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	a := agent.New(agent.Config{
		RouterURL: srv.URL, Token: "t", Label: "test", TmpDir: t.TempDir(), Slots: 1,
		Raw: raw,
	}, runner.Runner{
		Cmd: cmd, Timeout: 10 * time.Second, MaxOutput: 1 << 20,
		Env: []string{"PATH=/usr/bin:/bin"}, Raw: raw,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = a.Run(ctx) }()
	t.Cleanup(cancel)
}

func (f *fakeRouter) rawSnapshot() ([][]byte, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.rawBodies...), append([]string(nil), f.rawJobIDs...)
}

func (f *fakeRouter) declared() (sse, claim string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sseRaw, f.claimRaw
}

// TestRawWorkerPostsOctetStream is the end of the worker half of ADR-0006: the
// bytes leave the worker without ever entering a JSON string.
func TestRawWorkerPostsOctetStream(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("job-raw", "ignored")

	startModeAgent(t, f, script(t, `printf '\211PNG\r\n\032\n'`), true)
	f.events <- "work"

	waitFor(t, func() bool { b, _ := f.rawSnapshot(); return len(b) == 1 }, 5*time.Second,
		"the raw worker never posted an octet-stream result")

	bodies, ids := f.rawSnapshot()
	if string(bodies[0]) != pngMagic {
		t.Errorf("posted % x (%d bytes), want % x (%d bytes) — the bytes changed on the way out",
			bodies[0], len(bodies[0]), pngMagic, len(pngMagic))
	}
	if ids[0] != "job-raw" {
		t.Errorf("job_id = %q, want job-raw — the id rides the query because the body is the payload", ids[0])
	}
	if reps := f.reportsSnapshot(); len(reps) != 0 {
		t.Errorf("a raw success also posted %d JSON report(s); it must take exactly one path", len(reps))
	}
}

// TestRawWorkerDeclaresMode covers the rung-2 selection: the flag must reach
// BOTH declaration points, or the router registers a mode the worker did not
// mean on whichever path it missed.
func TestRawWorkerDeclaresMode(t *testing.T) {
	f := newFakeRouter()
	startModeAgent(t, f, script(t, `printf 'x'`), true)

	// waitFor only bounds the wait. The assertion is below and in this body on
	// purpose: it names the values that were actually sent, which "timed out" does
	// not, and it is what makes this test's own failure path visible.
	waitFor(t, func() bool { sse, claim := f.declared(); return sse != "" && claim != "" },
		5*time.Second, "the worker never reached both /sse and /claim")

	sse, claim := f.declared()
	if sse != "1" || claim != "1" {
		t.Errorf("a --raw worker declared sse=%q claim=%q, want 1 and 1 — the flag must reach "+
			"BOTH declaration points, or the router registers a mode the worker did not mean on "+
			"whichever path it missed", sse, claim)
	}
}

// TestUnitsWorkerDeclaresUnits is the same assertion inverted. Without it the
// test above is satisfied by a worker that hardcodes raw=1.
func TestUnitsWorkerDeclaresUnits(t *testing.T) {
	f := newFakeRouter()
	startModeAgent(t, f, script(t, `printf '["ok"]'`), false)

	waitFor(t, func() bool { sse, claim := f.declared(); return sse != "" && claim != "" },
		5*time.Second, "the worker never reached both /sse and /claim")

	sse, claim := f.declared()
	if sse != "0" || claim != "0" {
		t.Errorf("a units worker declared sse=%q claim=%q, want 0 and 0", sse, claim)
	}
}

// TestRawFailureIsStillJSON keeps the failure path on one encoding.
//
// A failure is a reason STRING in every mode, so giving failures two encodings
// would double the router's parse surface for nothing.
func TestRawFailureIsStillJSON(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("job-fail", "x")

	startModeAgent(t, f, script(t, `echo "boom" >&2; exit 3`), true)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"a failing raw job never reported")

	rep := f.reportsSnapshot()[0]
	if rep.JobID != "job-fail" || rep.Error == "" {
		t.Errorf("failure report = %+v, want a JSON failure naming the job", rep)
	}
	if bodies, _ := f.rawSnapshot(); len(bodies) != 0 {
		t.Errorf("a FAILED raw job posted %d octet-stream body/bodies — failures stay JSON", len(bodies))
	}
}

// TestUnitsWorkerUnchanged pins that a worker without --raw still posts exactly
// what it posted before this ADR.
func TestUnitsWorkerUnchanged(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("job-units", "the source")

	startModeAgent(t, f, script(t, `printf '["a","b"]'`), false)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the units worker never reported")

	rep := f.reportsSnapshot()[0]
	if len(rep.Units) != 2 || rep.Units[0] != "a" || rep.Units[1] != "b" {
		t.Errorf("units report = %+v", rep)
	}
	if bodies, _ := f.rawSnapshot(); len(bodies) != 0 {
		t.Error("a units worker posted an octet-stream body")
	}
}
