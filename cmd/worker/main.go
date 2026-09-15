// Command worker claims jobs for one service label and runs a configured
// program against them.
//
// Nothing about OCR is compiled in. The worker downloads an input, forks
// whatever --cmd names, and posts that command's stdout back — so the same
// binary is an OCR worker, an HTML stripper or a crawler depending on two flags.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/atvirokodosprendimai/ocr-router/internal/agent"
	"github.com/atvirokodosprendimai/ocr-router/internal/runner"
)

func main() {
	if err := newCLI().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newCLI() *cli.Command {
	return &cli.Command{
		Name:  "worker",
		Usage: "claim jobs for one service label and run a program against them",
		Description: `The configured program is invoked as:

    <cmd> [-i <input-file>] -o - [--<key> <value>]...

-i is present only when the job has a source file; a crawler-style job has
parameters instead. The program must print a JSON array of strings to stdout —
one element per output unit — and exit 0. Anything else is reported as a job
failure.

⚠ A parameter VALUE beginning with "-" may be read as a flag by your program.
Point --cmd at something that honours "--", or at a small wrapper script.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "router", Value: "http://localhost:8080",
				Usage: "router base URL"},
			&cli.StringFlag{Name: "token", Sources: cli.EnvVars("OCR_WORKER_TOKEN"),
				Usage: "worker bearer token", Required: true},
			&cli.StringFlag{Name: "label", Value: "ocr",
				Usage: "the ONE service label this process serves"},
			&cli.StringFlag{Name: "cmd", Required: true,
				Usage: "the program to fork for each job"},
			&cli.StringFlag{Name: "tmpdir", Value: os.TempDir(),
				Usage: "where inputs are materialised (a tmpfs mount is a good choice)"},
			&cli.IntFlag{Name: "slots", Value: 2,
				Usage: "concurrent subprocesses"},
			&cli.DurationFlag{Name: "timeout", Value: 5 * time.Minute,
				Usage: "per-job time limit"},
			&cli.IntFlag{Name: "max-output", Value: 64 << 20,
				Usage: "maximum bytes read from the program's stdout"},
		},
		Action: run,
	}
}

func run(ctx context.Context, c *cli.Command) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := buildRunner(c.String("cmd"), c.Duration("timeout"), int64(c.Int("max-output")))

	a := agent.New(agent.Config{
		RouterURL: c.String("router"),
		Token:     c.String("token"),
		Label:     c.String("label"),
		TmpDir:    c.String("tmpdir"),
		Slots:     int(c.Int("slots")),
	}, r)
	a.Log = func(format string, args ...any) {
		fmt.Printf(time.Now().Format(time.RFC3339)+" "+format+"\n", args...)
	}

	fmt.Printf("worker serving %q via %s (slots=%d cmd=%s)\n",
		c.String("label"), c.String("router"), c.Int("slots"), c.String("cmd"))

	return a.Run(ctx)
}

// buildRunner assembles the subprocess runner.
//
// It is a named function rather than a struct literal inside run() so the test
// can build the runner THE SAME WAY the binary does. A test that constructs its
// own runner proves nothing about the one this command actually uses — in
// particular, nothing about the environment allow-list.
func buildRunner(cmd string, timeout time.Duration, maxOutput int64) runner.Runner {
	return runner.Runner{
		Cmd:       cmd,
		Timeout:   timeout,
		MaxOutput: maxOutput,
		// An explicit allow-list, so the worker's own environment — which holds
		// its router token — is never handed to a service it runs.
		Env: allowedEnv(),
	}
}

// jobFor is the single-input shape used by the binary's own smoke test.
func jobFor(inputPath string) runner.Job {
	return runner.Job{InputPath: inputPath}
}

// allowedEnv is the environment a forked service receives.
//
// PATH so it can find its own helpers, HOME and TMPDIR because many tools need
// somewhere to write. Nothing else: not the worker's token, not whatever the
// operator happened to export in the shell that started it.
func allowedEnv() []string {
	var out []string
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"} {
		if v, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+v)
		}
	}
	return out
}
