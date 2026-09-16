// Command client submits one document to the router and waits for its result.
//
// It blocks: a queued job behind a busy worker pool is supposed to take a long
// time, and a default timeout would turn a slow success into a spurious failure.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/atvirokodosprendimai/ocr-router/internal/client"
)

// Exit codes name an ACTION, not an error taxonomy.
//
// ⚠ A script branches on what to do NEXT. Retrying a dead job is pointless and
// retrying a rate-limited one is correct, and a single non-zero code leaves a
// caller unable to tell them apart.
const (
	// exitOK means the result was written.
	exitOK = 0
	// exitFix means a human must change something: a bad token, no credits, a
	// missing file, a malformed request. Retrying changes nothing.
	exitFix = 1
	// exitRetry means try again later: rate limited, router down, timed out.
	exitRetry = 2
	// exitJobFailed means the job RAN and did not produce a result.
	//
	// ⚠ This is the code that matters most. A client that wrote an empty file
	// and exited 0 because "the request succeeded" would turn a dead job into
	// silent data loss in whatever pipeline called it.
	exitJobFailed = 3
)

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

// run is main's body with its streams injected, so a test can drive the real
// command and read both of them.
func run(args []string, stdout, stderr io.Writer) int {
	cmd := newCLI(stdout, stderr)
	err := cmd.Run(context.Background(), args)
	if err == nil {
		return exitOK
	}

	var coded *codedError
	if errors.As(err, &coded) {
		if coded.msg != "" {
			fmt.Fprintln(stderr, "error:", coded.msg)
		}
		return coded.code
	}
	fmt.Fprintln(stderr, "error:", err)
	return exitFix
}

// codedError carries the exit code out through urfave's error return.
type codedError struct {
	code int
	msg  string
}

func (e *codedError) Error() string { return e.msg }

func newCLI(stdout, stderr io.Writer) *cli.Command {
	return &cli.Command{
		Name:  "client",
		Usage: "submit a document to the router and wait for the result",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "router", Required: true,
				Usage: "the router's base URL, e.g. https://ocr.example.com"},
			&cli.StringFlag{Name: "token",
				Usage:   "bearer token; ⚠ a token here is in your shell history and in ps",
				Sources: cli.EnvVars("OCRR_TOKEN")},
			&cli.StringFlag{Name: "input", Aliases: []string{"i"},
				Usage: "the document to submit; omit it for a params-only job"},
			&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Value: "-",
				Usage: "where to write the result; - is stdout"},
			&cli.StringFlag{Name: "label",
				Usage: "the service to use; the router's default when omitted"},
			&cli.StringSliceFlag{Name: "pipeline",
				Usage: "an ordered list of services; wins over --label"},
			&cli.StringSliceFlag{Name: "param",
				Usage: "a k=v parameter for the worker, repeatable"},
			&cli.BoolFlag{Name: "json",
				Usage: "write the result envelope instead of newline-joined text"},
			&cli.BoolFlag{Name: "raw",
				Usage: "the service returns opaque BYTES, written to -o unchanged. " +
					"The service must be marked raw by an administrator, or the upload is refused"},
			&cli.BoolFlag{Name: "quiet",
				Usage: "suppress progress; errors are still reported"},
			&cli.DurationFlag{Name: "timeout",
				Usage: "give up after this long; zero waits forever, which is the default " +
					"because a queued job is supposed to take a while"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			return submit(ctx, c, stdout, stderr)
		},
	}
}

func submit(ctx context.Context, c *cli.Command, stdout, stderr io.Writer) error {
	token := c.String("token")
	if token == "" {
		return &codedError{exitFix, "no token: pass --token or set OCRR_TOKEN"}
	}

	inputPath := c.String("input")
	params, err := parseParams(c.StringSlice("param"))
	if err != nil {
		return &codedError{exitFix, err.Error()}
	}
	if inputPath == "" && len(params) == 0 {
		// A params-only job is ADR-0001's crawler shape; with neither there is
		// nothing to submit at all.
		return &codedError{exitFix,
			"nothing to submit: pass --input FILE, or --param k=v for a job whose service " +
				"fetches its own input"}
	}

	outPath := c.String("output")
	// ⚠ BEFORE the upload. Collecting the result charges the credits, so an
	// unwritable destination found afterwards means the customer paid for a
	// result that goes nowhere.
	if err := probeDestination(outPath); err != nil {
		return &codedError{exitFix, err.Error()}
	}

	var body io.Reader
	var filename string
	if inputPath != "" {
		f, err := os.Open(inputPath)
		if err != nil {
			return &codedError{exitFix, err.Error()}
		}
		defer f.Close()
		body = f
		filename = pathBase(inputPath)
	}

	if d := c.Duration("timeout"); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	rep := newReporter(stderr, c.Bool("quiet"))
	res, err := client.Submit(ctx, client.Config{
		RouterURL: c.String("router"),
		Token:     token,
		// ⚠ No Timeout on the http.Client: the SSE stream is held open for the
		// life of the job, and a client-level timeout would abort it mid-wait.
		// The context above is what bounds the whole operation.
		HTTP: &http.Client{},
	}, client.Input{
		Filename: filename,
		Body:     body,
		Label:    c.String("label"),
		Pipeline: c.StringSlice("pipeline"),
		Params:   params,
		Raw:      c.Bool("raw"),
	}, rep.stage)
	if err != nil {
		rep.done()
		return classify(ctx, err)
	}

	if err := writeResult(stdout, outPath, res, c.Bool("json")); err != nil {
		// The result is already collected and CHARGED at this point, so this is
		// the worst place to fail. Say the job id so it is at least traceable.
		return &codedError{exitFix,
			fmt.Sprintf("job %s succeeded but its result could not be written: %v", res.JobID, err)}
	}
	return nil
}

// classify turns a submission error into an exit code.
//
// ⚠ This is the line that gives internal/client's typed errors their point.
// Collapsing it to one code would leave a script unable to tell a pointless
// retry from a correct one.
func classify(ctx context.Context, err error) error {
	var failed *client.FailedError
	if errors.As(err, &failed) {
		return &codedError{exitJobFailed, failed.Error()}
	}

	var retryable *client.RetryableError
	if errors.As(err, &retryable) {
		return &codedError{exitRetry, retryable.Error()}
	}

	// A timeout says nothing about whether the job will eventually succeed, so
	// it is retryable rather than a failure of the job.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &codedError{exitRetry, "timed out waiting for the job; it may still be running"}
	}
	if errors.Is(err, context.Canceled) {
		return &codedError{exitRetry, "cancelled"}
	}

	return &codedError{exitFix, err.Error()}
}

// parseParams turns repeated k=v flags into a map.
func parseParams(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for _, kv := range raw {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--param %q is not k=v", kv)
		}
		out[k] = v
	}
	return out, nil
}

func pathBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
