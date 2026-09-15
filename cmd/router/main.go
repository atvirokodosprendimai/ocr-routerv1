// Command router is the OCR router: it accepts jobs from customers, hands them
// to workers by label, and meters the results.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
)

func main() {
	if err := newCLI().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newCLI() *cli.Command {
	return &cli.Command{
		Name:  "router",
		Usage: "route jobs to workers and meter the results",
		Flags: configFlags(),
		Commands: []*cli.Command{
			{
				Name:  "admin",
				Usage: "administrative commands",
				Commands: []*cli.Command{
					{
						Name:  "bootstrap",
						Usage: "create the first administrator and print its token",
						Flags: append(configFlags(),
							&cli.StringFlag{
								Name:     "email",
								Usage:    "the administrator's email address",
								Required: true,
							},
							&cli.StringFlag{
								Name: "password",
								Usage: "dashboard password; omit to be prompted. " +
									"⚠ a password given here is in your shell history and in ps",
							}),
						Action: runBootstrap,
					},
					{
						Name:  "set-password",
						Usage: "set or change an administrator's dashboard password",
						Flags: append(configFlags(),
							&cli.StringFlag{
								Name:     "email",
								Usage:    "the administrator's email address",
								Required: true,
							},
							&cli.StringFlag{
								Name: "password",
								Usage: "the new password; omit to be prompted. " +
									"⚠ a password given here is in your shell history and in ps",
							}),
						Action: runSetPassword,
					},
				},
			},
		},
		Action: runServe,
	}
}

// configFlags are shared by serve and by the admin commands, because both have
// to open the same database and blob directory.
func configFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "addr", Value: ":8080", Usage: "listen address"},
		&cli.StringFlag{Name: "db", Value: "ocr-router.db", Usage: "SQLite database path"},
		&cli.StringFlag{Name: "blobs", Value: "blobs", Usage: "directory for source files"},
		&cli.DurationFlag{Name: "result-ttl", Value: time.Hour,
			Usage: "how long an uncollected result is kept in memory before its job is requeued"},
		&cli.DurationFlag{Name: "lease", Value: 5 * time.Minute,
			Usage: "how long a worker holds a job before the reaper takes it back"},
		&cli.IntFlag{Name: "max-attempts", Value: 3,
			Usage: "attempts before a job is abandoned as dead"},
		&cli.DurationFlag{Name: "aging-step", Value: time.Minute,
			Usage: "waiting time that buys one point of effective priority"},
		&cli.DurationFlag{Name: "label-grace", Value: 5 * time.Minute,
			Usage: "how long a service stays available after its last worker disconnects"},
		&cli.DurationFlag{Name: "reap-interval", Value: 30 * time.Second,
			Usage: "how often expired leases, deadlines and results are swept"},
		&cli.IntFlag{Name: "max-upload", Value: 64 << 20, Usage: "maximum upload size in bytes"},
		&cli.StringFlag{Name: "metrics-addr", Value: "127.0.0.1:9090",
			Usage: "PRIVATE listener for /metrics; loopback by default because queue depth and throughput are commercially sensitive"},
		&cli.StringFlag{Name: "default-label", Value: "ocr",
			Usage: "the service a client gets when it names none"},

		// Rate limits are an ABUSE CEILING, not a quota: they sit far above any
		// legitimate use, and 0 on any of them disables limiting for that role —
		// which is the operational rollback, available without a redeploy.
		&cli.FloatFlag{Name: "rate-client", Value: 10,
			Usage: "client requests per second per token (0 = unlimited)"},
		&cli.FloatFlag{Name: "rate-worker", Value: 30,
			Usage: "worker requests per second per token (0 = unlimited) — above the poll rate"},
		&cli.FloatFlag{Name: "rate-admin", Value: 30,
			Usage: "admin requests per second per token (0 = unlimited)"},
		&cli.IntFlag{Name: "rate-burst", Value: 20,
			Usage: "requests allowed at once before the rate applies"},
		&cli.DurationFlag{Name: "rate-idle", Value: 10 * time.Minute,
			Usage: "how long a silent token's bucket is kept before it is evicted"},

		&cli.StringFlag{Name: "log-level", Value: "info",
			Usage: "debug, info, warn or error"},
		&cli.StringFlag{Name: "log-format", Value: "json",
			Usage: "json or text"},

		&cli.BoolFlag{Name: "insecure-cookies", Value: false,
			Usage: "DEVELOPMENT ONLY: drop Secure from the session cookie so you can sign in " +
				"over plain http://localhost. The cookie then travels in clear text — never " +
				"set this on anything reachable from a network"},
	}
}

func configFrom(c *cli.Command) Config {
	return Config{
		Addr:            c.String("addr"),
		DBPath:          c.String("db"),
		BlobDir:         c.String("blobs"),
		ResultTTL:       c.Duration("result-ttl"),
		Lease:           c.Duration("lease"),
		MaxAttempts:     c.Int("max-attempts"),
		AgingStep:       c.Duration("aging-step"),
		LabelGrace:      c.Duration("label-grace"),
		ReapInterval:    c.Duration("reap-interval"),
		MaxUpload:       int64(c.Int("max-upload")),
		DefaultLabel:    c.String("default-label"),
		MetricsAddr:     c.String("metrics-addr"),
		Version:         version,
		RateClient:      c.Float("rate-client"),
		RateWorker:      c.Float("rate-worker"),
		RateAdmin:       c.Float("rate-admin"),
		RateBurst:       c.Int("rate-burst"),
		RateIdle:        c.Duration("rate-idle"),
		LogLevel:        c.String("log-level"),
		LogFormat:       c.String("log-format"),
		InsecureCookies: c.Bool("insecure-cookies"),
	}
}

func runServe(ctx context.Context, c *cli.Command) error {
	cfg := configFrom(c)

	app, err := buildApp(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	app.StartReaper(ctx, cfg.ReapInterval)

	// The metrics endpoint listens separately, so exposing it is a deliberate
	// act in the operator's proxy rather than a consequence of running the
	// router at all.
	metricsAddr, shutdownMetrics, err := app.Monitor.Serve(ctx, cfg.MetricsAddr)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancelMetrics := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelMetrics()
		_ = shutdownMetrics(shutdownCtx)
	}()
	// The RESOLVED address, not the flag: an operator reading ":9090" from the log
	// cannot tell what it actually bound to.
	fmt.Printf("metrics on http://%s/metrics (private)\n", metricsAddr)

	// ⚠ Loud, on stdout, on every boot. A dangerous flag whose danger is only
	// documented in --help is a flag somebody sets once for local work and never
	// notices again — so the running process says it out loud instead.
	if cfg.InsecureCookies {
		fmt.Println("⚠ --insecure-cookies is SET: the session cookie has no Secure attribute " +
			"and travels in clear text. Development only — never on anything reachable from a " +
			"network.")
	}

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: app.Handler,
		// ⚠ WriteTimeout is deliberately ZERO. A non-zero value applies to the
		// whole response, which for an SSE stream means the connection dies
		// mid-session. The stream handler also clears its own deadline, so this
		// is belt and braces — a later operator adding a WriteTimeout here for
		// good reasons must not silently break every stream.
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() {
		fmt.Printf("router listening on %s (db=%s blobs=%s)\n", cfg.Addr, cfg.DBPath, cfg.BlobDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Stop accepting, then give open streams a moment to notice their context
	// is done and return. In-memory results are lost by design; their jobs go
	// back to the queue on the next boot.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fmt.Println("shutting down")
	return srv.Shutdown(shutdownCtx)
}

func runBootstrap(ctx context.Context, c *cli.Command) error {
	app, err := buildApp(configFrom(c))
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()

	user, token, err := app.Ident.Bootstrap(ctx, c.String("email"), time.Now())
	if err != nil {
		return err
	}

	// ⚠ The password is set AFTER the account exists, so a failure here has to be
	// reported WITH the token: an administrator with a token and no password is a
	// working account that cannot reach the dashboard, and the operator needs to
	// know which half succeeded rather than being told only that something broke.
	//
	// ErrNoPasswordSource is the one outcome that is not a failure. It means
	// nobody supplied a password and there was no terminal to ask at, which
	// leaves the account token-only — exactly how every account behaved before
	// ADR-0003.
	password, perr := resolvePassword(c, "Dashboard password for "+user.Email)
	switch {
	case errors.Is(perr, ErrNoPasswordSource):
		// Token-only. Nothing to do.
	case perr != nil:
		fmt.Printf("administrator created: %s (%s)\n", user.Email, user.ID)
		fmt.Printf("token: %s\n", token)
		return fmt.Errorf("the account exists and its token is above, but no password was set: %w", perr)
	default:
		if err := app.Ident.SetPassword(ctx, user.Email, password, time.Now()); err != nil {
			fmt.Printf("administrator created: %s (%s)\n", user.Email, user.ID)
			fmt.Printf("token: %s\n", token)
			return fmt.Errorf("the account exists and its token is above, but the password was refused: %w", err)
		}
	}

	// Printed exactly once, and recoverable from nowhere afterwards.
	fmt.Printf("administrator created: %s (%s)\n", user.Email, user.ID)
	fmt.Printf("token: %s\n", token)
	fmt.Println("\nThis token is shown once and is not stored in recoverable form.")
	fmt.Println("Save it now; if it is lost, create another administrator with a new email.")
	return nil
}

// version is reported by /healthz. A build stamps it with -ldflags; the default
// says plainly that nobody did, which is more useful than a plausible-looking
// fake number.
var version = "dev"
