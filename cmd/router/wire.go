package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/httpapi"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/logging"
	"github.com/atvirokodosprendimai/ocr-router/internal/monitor"
	"github.com/atvirokodosprendimai/ocr-router/internal/ratelimit"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
	"github.com/atvirokodosprendimai/ocr-router/internal/web"
)

// Config is everything the binary needs to build itself.
type Config struct {
	Addr         string
	DBPath       string
	BlobDir      string
	ResultTTL    time.Duration
	Lease        time.Duration
	MaxAttempts  int
	AgingStep    time.Duration
	LabelGrace   time.Duration
	ReapInterval time.Duration
	MaxUpload    int64
	DefaultLabel string
	// PingInterval is the SSE keepalive cadence. Zero means the API's default.
	// It is configurable chiefly so the end-to-end test can observe a ping
	// without waiting fifteen seconds — a fence that runs three times per task
	// pays that wait nine times.
	PingInterval time.Duration
	// MetricsAddr is where the PRIVATE metrics listener binds. Loopback by
	// default: queue depths and throughput say how much work a customer pushes,
	// and the main listener faces the internet.
	MetricsAddr string
	// Version is reported by /healthz.
	Version string

	// Rate limits per role, in requests per second per TOKEN. Zero disables
	// limiting for that role, which is ADR-0002's operational rollback.
	RateClient float64
	RateWorker float64
	RateAdmin  float64
	// RateBurst is how many requests may arrive at once before the rate applies.
	RateBurst int
	// RateIdle is how long a silent token's bucket is kept. It bounds the
	// limiter's memory, which is otherwise one entry per token forever.
	RateIdle time.Duration

	// LogLevel and LogFormat configure the structured logger. An unrecognised
	// value fails the boot rather than falling back silently.
	LogLevel  string
	LogFormat string
}

// App is a fully constructed router: its handler, its collaborators, and the
// cleanup that releases them.
type App struct {
	Handler http.Handler
	Router  *router.Service
	Ident   *identity.Service
	Repo    *store.Repo
	Bus     *bus.Bus
	Results *results.Store
	Blobs   *blob.Store
	Monitor *monitor.Monitor
	Limiter *ratelimit.Limiter
	Logger  *slog.Logger
	Close   func() error
}

// routerLogger adapts router.TransitionEvent to the logging package.
//
// ⚠ IT IS THE ONLY PLACE the two meet, and it is why router depends on nothing
// above it. The param map crosses here and is handed to LogTransition, which
// emits keys only — so the redaction guarantee is enforced on the real path, not
// merely in a type nobody is obliged to use.
type routerLogger struct{ log *slog.Logger }

func (r routerLogger) Transition(e router.TransitionEvent) {
	logging.LogTransition(r.log, logging.Transition{
		JobID:    e.JobID,
		UserID:   e.UserID,
		Label:    e.Label,
		From:     string(e.From),
		To:       string(e.To),
		Attempt:  e.Attempt,
		WorkerID: e.WorkerID,
		Actor:    e.Actor,
		Stage:    e.Stage,
		Params:   e.Params,
		Reason:   e.Reason,
		InState:  e.InState,
	})
}

// buildApp constructs the whole dependency graph.
//
// ⚠ THIS IS THE COMPOSITION ROOT, AND IT EXISTS AS A FUNCTION SO THE TESTS CAN
// USE IT. A test that assembles its own graph proves things about that graph and
// nothing about the binary: every unit below can be correct while main.go
// forgets to start the reaper, or mounts nothing, or passes the read handle
// where the write handle belongs. That class of defect is invisible to every
// test in T2–T7, and this is the seam that makes it visible.
func buildApp(cfg Config) (*App, error) {
	// The logger is built FIRST and its failure is fatal: a misconfigured
	// --log-level must stop the boot rather than start a process whose logging
	// silently differs from what was asked for.
	log, err := logging.New(logging.Options{Level: cfg.LogLevel, Format: cfg.LogFormat})
	if err != nil {
		return nil, err
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	blobs, err := blob.New(cfg.BlobDir)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("opening blob store: %w", err)
	}

	repo := store.NewRepo(db)
	res := results.New(cfg.ResultTTL)
	b := bus.New()
	ident := identity.New(repo)
	rt := router.New(repo, blobs, res, b, router.Config{
		Lease:        cfg.Lease,
		MaxAttempts:  cfg.MaxAttempts,
		AgingStep:    cfg.AgingStep,
		LabelGrace:   cfg.LabelGrace,
		DefaultLabel: cfg.DefaultLabel,
	})

	// Leases and in-memory results died with the previous process, so anything
	// in flight has to go back to the queue before we start serving. Nothing was
	// charged, so this can only cost repeated work.
	if _, err := rt.RecoverOnBoot(context.Background(), time.Now()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("boot recovery: %w", err)
	}

	// The metrics registry is attached to the single writer, so the counters are
	// incremented at the same place the state actually changes. A counter nobody
	// increments is always zero, and always-zero reads exactly like healthy.
	reg := monitor.NewRegistry()
	rt.SetCounter(reg)
	// The same reasoning for the logger: a transition that is counted but not
	// logged leaves an operator with a number and no way to act on it.
	rt.SetLogger(routerLogger{log: log})

	limiter := ratelimit.New(time.Now, cfg.RateIdle)

	api := httpapi.New(httpapi.Deps{
		Identity:     ident,
		Router:       rt,
		Repo:         repo,
		Blobs:        blobs,
		Bus:          b,
		MaxUpload:    cfg.MaxUpload,
		PingInterval: cfg.PingInterval,
		Now:          time.Now,
		Limiter:      limiter,
		Limits: httpapi.RoleLimits{
			Client: ratelimit.Limit{RPS: cfg.RateClient, Burst: cfg.RateBurst},
			Worker: ratelimit.Limit{RPS: cfg.RateWorker, Burst: cfg.RateBurst},
			Admin:  ratelimit.Limit{RPS: cfg.RateAdmin, Burst: cfg.RateBurst},
		},
		Logger:  log,
		Counter: reg,
	})

	dash := web.New(web.Deps{
		Identity: ident, Router: rt, Repo: repo, Bus: b, Results: res,
		PingInterval: cfg.PingInterval, Now: time.Now,
	})
	mon := monitor.New(reg, monitor.Deps{
		Version: cfg.Version,
		BlobDir: cfg.BlobDir,
		PingDB: func(ctx context.Context) error {
			// Through the READ handle, deliberately: a health probe must never
			// be able to write.
			var one int
			return db.Read.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
		},
		QueueDepth:   repo.QueueDepthByLabel,
		OldestQueued: repo.OldestQueuedByLabel,
		JobStates: func(ctx context.Context) (map[string]int, error) {
			byState, err := repo.CountJobsByState(ctx)
			if err != nil {
				return nil, err
			}
			out := make(map[string]int, len(byState))
			for s, n := range byState {
				out[string(s)] = n
			}
			return out, nil
		},
		LiveLabels:      rt.AvailableLabels,
		WorkersFor:      func(label string) int { return b.Subscribers(bus.WorkerTopic(label)) },
		ResultsInMemory: res.Len,
		Now:             time.Now,
	})

	mux := chi.NewRouter()
	// ⚠ /healthz is mounted OUTSIDE the API's authenticated group, and is the
	// only unauthenticated route in the process. A load balancer and an
	// orchestrator probe cannot hold a bearer token.
	mux.Get("/healthz", mon.HealthHandler)
	// The dashboard is mounted behind the API's own authenticator, so there is
	// one authentication path in the process rather than two.
	dash.Mount(mux, api.Authenticator())
	mux.Mount("/", api)

	return &App{
		Handler: mux,
		Router:  rt,
		Ident:   ident,
		Repo:    repo,
		Bus:     b,
		Results: res,
		Blobs:   blobs,
		Monitor: mon,
		Limiter: limiter,
		Logger:  log,
		Close:   db.Close,
	}, nil
}

// StartReaper runs the clock-driven sweeps until ctx is cancelled.
//
// It is here rather than inside router.Service because a service that starts its
// own goroutine cannot be constructed in a test without also starting it — and
// the tests need to drive Reap directly, at a time of their choosing, instead of
// sleeping.
func (a *App) StartReaper(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A failed sweep must not kill the loop: the next tick retries,
				// and the alternative is a router that silently stops reaping
				// after one transient database error.
				_, _ = a.Router.Reap(ctx, time.Now())
				// ⚠ Limiter eviction rides THIS tick rather than a timer of its
				// own. The tick already exists, and a second timer is a second
				// goroutine to leak. Without this line the limiter's map grows
				// one entry per token forever, and no test inside
				// internal/ratelimit can see it missing.
				a.Limiter.EvictIdle()
			}
		}
	}()
}
