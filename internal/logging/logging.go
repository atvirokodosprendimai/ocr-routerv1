// Package logging builds the router's structured logger and the attributes it
// is safe to log.
//
// ⚠ ITS CENTRAL DESIGN IS A NEGATIVE CLAIM: no exported function here accepts a
// job parameter VALUE. ADR-0001 lets a client attach arbitrary query parameters
// to a job — a crawler worker takes `?url=…` — and a URL carries credentials
// often enough that treating it as safe is a decision to leak them eventually.
// So Job() takes the whole map and emits only its keys, and the rule is enforced
// by the absence of an argument rather than by asking every call site to
// remember.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
)

// Options configure the logger.
type Options struct {
	// Level is one of debug, info, warn, error.
	Level string
	// Format is json (default) or text.
	Format string
	// Out defaults to os.Stdout. Injectable so the tests can read what was
	// actually written — every test in this package asserts against real emitted
	// bytes rather than a mock, because what comes out is the thing under test.
	Out io.Writer
}

// New builds a logger.
//
// ⚠ An unrecognised level or format is an ERROR, never a silent fallback. An
// operator who typed `--log-level verbose`, got info-level logging and no
// complaint would have no way to discover the difference; failing the boot is
// the only outcome that tells them.
func New(o Options) (*slog.Logger, error) {
	lvl, err := parseLevel(o.Level)
	if err != nil {
		return nil, err
	}

	out := o.Out
	if out == nil {
		// stdout, not slog's own default of stderr: the router's existing startup
		// lines go to stdout, and splitting the output across two streams would
		// make a deployment that captures one lose half.
		out = os.Stdout
	}

	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(strings.TrimSpace(o.Format)) {
	case "", "json":
		return slog.New(slog.NewJSONHandler(out, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(out, opts)), nil
	default:
		return nil, fmt.Errorf("logging: unknown format %q (want json or text)", o.Format)
	}
}

// parseLevel maps a flag value to a slog level.
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: unknown level %q (want debug, info, warn or error)", s)
	}
}

// Nop returns a logger that writes nothing.
//
// It exists so consumers need no nil checks, on the same reasoning as the
// monitor package's nop counter: one default set once beats a guard at every
// call site, where forgetting one is a nil dereference in production.
func Nop() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// Job returns the attributes that describe a job, with its parameters REDACTED
// to their keys.
//
// ⚠ There is deliberately no variant taking values, and none should be added.
// The keys are safe because core.ValidParamKey already bounds them to
// ^[a-z][a-z0-9-]{0,31}$; the values have no constraint at all. Keys are sorted
// so two identical jobs produce identical lines and a diff of two runs shows
// real differences rather than Go's randomised map order.
func Job(id, userID, label string, params map[string]string) slog.Attr {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return slog.Group("job",
		slog.String("id", id),
		slog.String("user_id", userID),
		slog.String("label", label),
		slog.Any("params", keys),
	)
}

// Transition is the shape of one job state change, without its param values.
//
// It mirrors router.TransitionEvent rather than importing it, because importing
// upward would invert the dependency — router owns the event, this package owns
// how it is rendered. The composition root adapts one to the other.
type Transition struct {
	JobID    string
	UserID   string
	Label    string
	From     string
	To       string
	Attempt  int
	WorkerID string
	Actor    string
	Stage    int
	Params   map[string]string
	Reason   string
	// ExitCode is the forked command status on a failure line, nil when the
	// failure never reached one. Emitted only when present (ADR-0007).
	ExitCode *int
	InState  time.Duration
}

// LogTransition writes one transition line.
//
// ⚠ The params reach this function and are handed straight to Job(), which emits
// keys only. This is the single place in the process where a param map is turned
// into text, and it is why the redaction guarantee holds at the call site rather
// than only in the type.
func LogTransition(log *slog.Logger, t Transition) {
	attrs := []any{
		Job(t.JobID, t.UserID, t.Label, t.Params),
		slog.String("from", t.From),
		slog.String("to", t.To),
		slog.String("actor", t.Actor),
		slog.Int("attempt", t.Attempt),
		slog.Int("stage", t.Stage),
		// ★ The field that turns "the job died" into "it sat queued for forty
		// minutes and then failed in two seconds".
		slog.Duration("in_state", t.InState),
	}
	if t.WorkerID != "" {
		attrs = append(attrs, slog.String("worker_id", t.WorkerID))
	}
	if t.Reason != "" {
		attrs = append(attrs, slog.String("reason", t.Reason))
	}
	if t.ExitCode != nil {
		// Only when there IS one. A missing code emitted as 0 would read as a
		// clean exit in every log search that filters on it.
		attrs = append(attrs, slog.Int("exit_code", *t.ExitCode))
	}
	log.Info("transition", attrs...)
}
