package agent_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// logSink collects a worker's log lines so a test can read what an operator
// would have seen.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) write(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// TestTheWorkerLogsTheInputItMaterialised is a diagnostic, and the reason it is
// worth a test is that its absence cost a real debugging session.
//
// A worker logged `job <id> (libre) started` and then a failure from a tool that
// could not read its input, with nothing anywhere saying what path the tool had
// been handed. The extension was the answer and the operator had no way to see
// it — not in the worker's log, not in the router, not in the dashboard.
func TestTheWorkerLogsTheInputItMaterialised(t *testing.T) {
	f := newFakeRouter()
	f.enqueueNamed("job-1", "the source", "quarterly.docx")

	sink := &logSink{}
	svc := script(t, `printf '["ok"]'`)
	startAgentWithLog(t, f, svc, 1, sink.write)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent never reported a result")

	logs := sink.all()
	if !strings.Contains(logs, "job-1.docx") {
		t.Errorf("the worker never logged the path it materialised, so an operator debugging a "+
			"tool that cannot read its input has nothing to look at:\n%s", logs)
	}
}

// TestTheWorkerSaysWhenItCouldNotTakeAnExtension is the other half, and the
// half that matters when something is wrong.
//
// A missing extension is the failure mode; logging the path only when there IS
// one would leave the interesting case silent. The line has to say that the
// filename offered nothing usable, so the operator knows the bare path is a
// finding rather than a log that forgot to print.
func TestTheWorkerSaysWhenItCouldNotTakeAnExtension(t *testing.T) {
	f := newFakeRouter()
	f.enqueueNamed("job-2", "the source", "scan_no_extension")

	sink := &logSink{}
	svc := script(t, `printf '["ok"]'`)
	startAgentWithLog(t, f, svc, 1, sink.write)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent never reported a result")

	logs := sink.all()
	if !strings.Contains(logs, "no usable extension") {
		t.Errorf("the worker logged a bare path without saying WHY it has no extension, so the "+
			"operator cannot tell a refused extension from a missing feature:\n%s", logs)
	}
}

// ⚠ TestTheWorkerNeverLogsTheCustomersFilename is a NEGATIVE, and it is the one
// that keeps this diagnostic safe to add.
//
// The filename is chosen by a customer and the worker's log goes to an
// operator's terminal or journal. Echoing it raw invites log injection —
// newlines that forge a line, ANSI escapes that rewrite what a human sees — and
// ADR-0002 already holds the line that client-supplied values do not reach the
// log (it emits param KEYS only, never their values). The materialised path is
// safe precisely because we built it: a job id we were given by the router plus
// an extension validated to [A-Za-z0-9].
func TestTheWorkerNeverLogsTheCustomersFilename(t *testing.T) {
	f := newFakeRouter()
	f.enqueueNamed("job-3", "the source",
		"invoice\n2026-09-28T00:00:00Z job-3 produced 999 unit(s)\x1b[2K.docx")

	sink := &logSink{}
	svc := script(t, `printf '["ok"]'`)
	startAgentWithLog(t, f, svc, 1, sink.write)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent never reported a result")

	logs := sink.all()
	if strings.Contains(logs, "invoice") {
		t.Errorf("the customer's filename reached the log verbatim:\n%q", logs)
	}
	if strings.Contains(logs, "\x1b") {
		t.Error("an ANSI escape from a customer's filename reached the operator's terminal")
	}
	if strings.Contains(logs, "produced 999 unit(s)") {
		t.Error("a customer forged a log line through their filename")
	}
	// The extension is still taken from it — that part is validated, so it is
	// the one thing that may survive.
	if !strings.Contains(logs, "job-3.docx") {
		t.Errorf("the validated extension was dropped along with the unsafe bytes:\n%s", logs)
	}
}
