package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCLIExposesEveryFlag is rung 3: a setting an operator cannot reach from the
// command line is not configuration.
func TestCLIExposesEveryFlag(t *testing.T) {
	cmd := newCLI()
	have := map[string]bool{}
	for _, f := range cmd.Flags {
		for _, n := range f.Names() {
			have[n] = true
		}
	}
	for _, want := range []string{
		"router", "token", "label", "cmd", "tmpdir", "slots", "timeout", "max-output",
	} {
		if !have[want] {
			t.Errorf("--%s is not exposed on the command line", want)
		}
	}
}

func TestTokenCanComeFromTheEnvironment(t *testing.T) {
	// A token on the command line lands in the process table and in shell
	// history, so reading it from the environment must be possible.
	cmd := newCLI()
	for _, f := range cmd.Flags {
		for _, n := range f.Names() {
			if n != "token" {
				continue
			}
			if !strings.Contains(strings.Join(f.(interface{ GetEnvVars() []string }).GetEnvVars(), ","), "OCR_WORKER_TOKEN") {
				t.Error("--token cannot be supplied via OCR_WORKER_TOKEN; a token on the " +
					"command line is visible in the process table")
			}
			return
		}
	}
	t.Error("no --token flag found")
}

func TestAllowedEnvExcludesEverythingElse(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("OCR_WORKER_TOKEN", "super-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "also-secret")

	env := allowedEnv()
	joined := strings.Join(env, "\n")

	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Error("PATH is not passed through; a service could not find its own helpers")
	}
	// ⚠ The worker's ROUTER TOKEN is the thing that must never reach a forked
	// service: the service is whatever program the operator configured, and it
	// has no business being able to act as this worker.
	if strings.Contains(joined, "super-secret") {
		t.Error("the worker's router token was passed to the forked service")
	}
	if strings.Contains(joined, "also-secret") {
		t.Error("an unrelated secret from the worker's environment reached the forked service " +
			"— the environment must be an allow-list, not a filter")
	}
}

func TestUsageDocumentsTheSubprocessContract(t *testing.T) {
	// Rung 3 again: the contract a --cmd must satisfy is not inferable from the
	// flag names, so it has to be in --help.
	desc := newCLI().Description
	for _, want := range []string{"-i", "-o -", "JSON array of strings", "exit 0"} {
		if !strings.Contains(desc, want) {
			t.Errorf("--help does not mention %q — an operator cannot write a conforming "+
				"program from the flag names alone", want)
		}
	}
	// And the residual risk is stated where the person configuring --cmd sees it.
	if !strings.Contains(desc, `"-"`) && !strings.Contains(desc, "--") {
		t.Error("--help does not warn that a value beginning with '-' may be read as a flag " +
			"by the configured program")
	}
}

// TestWorkerRunsAgainstARealScript is the smallest end-to-end shape this binary
// owns: build the runner exactly as run() does, and execute a real program.
func TestWorkerRunsAgainstARealScript(t *testing.T) {
	dir := t.TempDir()
	svc := filepath.Join(dir, "svc.sh")
	body := "#!/bin/sh\n" + `
while [ $# -gt 0 ]; do case "$1" in -i) f="$2"; shift 2 ;; *) shift ;; esac; done
printf '["%s"]' "$(cat "$f")"
`
	if err := os.WriteFile(svc, []byte(body), 0o700); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	input := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(input, []byte("hello"), 0o600); err != nil {
		t.Fatalf("writing input: %v", err)
	}

	r := buildRunner(svc, 5*time.Second, 1<<20, false)
	units, err := r.Run(context.Background(), jobFor(input))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(units) != 1 || units[0] != "hello" {
		t.Errorf("units = %v", units)
	}
}
