package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/httpapi"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
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
	Close   func() error
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

	api := httpapi.New(httpapi.Deps{
		Identity:     ident,
		Router:       rt,
		Repo:         repo,
		Blobs:        blobs,
		Bus:          b,
		MaxUpload:    cfg.MaxUpload,
		PingInterval: cfg.PingInterval,
		Now:          time.Now,
	})

	mux := chi.NewRouter()
	mux.Mount("/", api)

	return &App{
		Handler: mux,
		Router:  rt,
		Ident:   ident,
		Repo:    repo,
		Bus:     b,
		Results: res,
		Blobs:   blobs,
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
			}
		}
	}()
}
