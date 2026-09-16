package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/agent"
	"github.com/atvirokodosprendimai/ocr-router/internal/runner"
)

// fakeRouter is a minimal stand-in for the real router, so the agent's loop can
// be driven precisely — a claim returning 204 on demand, an event emitted on
// demand, a stream dropped on demand.
type fakeRouter struct {
	mu sync.Mutex

	queued   []string          // job ids waiting to be claimed
	blobs    map[string]string // job id -> source bytes
	reports  []report
	claims   atomic.Int64
	streams  atomic.Int64
	sendPing bool

	events chan string // event names to push to the connected stream
	drop   chan struct{}

	// What the worker DECLARED and POSTED, for ADR-0006's tests. Captured on the
	// fake rather than asserted through the real router, because the property
	// under test is what the AGENT sends — the router's half is T2's.
	sseRaw    string   // ?raw= seen on the subscribe
	claimRaw  string   // ?raw= seen on the claim
	rawBodies [][]byte // octet-stream bodies, in order
	rawJobIDs []string // ?job_id= that came with each
}

type report struct {
	JobID string   `json:"job_id"`
	Units []string `json:"units"`
	Error string   `json:"error"`
}

func newFakeRouter() *fakeRouter {
	return &fakeRouter{
		blobs:  map[string]string{},
		events: make(chan string, 64),
		drop:   make(chan struct{}),
	}
}

func (f *fakeRouter) enqueue(id, blob string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = append(f.queued, id)
	if blob != "" {
		f.blobs[id] = blob
	}
}

func (f *fakeRouter) reportsSnapshot() []report {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]report(nil), f.reports...)
}

func (f *fakeRouter) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		f.streams.Add(1)
		f.mu.Lock()
		f.sseRaw = r.URL.Query().Get("raw")
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: hello\ndata: {}\n\n")
		w.(http.Flusher).Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-f.drop:
				return
			case name := <-f.events:
				fmt.Fprintf(w, "event: %s\ndata: {}\n\n", name)
				w.(http.Flusher).Flush()
			}
		}
	})

	mux.HandleFunc("/claim", func(w http.ResponseWriter, r *http.Request) {
		f.claims.Add(1)
		f.mu.Lock()
		f.claimRaw = r.URL.Query().Get("raw")
		f.mu.Unlock()
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.queued) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		id := f.queued[0]
		f.queued = f.queued[1:]
		_, hasBlob := f.blobs[id]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"job_id": id, "label": "test", "has_blob": hasBlob,
			"params": map[string]string{"n": id},
		})
	})

	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		id := filepath.Base(r.URL.Path)
		f.mu.Lock()
		blob, ok := f.blobs[id]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(blob))
	})

	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		// Branch on the content type, exactly as the real router's worker arm
		// does: octet-stream is a raw result, JSON is units-or-failure.
		if r.Header.Get("Content-Type") == "application/octet-stream" {
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.rawBodies = append(f.rawBodies, body)
			f.rawJobIDs = append(f.rawJobIDs, r.URL.Query().Get("job_id"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var rep report
		_ = json.NewDecoder(r.Body).Decode(&rep)
		f.mu.Lock()
		f.reports = append(f.reports, rep)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "svc.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	return path
}

func startAgent(t *testing.T, f *fakeRouter, cmd string, slots int) (*agent.Agent, string, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	a := agent.New(agent.Config{
		RouterURL: srv.URL, Token: "t", Label: "test", TmpDir: tmp, Slots: slots,
	}, runner.Runner{
		Cmd: cmd, Timeout: 10 * time.Second, MaxOutput: 1 << 20,
		Env: []string{"PATH=/usr/bin:/bin"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = a.Run(ctx) }()
	t.Cleanup(cancel)
	return a, tmp, cancel
}

func waitFor(t *testing.T, cond func() bool, within time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", msg)
}

func TestAgentClaimsRunsAndReports(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("job-1", "the source")

	// A service that echoes its input file back as one unit.
	svc := script(t, `
while [ $# -gt 0 ]; do case "$1" in -i) in_file="$2"; shift 2 ;; *) shift ;; esac; done
printf '["%s"]' "$(cat "$in_file")"
`)
	startAgent(t, f, svc, 2)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent never reported a result")

	rep := f.reportsSnapshot()[0]
	if rep.JobID != "job-1" || len(rep.Units) != 1 || rep.Units[0] != "the source" {
		t.Errorf("report = %+v", rep)
	}
	if rep.Error != "" {
		t.Errorf("unexpected error: %q", rep.Error)
	}
}

func TestAgentClaimsWithoutBlob(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("crawl-1", "") // no blob: the crawler shape

	// The service reads --n instead of a file.
	svc := script(t, `
while [ $# -gt 0 ]; do case "$1" in --n) n="$2"; shift 2 ;; *) shift ;; esac; done
printf '["fetched %s"]' "$n"
`)
	startAgent(t, f, svc, 1)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent never reported")
	rep := f.reportsSnapshot()[0]
	if len(rep.Units) != 1 || rep.Units[0] != "fetched crawl-1" {
		t.Errorf("report = %+v — a params-only job must run without -i", rep)
	}
}

func TestAgentClaimsOnPing(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("job-1", "x")

	svc := script(t, `printf '["ok"]'`)
	startAgent(t, f, svc, 1)

	// ⚠ No `work` event is ever sent. Only a ping. The bus drops a superseded
	// event when a buffer is full, so a work notification can legitimately be
	// lost — without claiming on ping, a worker would then idle until the next
	// upload happened to arrive.
	f.events <- "ping"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent did not claim on a ping — a dropped work event would strand it")
}

func TestAgentDrainsOnConnect(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("pre-existing", "x")

	// No event at all: the job was queued before this worker existed, and
	// nothing will push a notification for work that is already waiting.
	startAgent(t, f, script(t, `printf '["ok"]'`), 1)

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent did not drain the queue on connect")
}

func TestAgentRespectsSlots(t *testing.T) {
	f := newFakeRouter()
	for i := range 5 {
		f.enqueue(fmt.Sprintf("job-%d", i), "x")
	}

	// A service that records concurrency by creating and removing a marker.
	dir := t.TempDir()
	svc := script(t, fmt.Sprintf(`
d=%q
touch "$d/$$"
n=$(ls "$d" | wc -l | tr -d ' ')
echo "$n" >> "$d/../peak"
sleep 0.3
rm -f "$d/$$"
printf '["ok"]'
`, dir))

	startAgent(t, f, svc, 2)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 5 }, 20*time.Second,
		"not all jobs completed")

	peak := 0
	b, err := os.ReadFile(filepath.Join(dir, "..", "peak"))
	if err != nil {
		t.Fatalf("reading concurrency log: %v", err)
	}
	for _, line := range splitLines(string(b)) {
		var n int
		if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n > peak {
			peak = n
		}
	}
	if peak > 2 {
		t.Errorf("peak concurrency was %d with slots=2 — the queue, not the worker's memory, "+
			"is where work should wait", peak)
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestAgentReportsFailureAndContinues(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("bad", "x")
	f.enqueue("good", "x")

	// Fails for the job whose --n is "bad", succeeds otherwise.
	svc := script(t, `
while [ $# -gt 0 ]; do case "$1" in --n) n="$2"; shift 2 ;; *) shift ;; esac; done
if [ "$n" = "bad" ]; then echo "tool exploded" >&2; exit 2; fi
printf '["ok"]'
`)
	startAgent(t, f, svc, 1)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 2 }, 10*time.Second,
		"the agent stopped after the failing job")

	var failed, ok bool
	for _, rep := range f.reportsSnapshot() {
		switch rep.JobID {
		case "bad":
			failed = rep.Error != ""
			if failed && !contains(rep.Error, "tool exploded") {
				t.Errorf("the failure report %q does not carry the tool's stderr", rep.Error)
			}
		case "good":
			ok = len(rep.Units) == 1
		}
	}
	if !failed {
		t.Error("the failing job was not reported as an error")
	}
	if !ok {
		t.Error("the agent did not go on to the next job — one bad document must not stop a " +
			"worker serving every other customer")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestAgentDeletesTempFileOnEveryPath is the leak guard.
func TestAgentDeletesTempFileOnEveryPath(t *testing.T) {
	cases := map[string]string{
		"success":       `printf '["ok"]'`,
		"non-zero exit": `exit 1`,
		"bad output":    `printf 'not json'`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeRouter()
			f.enqueue("job-1", "some bytes")
			_, tmp, _ := startAgent(t, f, script(t, body), 1)
			f.events <- "work"

			waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
				"no report")

			// Give the deferred cleanup a moment after the report.
			waitFor(t, func() bool {
				entries, err := os.ReadDir(tmp)
				return err == nil && len(entries) == 0
			}, 3*time.Second,
				"the temp file survived — a busy worker would fill its tmpdir with other "+
					"people's documents")
		})
	}
}

func TestAgentReconnectsAfterStreamDrop(t *testing.T) {
	f := newFakeRouter()
	startAgent(t, f, script(t, `printf '["ok"]'`), 1)

	waitFor(t, func() bool { return f.streams.Load() >= 1 }, 5*time.Second, "no initial stream")

	close(f.drop) // every connected stream ends

	waitFor(t, func() bool { return f.streams.Load() >= 2 }, 15*time.Second,
		"the agent did not reconnect after the stream dropped")
}

func TestAgentStopsOnContextCancel(t *testing.T) {
	f := newFakeRouter()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	a := agent.New(agent.Config{
		RouterURL: srv.URL, Token: "t", Label: "test", TmpDir: t.TempDir(), Slots: 1,
	}, runner.Runner{Cmd: script(t, `printf '["ok"]'`), Timeout: time.Second, MaxOutput: 1024})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v after cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not return after its context was cancelled")
	}
}
