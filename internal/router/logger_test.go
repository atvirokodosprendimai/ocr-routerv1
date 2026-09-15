package router_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/logging"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// recordingLogger keeps every transition for assertion.
type recordingLogger struct {
	mu     sync.Mutex
	events []router.TransitionEvent
}

func (r *recordingLogger) Transition(e router.TransitionEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingLogger) all() []router.TransitionEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]router.TransitionEvent(nil), r.events...)
}

func (r *recordingLogger) find(from, to core.JobState) (router.TransitionEvent, bool) {
	for _, e := range r.all() {
		if e.From == from && e.To == to {
			return e, true
		}
	}
	return router.TransitionEvent{}, false
}

// TestTransitionIsLoggedForEveryStateChange goes red if any call site is dropped.
func TestTransitionIsLoggedForEveryStateChange(t *testing.T) {
	h := newHarness(t)
	rec := &recordingLogger{}
	h.svc.SetLogger(rec)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)

	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"p1"}, base); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := h.svc.Deliver(ctx, "u", j.ID, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	want := []struct{ from, to core.JobState }{
		{"", core.JobQueued},                 // upload
		{core.JobQueued, core.JobProcessing}, // claim
		{core.JobProcessing, core.JobDone},   // complete
		{core.JobDone, core.JobDelivered},    // deliver
	}
	for _, w := range want {
		if _, ok := rec.find(w.from, w.to); !ok {
			t.Errorf("no transition logged for %q -> %q; a job's path cannot be read end to end "+
				"with a step missing. Got: %+v", w.from, w.to, rec.all())
		}
	}
}

func TestTransitionCarriesInState(t *testing.T) {
	h := newHarness(t)
	rec := &recordingLogger{}
	h.svc.SetLogger(rec)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)

	upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})

	// Claimed 300 seconds after it was queued.
	later := base.Add(300 * time.Second)
	if _, err := h.svc.Claim(context.Background(), "w1", "ocr", later); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	e, ok := rec.find(core.JobQueued, core.JobProcessing)
	if !ok {
		t.Fatal("no claim transition logged")
	}
	if e.InState != 300*time.Second {
		t.Errorf("InState = %v, want 5m. Without this, 'the job died' cannot be distinguished "+
			"from 'it sat queued for five minutes and then died'", e.InState)
	}
}

// TestInStateSurvivesARestart is red if the duration is read from process memory.
func TestInStateSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)
	upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})

	// A SECOND service over the same database: nothing in memory carries over,
	// exactly as after a process restart.
	fresh := router.New(h.repo, h.blobs, h.results, h.bus, router.Config{
		Lease: 5 * time.Minute, MaxAttempts: 3, AgingStep: time.Minute,
		LabelGrace: 5 * time.Minute, DefaultLabel: "ocr",
	})
	rec := &recordingLogger{}
	fresh.SetLogger(rec)

	later := base.Add(600 * time.Second)
	if _, err := fresh.Claim(context.Background(), "w1", "ocr", later); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	e, ok := rec.find(core.JobQueued, core.JobProcessing)
	if !ok {
		t.Fatal("no claim transition logged")
	}
	if e.InState != 600*time.Second {
		t.Errorf("InState = %v after a restart, want 10m — the duration is being read from "+
			"memory that did not survive, so every post-restart line is silently wrong", e.InState)
	}
}

func TestReaperActionsAreLogged(t *testing.T) {
	h := newHarness(t)
	rec := &recordingLogger{}
	h.svc.SetLogger(rec)
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)

	ctx := context.Background()
	upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	report, err := h.svc.Reap(ctx, base.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if report.LeasesExpired == 0 {
		t.Fatalf("the fixture expired no lease, so this proves nothing: %+v", report)
	}

	var found bool
	for _, e := range rec.all() {
		if e.Actor == "reaper" {
			found = true
			if e.Reason == "" {
				t.Error("a reaper transition carries no reason — 'it went back to the queue' " +
					"without saying why is the line that sends someone reading source")
			}
		}
	}
	if !found {
		t.Errorf("the reaper moved a job and logged nothing. A reaper that silently stopped and "+
			"one that had nothing to do look identical. Got: %+v", rec.all())
	}
}

func TestServiceWithNoLoggerStillWorks(t *testing.T) {
	h := newHarness(t) // no SetLogger call
	h.worker(t, "ocr")
	h.user(t, "u", 10, 4, 0)

	ctx := context.Background()
	j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})
	if _, err := h.svc.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim with no logger: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"p"}, base); err != nil {
		t.Fatalf("Complete with no logger: %v", err)
	}
}

func TestSetLoggerNilIsSafe(t *testing.T) {
	h := newHarness(t)
	h.svc.SetLogger(nil)
	h.worker(t, "ocr")
	h.user(t, "u", 10, 4, 0)

	upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})
	if _, err := h.svc.Claim(context.Background(), "w1", "ocr", base); err != nil {
		t.Fatalf("Claim after SetLogger(nil): %v", err)
	}
}

// TestTransitionLogNeverContainsAParamValue runs through the REAL call site.
//
// ⚠ internal/logging's own redaction test proves the function redacts. It does
// not prove the ROUTER redacts: the router builds the event, and a call site
// that put a value into any other field would pass every test in that package.
// This drives a real job and renders through the real adapter.
func TestTransitionLogNeverContainsAParamValue(t *testing.T) {
	h := newHarness(t)
	var buf lockedBuf
	log, err := logging.New(logging.Options{Out: &buf})
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	h.svc.SetLogger(adapter{log})
	h.worker(t, "crawl")
	h.user(t, "u", 100, 4, 0)

	const secret = "hunter2-do-not-log"
	ctx := context.Background()
	j, err := h.svc.Upload(ctx, "u", router.UploadInput{
		Pipeline: []string{"crawl"},
		Params:   map[string]string{"url": "https://alice:" + secret + "@example.com/x"},
	}, base)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := h.svc.Claim(ctx, "w1", "crawl", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.svc.Complete(ctx, "w1", j.ID, []string{"page"}, base); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("nothing was logged, so neither half of this test proves anything")
	}
	// ★ The key MUST be present, or "the secret is absent" is satisfied by an
	// implementation that logs no params at all — and by a misspelled sentinel.
	if !strings.Contains(out, `"url"`) {
		t.Fatalf("the param KEY is missing, so the absence check below is vacuous:\n%s", out)
	}
	for _, leak := range []string{secret, "alice", "example.com"} {
		if strings.Contains(out, leak) {
			t.Errorf("the transition log leaks %q from a customer-supplied param value:\n%s",
				leak, out)
		}
	}
}

func TestTransitionLogIsValidJSON(t *testing.T) {
	h := newHarness(t)
	var buf lockedBuf
	log, _ := logging.New(logging.Options{Out: &buf})
	h.svc.SetLogger(adapter{log})
	h.worker(t, "ocr")
	h.user(t, "u", 100, 4, 0)

	upload(t, h, "u", router.UploadInput{Body: strings.NewReader("src")})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("no transition line emitted")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("the transition line is not JSON: %v (%s)", err, lines[0])
	}
	for _, k := range []string{"job", "from", "to", "actor", "in_state"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("the transition line has no %q field: %s", k, lines[0])
		}
	}
}

// adapter renders a router event through the logging package, the same way the
// composition root does. Duplicated here rather than exported from cmd, because
// a test that used a DIFFERENT adapter would prove nothing about the real one —
// and the binary's own version is covered by cmd/router's tests.
type adapter struct{ log *slog.Logger }

func (a adapter) Transition(e router.TransitionEvent) {
	logging.LogTransition(a.log, logging.Transition{
		JobID: e.JobID, UserID: e.UserID, Label: e.Label,
		From: string(e.From), To: string(e.To),
		Attempt: e.Attempt, WorkerID: e.WorkerID, Actor: e.Actor,
		Stage: e.Stage, Params: e.Params, Reason: e.Reason, InState: e.InState,
	})
}

// lockedBuf is a buffer safe for concurrent writes.
type lockedBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
