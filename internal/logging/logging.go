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
