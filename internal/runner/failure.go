package runner

import (
	"errors"
	"fmt"
	"strings"
)

// FailureKind names WHY a run failed, so a reader does not have to infer it from
// prose (ADR-0007).
type FailureKind string

const (
	// FailureExit is the only kind that HAS an exit code: the command ran to
	// completion and returned a non-zero status.
	FailureExit FailureKind = "exit"
	// FailureTimeout is the command killed at the deadline. It never exited.
	FailureTimeout FailureKind = "timeout"
	// FailureOutputLimit is the command exceeding --max-output. It never exited.
	FailureOutputLimit FailureKind = "output-limit"
	// FailureContract is stdout that did not satisfy the service's output
	// contract — ADR-0001's JSON unit list. The command may well have exited 0.
	FailureContract FailureKind = "contract"
)

// Failure is what a forked command did wrong, as fields rather than a sentence.
//
// ⚠ ExitCode IS A POINTER AND IS nil FOR EVERY KIND BUT FailureExit. A timeout
// and an output-limit trip never reached an exit status at all, and 0 is the
// code for SUCCESS — so an int would make "never exited" and "exited cleanly"
// the same value, which is the one comparison this type exists to keep possible.
type Failure struct {
	Kind     FailureKind
	ExitCode *int
	// Output is what the command actually printed, stderr first. Each stream is
	// bounded separately — see combineOutput.
	Output string
	// Detail is the runner's own explanation, for kinds where the failure is the
	// runner's judgement rather than the command's (a timeout, a broken
	// contract) and there is something to say beyond the streams.
	Detail string
}

func (f *Failure) Error() string {
	head := string(f.Kind)
	if f.Kind == FailureExit && f.ExitCode != nil {
		// The shape the runner has always rendered, kept verbatim: an operator
		// reading "exit status 3: …" today must not have to learn a new one.
		head = fmt.Sprintf("exit status %d", *f.ExitCode)
	}
	body := f.Detail
	if f.Output != "" {
		if body != "" {
			body += ": "
		}
		body += f.Output
	}
	if body == "" {
		return head
	}
	return head + ": " + body
}

// AsFailure unwraps a runner error into its structured form.
//
// It exists so callers ask the error what it IS rather than matching on its
// text — the whole point of ADR-0007 being that a fact inside a sentence is not
// a fact anything can act on.
func AsFailure(err error) (*Failure, bool) {
	var f *Failure
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// combineOutput renders what the command printed, stderr FIRST.
//
// ⚠ EACH STREAM KEEPS ITS OWN BUDGET, and that is not tidiness. With one shared
// budget a command that floods stdout crowds out stderr entirely, and stderr is
// where a well-behaved tool explains itself — so the noisy stream would reliably
// delete the useful one. stderr leads for the same reason: an operator reading a
// truncated cell should meet the likeliest answer first.
func combineOutput(stdout []byte, stderrText string) string {
	var b strings.Builder
	if s := strings.TrimSpace(stderrText); s != "" {
		b.WriteString("stderr: ")
		b.WriteString(clip(s, stderrTail))
	}
	if s := strings.TrimSpace(string(stdout)); s != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("stdout: ")
		b.WriteString(clip(s, stderrTail))
	}
	return b.String()
}

// clip bounds one stream, marking the cut so a truncated explanation is never
// mistaken for a complete one.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "… (truncated)"
}
