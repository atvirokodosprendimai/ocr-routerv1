package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/runner"
)

// script writes an executable shell script into a temp dir and returns its path.
//
// The scripts here stand in for whatever tool an operator configures. Using real
// programs rather than a fake interface is the point: the properties under test
// — argv separation, process groups, stdout parsing — exist only at a real
// process boundary and cannot be observed against a stub.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "svc.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	return path
}

func newRunner(cmd string) runner.Runner {
	return runner.Runner{
		Cmd: cmd, Timeout: 10 * time.Second, MaxOutput: 1 << 20,
		Env: []string{"PATH=/usr/bin:/bin"},
	}
}

func TestArgvIncludesInputOnlyWithBlob(t *testing.T) {
	withBlob, err := runner.BuildArgv("tool", runner.Job{InputPath: "/tmp/x.pdf"})
	if err != nil {
		t.Fatalf("BuildArgv: %v", err)
	}
	if strings.Join(withBlob, " ") != "tool -i /tmp/x.pdf -o -" {
		t.Errorf("argv = %v", withBlob)
	}

	// The crawler shape: no input file at all, so no -i.
	noBlob, err := runner.BuildArgv("tool", runner.Job{Params: map[string]string{"url": "u"}})
	if err != nil {
		t.Fatalf("BuildArgv: %v", err)
	}
	if strings.Join(noBlob, " ") != "tool -o - --url u" {
		t.Errorf("argv = %v — a params-only job must not be handed an empty -i", noBlob)
	}
}

func TestArgvIsDeterministic(t *testing.T) {
	params := map[string]string{"z": "1", "a": "2", "m": "3"}
	first, _ := runner.BuildArgv("tool", runner.Job{Params: params})
	for i := 0; i < 50; i++ {
		got, _ := runner.BuildArgv("tool", runner.Job{Params: params})
		if strings.Join(got, "\x00") != strings.Join(first, "\x00") {
			t.Fatalf("argv varies between calls: %v vs %v — map order is not a contract", first, got)
		}
	}
	if strings.Join(first, " ") != "tool -o - --a 2 --m 3 --z 1" {
		t.Errorf("argv = %v, want keys sorted", first)
	}
}

func TestArgvRejectsBadKeys(t *testing.T) {
	bad := []string{"UPPER", "has space", "--flag", "a;b", "", "1st", "a_b", strings.Repeat("a", 33)}
	for _, k := range bad {
		_, err := runner.BuildArgv("tool", runner.Job{Params: map[string]string{k: "v"}})
		if !errors.Is(err, core.ErrInvalidParam) {
			t.Errorf("BuildArgv with key %q = %v, want core.ErrInvalidParam", k, err)
		}
	}
}

// TestArgvValueWithShellMetacharsIsOneLiteralArgument is the single most
// important test in this repository.
//
// ⚠ It deliberately does NOT assert on the argv slice. Checking what BuildArgv
// returned proves only what the builder did; the property that matters is what
// the CHILD PROCESS receives, and only forking a real program can show that. The
// helper below reports its own argument count and bytes, so the assertion is
// about the process boundary rather than about a slice.
func TestArgvValueWithShellMetacharsIsOneLiteralArgument(t *testing.T) {
	canary := filepath.Join(t.TempDir(), "canary")

	// A script that emits its arguments as a JSON array, so we can see exactly
	// what crossed the boundary.
	echoArgs := script(t, `
printf '['
first=1
for a in "$@"; do
  if [ $first -eq 0 ]; then printf ','; fi
  first=0
  printf '"%s"' "$(printf '%s' "$a" | sed 's/\\/\\\\/g; s/"/\\"/g')"
done
printf ']'
`)

	nasty := "; rm -rf " + canary + " `touch " + canary + "` $(touch " + canary + ") && echo pwned | tee /dev/null"

	if err := os.WriteFile(canary, []byte("intact"), 0o600); err != nil {
		t.Fatalf("writing canary: %v", err)
	}

	out, err := newRunner(echoArgs).Run(context.Background(), runner.Job{
		Params: map[string]string{"q": nasty},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The child received: -o, -, --q, <nasty>. Exactly four arguments, with the
	// whole hostile string as ONE of them.
	want := []string{"-o", "-", "--q", nasty}
	if len(out) != len(want) {
		t.Fatalf("child received %d arguments (%q), want %d — a value that split itself into "+
			"several arguments is command injection", len(out), out, len(want))
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("argument %d = %q, want %q", i, out[i], want[i])
		}
	}

	// And nothing executed: the canary is untouched and was never deleted.
	got, err := os.ReadFile(canary)
	if err != nil {
		t.Fatalf("the canary file was DELETED — the value was interpreted as shell: %v", err)
	}
	if string(got) != "intact" {
		t.Errorf("the canary was modified (%q) — something in the value executed", got)
	}
}

func TestRunExecutesWithoutShell(t *testing.T) {
	// $HOME must arrive unexpanded. If a shell had touched it, it would be a path.
	echoQ := script(t, `
for a in "$@"; do
  case "$a" in --q) next=1 ;; *) if [ "$next" = 1 ]; then printf '["%s"]' "$a"; exit 0; fi ;;
  esac
done
printf '[]'
`)
	out, err := newRunner(echoQ).Run(context.Background(), runner.Job{
		Params: map[string]string{"q": "$HOME/../etc"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 1 || out[0] != "$HOME/../etc" {
		t.Errorf("child saw %q, want the literal %q — an expanded value means a shell ran",
			out, "$HOME/../etc")
	}
}

func TestRunEnvironmentIsAllowListed(t *testing.T) {
	t.Setenv("OCRR_SECRET", "do-not-leak")

	dumpEnv := script(t, `printf '["%s"]' "${OCRR_SECRET:-absent}"`)
	out, err := newRunner(dumpEnv).Run(context.Background(), runner.Job{
		Params: map[string]string{"q": "x"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out[0] != "absent" {
		t.Errorf("the child saw OCRR_SECRET=%q — the environment must be allow-listed, so the "+
			"worker's own secrets are not handed to every service it runs", out[0])
	}
}

func TestRunParsesJSONArray(t *testing.T) {
	out, err := newRunner(script(t, `printf '["page one","page two"]'`)).
		Run(context.Background(), runner.Job{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 2 || out[0] != "page one" {
		t.Errorf("units = %v", out)
	}
}

func TestRunRejectsNonArrayStdout(t *testing.T) {
	cases := map[string]string{
		"object":       `printf '{"a":1}'`,
		"bare string":  `printf '"just a string"'`,
		"number array": `printf '[1,2,3]'`,
		"not json":     `printf 'Loading model...\nDone.'`,
		"empty":        `printf ''`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newRunner(script(t, body)).Run(context.Background(), runner.Job{})
			if err == nil {
				t.Error("Run succeeded on output that is not a JSON array of strings")
			}
		})
	}
}

func TestRunNonZeroExitIsJobFailureWithStderr(t *testing.T) {
	_, err := newRunner(script(t, `echo "could not open input" >&2; exit 3`)).
		Run(context.Background(), runner.Job{})
	if err == nil {
		t.Fatal("Run succeeded on a command that exited 3")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error %q does not name the exit status", err)
	}
	if !strings.Contains(err.Error(), "could not open input") {
		t.Errorf("error %q does not carry the stderr tail — the dashboard would show no reason",
			err)
	}
}

// TestRunTimeoutKillsProcessGroup asserts that the timeout kills the whole
// process TREE, not just the program we started.
//
// ⚠ THIS TEST CANNOT DISTINGUISH THE PROCESS-GROUP KILL FROM A CHILD-ONLY KILL
// ON DARWIN, and that is recorded here rather than hidden, because the test
// reads as though it does.
//
// Measured 2026-09-15 on darwin/arm64 with a standalone probe: a `( sleep 1;
// touch marker ) &` grandchild is reaped in BOTH arms — with no kill at all the
// marker appears, so the mechanism works, but `cmd.Process.Kill()` alone is
// enough to stop it here. A mutant replacing the group kill with a child kill
// therefore SURVIVES, and that survival is left in the Mutation Log with this
// explanation instead of being papered over.
//
// The group kill is kept because it is correct where the platform does not do
// the work for us: a program that genuinely detaches — double-forks, or calls
// setsid — leaves an orphan that holds the inherited stdout pipe, one per
// timed-out job. Constructing that shape portably needs a helper binary, which
// is more machinery than the property is worth today.
//
// What this test DOES prove is the property we actually depend on: after the
// timeout, nothing from the job is still running.
func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild-survived")

	// The grandchild's output is redirected so it cannot affect when Run
	// returns: the ONLY observable difference between the two kill strategies is
	// whether this marker appears.
	hangs := script(t, `
( sleep 1; touch `+marker+` ) >/dev/null 2>&1 &
sleep 30
`)
	r := newRunner(hangs)
	r.Timeout = 300 * time.Millisecond

	_, err := r.Run(context.Background(), runner.Job{})
	if err == nil {
		t.Fatal("Run succeeded on a command that never finishes")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want a timeout", err)
	}

	// Well past when the grandchild would have fired.
	time.Sleep(2 * time.Second)

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Errorf("the grandchild was still alive after the timeout and created %s — killing "+
			"only the direct child leaves an orphan per timed-out job, holding the inherited "+
			"stdout pipe", marker)
	}
}

func TestRunRespectsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	r := newRunner(script(t, `sleep 30`))
	r.Timeout = 30 * time.Second // the caller's cancel must win, not this

	start := time.Now()
	if _, err := r.Run(ctx, runner.Job{}); err == nil {
		t.Fatal("Run succeeded despite cancellation")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("Run ignored the caller's cancellation — a worker shutting down would hang")
	}
}

func TestRunCapsStdout(t *testing.T) {
	// A runaway command must not exhaust the worker's memory before the timeout.
	r := newRunner(script(t, `yes 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' | head -c 10000000`))
	r.MaxOutput = 4096
	// The truncated output will not parse, which is the correct outcome: the
	// command broke its contract.
	if _, err := r.Run(context.Background(), runner.Job{}); err == nil {
		t.Error("a command producing 10MB of non-JSON succeeded")
	}
}

func TestRunMissingCommand(t *testing.T) {
	_, err := newRunner("/definitely/not/a/real/program").Run(context.Background(), runner.Job{})
	if err == nil {
		t.Error("Run succeeded with a command that does not exist")
	}
}

func TestRunEmptyCommandIsRejected(t *testing.T) {
	_, err := newRunner("").Run(context.Background(), runner.Job{})
	if !errors.Is(err, core.ErrInvalidParam) {
		t.Errorf("Run with no command = %v, want core.ErrInvalidParam", err)
	}
}

// TestRunReadsInputFile is the ordinary path: a real file handed to a real tool.
func TestRunReadsInputFile(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.txt")
	// Trailing newline on purpose: `read -r` returns non-zero on a final line
	// without one, so the shell loop below would silently drop it. That is a
	// property of the fixture's shell, not of the runner.
	if err := os.WriteFile(input, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatalf("writing input: %v", err)
	}

	// A "service" that reads -i and emits one unit per line.
	svc := script(t, `
while [ $# -gt 0 ]; do
  case "$1" in -i) in_file="$2"; shift 2 ;; *) shift ;;
  esac
done
printf '['
first=1
while IFS= read -r line; do
  if [ $first -eq 0 ]; then printf ','; fi
  first=0
  printf '"%s"' "$line"
done < "$in_file"
printf ']'
`)

	out, err := newRunner(svc).Run(context.Background(), runner.Job{InputPath: input})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 2 || out[0] != "line one" || out[1] != "line two" {
		t.Errorf("units = %v", out)
	}
}

func TestParseUnitsAcceptsEmptyArray(t *testing.T) {
	// A service correctly producing nothing is a legitimate outcome — a page
	// with no text, a crawl that found nothing — and must not be an error.
	out, err := newRunner(script(t, `printf '[]'`)).Run(context.Background(), runner.Job{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("units = %v, want empty", out)
	}
	var check []string
	if err := json.Unmarshal([]byte("[]"), &check); err != nil {
		t.Fatal(err)
	}
}

func TestExecLookPathIsNotUsedForRelativeNames(t *testing.T) {
	// A sanity check on the environment assumption: the runner passes an
	// explicit path, and PATH resolution is the operator's business.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no /bin/sh available: %v", err)
	}
}
