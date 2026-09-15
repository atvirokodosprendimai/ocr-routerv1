package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/ratelimit"
)

// metricRequestsThrottled is duplicated from the monitor package's constant
// rather than imported, on the same reasoning as router's copies: importing
// upward would invert the dependency. The pair is pinned by a test.
const metricRequestsThrottled = "ocrr_requests_throttled_total"

type ctxKey int

const (
	// principalKey carries the authenticated caller down the handler chain.
	principalKey ctxKey = iota
	// principalSlotKey carries a POINTER the outer request logger placed on the
	// way in, for authenticate to fill on the way past. Two keys rather than one
	// because they travel in opposite directions.
	principalSlotKey
)

// role aliases keep the route table readable.
const (
	roleWorker = core.RoleWorker
	roleAdmin  = core.RoleAdmin
)

// authenticate resolves the bearer token and puts the principal in the context.
//
// It is applied to the whole group in New rather than per-route, so adding a
// route cannot accidentally add an unauthenticated one.
func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.deps.Identity.Authenticate(r.Context(), r.Header.Get("Authorization"), a.deps.Now())
		if err != nil {
			// The challenge header is what tells a well-behaved client HOW to
			// authenticate rather than merely that it failed.
			w.Header().Set("WWW-Authenticate", `Bearer realm="ocr-router"`)
			writeError(w, core.ErrUnauthorized)
			return
		}
		// Publish to the outer request logger too. The logger runs OUTSIDE this
		// middleware, so it holds the original *http.Request and can never see the
		// context created below — the derived request only travels downward.
		// Filling a slot the logger placed on the way in is what lets one log line
		// carry both the 401s (which have no principal) and the principal.
		if slot, ok := r.Context().Value(principalSlotKey).(*core.Principal); ok {
			*slot = p
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

// requireRole gates a route to one role.
func (a *API) requireRole(role core.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if principal(r).Role != role {
				writeError(w, core.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rateLimit refuses a caller that has exceeded its TOKEN's request rate.
//
// ⚠ IT MUST RUN INSIDE THE AUTHENTICATED GROUP, and that is the whole shape of
// the design rather than an implementation detail. It keys on TokenID, which
// only exists after authenticate has run. Applied outside it, every caller would
// present the zero Principal and therefore share ONE bucket — which looks like a
// working rate limiter right up to the second customer, and is then a global
// outage caused by whoever is busiest.
//
// Keying on the remote address was the alternative and is wrong in both
// directions here: ADR-0001 puts workers anywhere on the internet, several may
// share one NAT egress, and a leaked token moves between addresses freely.
func (a *API) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		lim := a.deps.Limits.For(p.Role)

		if !a.deps.Limiter.Allow(p.TokenID, lim) {
			secs := ratelimit.RetryAfterSeconds(a.deps.Limiter.RetryAfter(p.TokenID, lim))
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			a.deps.Counter.Inc(metricRequestsThrottled, map[string]string{"role": string(p.Role)})
			writeError(w, core.ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests emits one structured line per request.
//
// ⚠ IT IS THE OUTERMOST MIDDLEWARE, before authenticate, so it records the 401s
// and the 429s too. A request log covering only requests that authenticated
// successfully cannot answer "is someone hammering us with a revoked token",
// which is the question it is most needed for — and it would look entirely
// healthy while that was happening.
func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := a.deps.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// The slot authenticate fills if it gets that far. On a 401 it stays zero,
		// which is what the emptiness check below reads.
		var seen core.Principal
		r = r.WithContext(context.WithValue(r.Context(), principalSlotKey, &seen))

		next.ServeHTTP(rec, r)

		// The ROUTE PATTERN, never the raw path: /files/{id} as a raw path embeds
		// a job id in a field meant to identify a route and makes every line
		// unique, which defeats grouping for the same reason T11 bounds its
		// metric label values.
		route := r.URL.Path
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}

		attrs := []any{
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.Int("status", rec.status),
			slog.Duration("duration", a.deps.Now().Sub(start)),
		}
		// The principal is absent on a 401 by definition, and its zero value would
		// log three empty strings that read like a real anonymous caller.
		if seen.TokenID != "" {
			attrs = append(attrs,
				slog.String("user_id", seen.UserID),
				slog.String("token_id", seen.TokenID),
				slog.String("role", string(seen.Role)))
		}
		a.deps.Logger.Info("request", attrs...)
	})
}

// statusRecorder remembers the status code so the log line can report it.
//
// http.ResponseWriter offers no way to read back what was written, which is why
// every request logger in every Go codebase has one of these.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status, s.written = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer.
//
// ⚠ Without this the SSE handler silently stops streaming: wrapping a
// ResponseWriter hides its http.Flusher, every flush becomes a no-op, and events
// sit in a buffer until the connection closes. A request logger is exactly the
// kind of harmless-looking wrapper that breaks streaming this way.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController.
//
// ⚠ THIS IS NOT OPTIONAL, and its absence is a five-minute debugging session
// that looks like a broken stream rather than a broken wrapper. The SSE handlers
// clear their write deadline through http.NewResponseController, which walks
// Unwrap to find a writer that supports it. A wrapper without this method makes
// SetWriteDeadline return ErrNotSupported, the server's deadline stands, and
// every stream dies mid-session with nothing in the logs — the exact failure
// ADR-0001 warns about for WriteTimeout, reintroduced by a logger.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// principal returns the authenticated caller.
//
// It returns a zero Principal when absent rather than panicking. The zero value
// has an empty Role, which matches no real role, so a handler reached without
// the middleware fails CLOSED — it cannot be mistaken for an admin.
func principal(r *http.Request) core.Principal {
	p, _ := r.Context().Value(principalKey).(core.Principal)
	return p
}

// PrincipalFrom returns the authenticated caller for handlers mounted OUTSIDE
// this package but behind its middleware — the admin dashboard.
//
// It exists so there is exactly ONE context key for the principal. Two packages
// each defining their own unexported key would compile, never match, and hand
// every dashboard handler a zero Principal — which fails closed, so the symptom
// would be "the dashboard always says forbidden" rather than anything pointing
// at the cause.
func PrincipalFrom(r *http.Request) core.Principal { return principal(r) }

// Authenticator exposes the auth middleware so the dashboard can sit behind the
// same one rather than growing a second login.
func (a *API) Authenticator() func(http.Handler) http.Handler { return a.authenticate }
