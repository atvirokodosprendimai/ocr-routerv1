package runner_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/runner"
)

// TestFailureKeepsStdout is the defect ADR-0007 was opened for.
//
// The explanation was CAPTURED and thrown away: exec buffers both streams and
// the exit-status message interpolated only stderr, so a tool that fails and
// says why on stdout produced "exit status 1: " and nothing after the colon.
func TestFailureKeepsStdout(t *testing.T) {
	r := newRunner(script(t, `printf 'cannot open the input file'; exit 1`))

	_, err := r.Run(context.Background(), runner.Job{ID: "j1"})
	if err == nil {
		t.Fatal("a command exiting 1 succeeded")
	}
	if !strings.Contains(err.Error(), "cannot open the input file") {
		t.Errorf("the command's own explanation was lost: %v", err)
	}
}

// TestFailureCarriesTheExitCodeAsAValue: a FIELD, not a substring.
func TestFailureCarriesTheExitCodeAsAValue(t *testing.T) {
	r := newRunner(script(t, `exit 3`))

	_, err := r.Run(context.Background(), runner.Job{ID: "j1"})
	f, ok := runner.AsFailure(err)
	if !ok {
		t.Fatalf("err is not a *runner.Failure: %#v", err)
	}
	if f.Kind != runner.FailureExit {
		t.Errorf("Kind = %q, want %q", f.Kind, runner.FailureExit)
	}
	if f.ExitCode == nil || *f.ExitCode != 3 {
		t.Errorf("ExitCode = %v, want 3 — a code inside a message cannot be filtered on", f.ExitCode)
	}
}

// TestTimeoutHasNoExitCode: nil, never 0.
func TestTimeoutHasNoExitCode(t *testing.T) {
	r := newRunner(script(t, `sleep 5`))
	r.Timeout = 100 * time.Millisecond

	_, err := r.Run(context.Background(), runner.Job{ID: "j1"})
	f, ok := runner.AsFailure(err)
	if !ok {
		t.Fatalf("err is not a *runner.Failure: %#v", err)
	}
	if f.Kind != runner.FailureTimeout {
		t.Errorf("Kind = %q, want %q", f.Kind, runner.FailureTimeout)
	}
	if f.ExitCode != nil {
		t.Errorf("a timeout reported ExitCode = %d, want nil — it never exited, and 0 would read "+
			"as a clean exit", *f.ExitCode)
	}
}

// TestOutputLimitHasNoExitCode, for the same reason.
func TestOutputLimitHasNoExitCode(t *testing.T) {
	r := newRunner(script(t, `head -c 100000 /dev/zero`))
	r.MaxOutput = 1024

	_, err := r.Run(context.Background(), runner.Job{ID: "j1"})
	f, ok := runner.AsFailure(err)
	if !ok {
		t.Fatalf("err is not a *runner.Failure: %#v", err)
	}
	if f.Kind != runner.FailureOutputLimit || f.ExitCode != nil {
		t.Errorf("Kind = %q, ExitCode = %v; want %q and nil", f.Kind, f.ExitCode, runner.FailureOutputLimit)
	}
}

// TestEachStreamIsBounded: one chatty stream must not crowd out the other's
// explanation, which a single shared budget allows.
func TestEachStreamIsBounded(t *testing.T) {
	r := newRunner(script(t, `head -c 50000 /dev/zero; printf 'THE REASON' >&2; exit 1`))
	r.MaxOutput = 1 << 20 // well above the flood, so the limit under test is the FAILURE budget

	_, err := r.Run(context.Background(), runner.Job{ID: "j1"})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "THE REASON") {
		t.Error("a flood on stdout crowded out stderr's explanation — each stream needs its own " +
			"budget, or the useful half is lost to the noisy one")
	}
	if len(err.Error()) > 8000 {
		t.Errorf("the failure message is %d bytes; each stream is supposed to be bounded", len(err.Error()))
	}
}
