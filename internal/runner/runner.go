// Package runner forks a configured command for one job and parses its output.
//
// It knows nothing about OCR. It materialises an input, builds an argv, runs the
// command, and reads a JSON array of strings from stdout — which is what makes
// one worker binary serve any service the operator can express as a program.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Runner executes one service.
type Runner struct {
	// Cmd is the program to fork. Never a shell string.
	Cmd string
	// Timeout bounds one execution.
	Timeout time.Duration
	// MaxOutput caps stdout, so a runaway command cannot exhaust the worker's
	// memory before the timeout fires.
	MaxOutput int64
	// Env is the allow-listed environment passed to the child.
	Env []string
}

// stderrTail is how much of stderr is kept for the failure reason. Enough to
// carry a real message; short enough that a chatty tool cannot fill the database.
const stderrTail = 2000

// Run executes the job and returns its output units.
//
// Every failure mode here is a JOB failure, returned as an error for the caller
// to report — never a panic and never a worker exit. One malformed document must
// not take down a worker that is serving every other customer.
func (r Runner) Run(ctx context.Context, j Job) ([]string, error) {
	argv, err := BuildArgv(r.Cmd, j)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)

	// A clean, explicitly allow-listed environment. Two reasons: the child gets
	// nothing the operator did not intend, and — the important half — a job
	// parameter can never become an environment variable, so argv stays the ONE
	// channel by which client input reaches the process.
	cmd.Env = r.Env

	// ⚠ Put the child in its own PROCESS GROUP so the timeout can kill the whole
	// tree. Killing only the direct child leaves a grandchild holding the stdout
	// pipe, and the read below then blocks forever — the worker hangs with no
	// error, which is worse than the slow command it was trying to bound.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, remaining: r.MaxOutput}
	cmd.Stderr = &limitedWriter{w: &stderr, remaining: stderrTail}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q: %w", r.Cmd, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		killGroup(cmd)
		<-done // reap, so the process is not left a zombie
		return nil, fmt.Errorf("timed out after %s", r.Timeout)

	case err := <-done:
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return nil, fmt.Errorf("exit status %d: %s",
					exitErr.ExitCode(), strings.TrimSpace(stderr.String()))
			}
			return nil, err
		}
	}

	return parseUnits(stdout.Bytes(), stderr.String())
}

// parseUnits reads the command's contract: a JSON array of strings on stdout.
func parseUnits(out []byte, stderrText string) ([]string, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("produced no output on stdout: %s", strings.TrimSpace(stderrText))
	}

	var units []string
	if err := json.Unmarshal(trimmed, &units); err != nil {
		// Deliberately quote a short prefix. Naming what WAS produced is what
		// turns this from "it broke" into "your tool printed a log line where
		// the JSON array should be", which is the actual mistake people make.
		return nil, fmt.Errorf("stdout is not a JSON array of strings (got %q): %w",
			preview(trimmed), err)
	}
	return units, nil
}

func preview(b []byte) string {
	const n = 120
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// killGroup signals the child's whole process group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// The negative pid addresses the group. Fall back to the single process if
	// the group cannot be resolved, which is better than not killing at all.
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = cmd.Process.Kill()
}

// limitedWriter discards everything past a byte budget.
//
// It does not error on overflow: a command that prints too much has failed in a
// way the caller will discover when the truncated output does not parse, and
// returning an error from a writer mid-execution only complicates the exit path.
type limitedWriter struct {
	w         *bytes.Buffer
	remaining int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	if int64(len(p)) > l.remaining {
		l.w.Write(p[:l.remaining])
		l.remaining = 0
		return len(p), nil
	}
	l.w.Write(p)
	l.remaining -= int64(len(p))
	return len(p), nil
}
