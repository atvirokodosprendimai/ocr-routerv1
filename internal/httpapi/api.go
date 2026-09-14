// Package httpapi is the wire boundary: the four job endpoints plus service
// discovery, authenticated by bearer token and routed by the caller's role.
package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// Deps is everything the API needs, named explicitly.
//
// A struct rather than variadic options so that a missing dependency is a
// COMPILE error at the composition root, not a nil pointer on the first request
// that happens to touch it.
type Deps struct {
	Identity *identity.Service
	Router   *router.Service
	Repo     *store.Repo
	Blobs    *blob.Store
	Bus      *bus.Bus

	// MaxUpload caps a client upload body.
	MaxUpload int64
	// PingInterval is the SSE keepalive cadence. Exposed so tests can shorten it
	// rather than wait fifteen seconds.
	PingInterval time.Duration
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// API holds the dependencies and the routes.
type API struct {
	deps Deps
	mux  *chi.Mux
}

// New builds the router.
//
// ⚠ THIS FUNCTION IS WHAT MAKES EVERY HANDLER REACHABLE. A handler that exists
// and is not mounted here is finished, tested, and called by nothing — the most
// common shipped defect this pipeline guards against. That is why the mutation
// recorded for this task deletes a ROUTE rather than a handler body, and why
// TestUnauthenticatedIsRejected walks the real route table instead of a list
// someone maintains by hand.
func New(deps Deps) *API {
	if deps.PingInterval == 0 {
		deps.PingInterval = 15 * time.Second
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.MaxUpload == 0 {
		deps.MaxUpload = 64 << 20
	}

	a := &API{deps: deps}
	r := chi.NewRouter()

	// Every route below sits inside the auth middleware. There is no
	// unauthenticated group here at all — /healthz is mounted by the monitoring
	// task onto the parent mux, deliberately outside this subtree.
	r.Group(func(r chi.Router) {
		r.Use(a.authenticate)

		r.Post("/upload", a.handleUpload)
		r.Get("/sse", a.handleSSE)
		r.Get("/files/{id}", a.handleFile)
		r.With(a.requireRole(roleWorker)).Post("/claim", a.handleClaim)
		r.Get("/services", a.handleServices)
	})

	a.mux = r
	return a
}

// ServeHTTP makes API an http.Handler.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// Routes exposes the mounted route table.
//
// It exists so tests can drive assertions from what is ACTUALLY mounted rather
// than from a hand-written list. A list someone maintains is a list that goes
// stale the first time a route is added in a hurry — and the route added in a
// hurry is exactly the one likely to be missing its guard.
func (a *API) Routes() []Route {
	var out []Route
	_ = chi.Walk(a.mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, Route{Method: method, Pattern: route})
		return nil
	})
	return out
}

// Route is one mounted method+pattern pair.
type Route struct {
	Method  string
	Pattern string
}
