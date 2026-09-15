// Package web is the admin dashboard: server-rendered templ, live over SSE.
//
// It shares the API's authentication rather than having its own, and is gated to
// administrators. Every action returns 200 with an HTML fragment; the browser
// holds essentially no state.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/httpapi"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/ratelimit"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
	"github.com/atvirokodosprendimai/ocr-router/internal/web/views"
)

// Deps is what the dashboard needs.
type Deps struct {
	Identity *identity.Service
	Router   *router.Service
	Repo     *store.Repo
	Bus      *bus.Bus
	Results  *results.Store

	PingInterval time.Duration
	Now          func() time.Time

	// Limiter throttles login attempts. Nil means no throttling, which keeps
	// every test written before ADR-0003 working — and is why the login handler
	// checks it rather than relying on a nop.
	//
	// ⚠ It is keyed on the EMAIL here, not on the token: a login has no
	// credential yet, so there is nothing else bounded to key on. Not the IP —
	// ADR-0001 puts callers behind NATs.
	Limiter *ratelimit.Limiter

	// InsecureCookies drops the Secure attribute and allows sign-in over plain
	// HTTP. ⚠ FOR LOCAL DEVELOPMENT ONLY.
	//
	// It exists because without it there is NO way to use the dashboard on
	// http://localhost, which made the first run of the feature impossible —
	// the login page could only tell the operator to go and get TLS. Default
	// false, so the safe behaviour is what you get by not thinking about it, and
	// the binary warns loudly on every boot when it is on.
	InsecureCookies bool
}

// Web serves /admin.
type Web struct {
	deps Deps
}

// New builds the dashboard.
func New(deps Deps) *Web {
	if deps.PingInterval == 0 {
		deps.PingInterval = 15 * time.Second
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Web{deps: deps}
}

// Mount attaches the admin subtree.
//
// ⚠ It uses the SAME authentication as the API and then requires the admin role.
// A dashboard with its own login is a second authentication system to keep
// correct, and the second one is always the one that rots.
func (wb *Web) Mount(r chi.Router, authenticate func(http.Handler) http.Handler, middleware ...func(http.Handler) http.Handler) {
	r.Route("/admin", func(r chi.Router) {
		// ⚠ Applied to the WHOLE subtree including the login routes, and
		// outermost, so failed sign-ins are logged. This subtree is mounted on
		// the parent mux rather than inside httpapi's router, so it does not
		// inherit that router's middleware — which is how the dashboard came to
		// produce no request log lines at all.
		for _, mw := range middleware {
			r.Use(mw)
		}

		// ⚠ OUTSIDE the authenticated group, and they are the only routes here
		// that are. A login page you must already be logged in to see is the
		// mistake this ordering prevents, and it is invisible in a diff — the
		// handler exists, the route exists, and the page 401s.
		//
		// They are also the second and third unauthenticated routes in the whole
		// process, after /healthz. cmd/router asserts that the list is exactly
		// these three.
		r.Get("/login", wb.showLogin)
		r.Post("/login", wb.doLogin)

		r.Group(func(r chi.Router) {
			r.Use(authenticate)
			r.Use(wb.requireAdmin)
			// The CSRF guard sits inside authentication because it only applies
			// to cookie-authenticated requests, and whether a request used a
			// cookie is not known before authenticate runs.
			r.Use(wb.requireSameOrigin)

			r.Post("/logout", wb.doLogout)
			r.Get("/", wb.overview)
			r.Get("/users", wb.users)
			r.Get("/services", wb.services)
			r.Get("/stream", wb.stream)
			r.Post("/users", wb.createUser)
			r.Post("/users/{id}/tokens", wb.mintToken)
			r.Post("/rates", wb.setRate)
		})
	})
}

// requireAdmin gates the whole subtree.
//
// The principal comes from httpapi's middleware via httpapi.PrincipalFrom, so
// there is exactly ONE context key in the process. A second, locally-defined key
// would compile and never match, handing every handler a zero Principal — which
// fails closed, so the symptom would be "the dashboard always says forbidden"
// with nothing pointing at the cause.
func (wb *Web) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !httpapi.PrincipalFrom(r).IsAdmin() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func principal(r *http.Request) core.Principal { return httpapi.PrincipalFrom(r) }

// buildDashboard is the read model: one function of the world, called by the
// page load AND by every SSE patch. Two renderers would drift.
func (wb *Web) buildDashboard(ctx context.Context) (views.Dashboard, error) {
	counts, err := wb.deps.Repo.CountJobsByState(ctx)
	if err != nil {
		return views.Dashboard{}, err
	}
	jobs, err := wb.deps.Repo.ListJobs(ctx, 50)
	if err != nil {
		return views.Dashboard{}, err
	}
	users, err := wb.deps.Repo.ListUsers(ctx)
	if err != nil {
		return views.Dashboard{}, err
	}
	depth, err := wb.deps.Repo.QueueDepthByLabel(ctx)
	if err != nil {
		return views.Dashboard{}, err
	}
	rates, err := wb.deps.Repo.ListRates(ctx)
	if err != nil {
		return views.Dashboard{}, err
	}

	emails := make(map[string]string, len(users))
	for _, u := range users {
		emails[u.ID] = u.Email
	}

	rows := make([]views.JobRow, 0, len(jobs))
	for _, j := range jobs {
		stage := "—"
		if len(j.Pipeline) > 1 {
			stage = fmt.Sprintf("%d/%d", j.Stage+1, len(j.Pipeline))
		}
		rows = append(rows, views.JobRow{Job: j, StageLabel: stage})
	}

	// The service list is the union of labels that have live workers, labels
	// with queued work, and labels with a configured rate. A label with a queue
	// and NO worker is the silent failure this table exists to surface, so it
	// must appear even though nothing is serving it.
	labels := map[string]struct{}{}
	for _, l := range wb.deps.Router.AvailableLabels(wb.deps.Now()) {
		labels[l] = struct{}{}
	}
	for l := range depth {
		labels[l] = struct{}{}
	}
	for l := range rates {
		labels[l] = struct{}{}
	}

	services := make([]views.ServiceRow, 0, len(labels))
	for l := range labels {
		rate, ok := rates[l]
		if !ok {
			rate = 1
		}
		services = append(services, views.ServiceRow{
			Label:   l,
			Workers: wb.deps.Bus.Subscribers(bus.WorkerTopic(l)),
			Queued:  depth[l],
			Rate:    rate,
		})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Label < services[j].Label })

	return views.Dashboard{
		Counts:          counts,
		Jobs:            rows,
		Users:           users,
		Services:        services,
		Emails:          emails,
		ResultsInMemory: wb.deps.Results.Len(),
	}, nil
}

func (wb *Web) render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

func (wb *Web) overview(w http.ResponseWriter, r *http.Request) {
	d, err := wb.buildDashboard(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	wb.render(w, r, views.Overview(d))
}

func (wb *Web) users(w http.ResponseWriter, r *http.Request) {
	d, err := wb.buildDashboard(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	wb.render(w, r, views.Users(d))
}

func (wb *Web) services(w http.ResponseWriter, r *http.Request) {
	d, err := wb.buildDashboard(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	wb.render(w, r, views.Services(d))
}

// signals is what datastar sends back on every action: every unprefixed signal
// on the page, as JSON.
type signals struct {
	NewEmail  string `json:"newEmail"`
	NewRole   string `json:"newRole"`
	RateLabel string `json:"rateLabel"`
	RateValue string `json:"rateValue"`
}

func readSignals(r *http.Request) (signals, error) {
	var s signals
	// ⚠ Read signals BEFORE opening an SSE writer. The datastar SDK flushes the
	// response as soon as it is constructed, and the request body cannot be read
	// afterwards.
	err := datastar.ReadSignals(r, &s)
	return s, err
}

// createUser handles the create action.
//
// ⚠ Every response here is 200 with an HTML fragment, including failures. A 4xx
// would leave the page with no explanation: datastar morphs what it is given,
// and an error status carries no fragment to morph.
func (wb *Web) createUser(w http.ResponseWriter, r *http.Request) {
	s, err := readSignals(r)
	if err != nil {
		wb.patch(w, r, views.CreateError("could not read the form"))
		return
	}

	actor := principal(r)
	role := core.Role(strings.TrimSpace(s.NewRole))
	if role == "" {
		role = core.RoleClient
	}

	user, err := wb.deps.Identity.CreateUser(r.Context(), actor, s.NewEmail, role, wb.deps.Now())
	if err != nil {
		wb.patch(w, r, views.CreateError(friendly(err)))
		return
	}

	d, err := wb.buildDashboard(r.Context())
	if err != nil {
		wb.patch(w, r, views.CreateError(err.Error()))
		return
	}
	wb.patch(w, r,
		views.CreateError(""),
		views.UserTable(d),
	)
	_ = user
}

func (wb *Web) mintToken(w http.ResponseWriter, r *http.Request) {
	actor := principal(r)
	userID := chi.URLParam(r, "id")

	user, err := wb.deps.Repo.UserByID(r.Context(), userID)
	if err != nil {
		wb.patch(w, r, views.CreateError(friendly(err)))
		return
	}
	_, token, err := wb.deps.Identity.MintToken(
		r.Context(), actor, userID, user.Role, "dashboard", wb.deps.Now())
	if err != nil {
		wb.patch(w, r, views.CreateError(friendly(err)))
		return
	}
	// Shown once. The router keeps only a hash and cannot produce it again.
	wb.patch(w, r, views.TokenOnce(token))
}

func (wb *Web) setRate(w http.ResponseWriter, r *http.Request) {
	s, err := readSignals(r)
	if err != nil {
		wb.patch(w, r, views.CreateError("could not read the form"))
		return
	}
	label := strings.TrimSpace(s.RateLabel)
	if label == "" {
		wb.patch(w, r, views.CreateError("a service label is required"))
		return
	}
	rate, err := strconv.Atoi(strings.TrimSpace(s.RateValue))
	if err != nil || rate < 0 {
		wb.patch(w, r, views.CreateError("credits per unit must be a non-negative whole number"))
		return
	}
	if err := wb.deps.Repo.SetRate(r.Context(), label, rate, wb.deps.Now()); err != nil {
		wb.patch(w, r, views.CreateError(err.Error()))
		return
	}
	wb.patch(w, r, views.RateSaved(label, rate))
}

// patch sends one or more fragments over SSE.
func (wb *Web) patch(w http.ResponseWriter, r *http.Request, cs ...templ.Component) {
	sse := datastar.NewSSE(w, r)
	for _, c := range cs {
		_ = sse.PatchElementTempl(c)
	}
}

// stream is the dashboard's live connection.
func (wb *Web) stream(w http.ResponseWriter, r *http.Request) {
	// ⚠ Same rule as the API's stream: clear the write deadline first, or a
	// server WriteTimeout kills this connection mid-session with nothing in the
	// logs.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Time{})
	}

	events, unsubscribe := wb.deps.Bus.Subscribe(bus.AdminTopic)
	defer unsubscribe()

	sse := datastar.NewSSE(w, r)
	ticker := time.NewTicker(wb.deps.PingInterval)
	defer ticker.Stop()

	push := func() bool {
		d, err := wb.buildDashboard(r.Context())
		if err != nil {
			return true // a transient read error must not end the stream
		}
		// Re-render from the read model rather than pushing what changed: two
		// events racing for one job then resolve correctly, because the second
		// render is simply current.
		if err := sse.PatchElementTempl(views.Stats(d)); err != nil {
			return false
		}
		if err := sse.PatchElementTempl(views.Jobs(d)); err != nil {
			return false
		}
		return sse.PatchElementTempl(views.Workers(d)) == nil
	}

	if !push() {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			if !push() {
				return
			}
		case <-ticker.C:
			if !push() {
				return
			}
		}
	}
}

// friendly turns a domain error into something an operator can act on.
// friendly turns a domain error into something an operator can act on.
func friendly(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, core.ErrConflict):
		return "that email address is already registered"
	case errors.Is(err, core.ErrInvalidParam):
		return "that email address does not look valid"
	case errors.Is(err, core.ErrForbidden):
		return "only an administrator can do that"
	default:
		return err.Error()
	}
}
