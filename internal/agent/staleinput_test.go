package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAStaleInputFileDoesNotBrickTheJob is a real failure, reported from a live
// worker on 2026-09-28.
//
// materialise opens the input with O_EXCL so a stale file can never be silently
// reused as though it were this job's input — the right intent. The consequence
// was that a worker killed mid-job left the file behind and PERMANENTLY BRICKED
// THAT JOB: every retry failed with `fetching input: … file exists`, three
// attempts, then dead. The operator saw a job failing for a reason that has
// nothing to do with the job, and the only cure was deleting a file by hand.
//
// ⚠ BOTH HALVES ARE ASSERTED, and a fix that satisfies one is wrong. The job
// must succeed, AND the command must see the freshly downloaded bytes — if a
// retry reused the stale file, the customer would be billed for a result
// computed from somebody's leftovers.
func TestAStaleInputFileDoesNotBrickTheJob(t *testing.T) {
	f := newFakeRouter()
	f.enqueueNamed("job-1", "the real source", "report.docx")

	sink := &logSink{}
	svc := script(t, `
while [ $# -gt 0 ]; do case "$1" in -i) in_file="$2"; shift 2 ;; *) shift ;; esac; done
printf '["%s"]' "$(cat "$in_file")"
`)
	_, tmp, _ := startAgentWithLog(t, f, svc, 1, sink.write)

	// Exactly what a killed worker leaves: the file this job will want, holding
	// a previous attempt's bytes.
	stale := filepath.Join(tmp, "job-1.docx")
	if err := os.WriteFile(stale, []byte("LEFTOVERS FROM A DEAD WORKER"), 0o600); err != nil {
		t.Fatalf("seeding the stale file: %v", err)
	}

	f.events <- "work"
	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent never reported a result")

	rep := f.reportsSnapshot()[0]
	if rep.Error != "" {
		t.Fatalf("a stale temp file failed the job: %q — a worker that died once bricks this "+
			"job id for ever", rep.Error)
	}
	if len(rep.Units) != 1 || rep.Units[0] != "the real source" {
		t.Errorf("units = %q, want [\"the real source\"] — the stale bytes were reused as this "+
			"job's input, so the customer is billed for a result computed from leftovers",
			rep.Units)
	}
	// The operator has to know a worker died here; silently absorbing it hides
	// the only evidence that anything went wrong earlier.
	if !strings.Contains(sink.all(), "stale") {
		t.Errorf("nothing in the log says a stale input was discarded:\n%s", sink.all())
	}
}

// TestACommandThatLeavesAChildHoldingStdoutDoesNotCostTheFullTimeout is the
// second half of the same live incident.
//
// `soffice` is a launcher: it starts a background process and exits. That
// background process INHERITS our stdout pipe, and cmd.Wait() returns only when
// every holder of the write end has closed it — not when the direct child exits.
// So a job whose command finished in two seconds sat until the 5-minute timeout
// and was then reported as a timeout, three times over. Fifteen minutes to learn
// nothing.
//
// The runner already anticipated this for the kill path (see its Setpgid
// comment). What was missing is a bound on the WAIT itself.
func TestACommandThatLeavesAChildHoldingStdoutDoesNotCostTheFullTimeout(t *testing.T) {
	f := newFakeRouter()
	f.enqueue("job-2", "the source")

	// Exits immediately, having left a background process holding stdout — the
	// soffice shape, reduced to one line.
	svc := script(t, `
sleep 30 &
printf '["done"]'
exit 0
`)
	startAgent(t, f, svc, 1)

	started := time.Now()
	f.events <- "work"
	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 8*time.Second,
		"the worker never reported — cmd.Wait() is still waiting for a grandchild to close the pipe")

	took := time.Since(started)
	rep := f.reportsSnapshot()[0]
	if rep.Error != "" {
		t.Errorf("the job failed: %q — a lingering grandchild must not turn a successful command "+
			"into a failure", rep.Error)
	}
	if len(rep.Units) != 1 || rep.Units[0] != "done" {
		t.Errorf("units = %q, want [\"done\"] — the output written before the child exited was "+
			"lost when the pipes were closed", rep.Units)
	}
	// The harness's Timeout is 10s and the grandchild holds for 30s, so anything
	// near or past 10s means we waited on the pipe rather than on the command.
	if took > 6*time.Second {
		t.Errorf("reporting took %s: the worker waited on the grandchild, not on the command", took)
	}
}
