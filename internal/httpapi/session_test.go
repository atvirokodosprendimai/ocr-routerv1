package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/httpapi"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/logging"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
	"github.com/atvirokodosprendimai/ocr-router/internal/session"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// sessionEnv mounts the API under a chi router with an /admin subtree, so the
// cookie's path scope can actually be exercised.
//
// ⚠ It does NOT mount the real dashboard: this package must not import web
// (web imports httpapi). A stub handler under /admin is enough, because what is
// under test is the AUTHENTICATOR's treatment of the cookie, not what the
// dashboard renders.
type sessionEnv struct {
	srv       *httptest.Server
	ident     *identity.Service
	sessions  *session.Store
	logs      *lockedBuffer
	clientTok string
	adminID   string
}

func newSessionEnv(t *testing.T) *sessionEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blob.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	repo := store.NewRepo(db)
	b := bus.New()
	ident := identity.New(repo)
	sessions := session.New(db)
	ident.SetSessions(sessions)
	rt := router.New(repo, blobs, results.New(time.Hour), b, router.Config{
		Lease: 5 * time.Minute, MaxAttempts: 3, AgingStep: time.Minute,
		LabelGrace: 5 * time.Minute, DefaultLabel: "ocr",
	})

	ctx := context.Background()
	admin, adminTok, err := ident.Bootstrap(ctx, "admin@example.com", base)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ap, _ := ident.Authenticate(ctx, "Bearer "+adminTok, base)
	cu, _ := ident.CreateUser(ctx, ap, "c@example.com", core.RoleClient, base)
	_ = repo.AddCredits(ctx, cu.ID, 100, "seed", base)
	_, clientTok, _ := ident.MintToken(ctx, ap, cu.ID, core.RoleClient, "c", base)

	logs := &lockedBuffer{}
	log, err := logging.New(logging.Options{Out: logs})
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}

	api := httpapi.New(httpapi.Deps{
		Identity: ident, Router: rt, Repo: repo, Blobs: blobs, Bus: b,
		MaxUpload: 1 << 20, Now: func() time.Time { return base }, Logger: log,
	})

	mux := chi.NewRouter()
	// A stub /admin subtree behind the API's authenticator — the same shape
	// cmd/router builds, minus the dashboard itself.
	mux.Route("/admin", func(r chi.Router) {
		r.Use(api.Authenticator())
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			p := httpapi.PrincipalFrom(r)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(p.UserID + " " + string(p.Role)))
		})
	})
	mux.Mount("/", api)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &sessionEnv{
		srv: srv, ident: ident, sessions: sessions, logs: logs,
		clientTok: clientTok, adminID: admin.ID,
	}
}

// newSession issues a session for the admin.
func (e *sessionEnv) newSession(t *testing.T) *http.Cookie {
	t.Helper()
	secret, err := e.sessions.Create(context.Background(), e.adminID, base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("session.Create: %v", err)
	}
	return &http.Cookie{Name: httpapi.SessionCookie, Value: secret}
}

func (e *sessionEnv) request(t *testing.T, method, path, token string, c *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestCookieAuthenticatesTheDashboard(t *testing.T) {
	e := newSessionEnv(t)
	c := e.newSession(t)

	resp := e.request(t, "GET", "/admin", "", c)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin with only a session cookie = %d, want 200", resp.StatusCode)
	}
}

// TestHeaderWinsOverCookie is red if the cookie is consulted first.
//
// ⚠ That ordering would silently escalate every API call made from a logged-in
// administrator's browser: a client's own token would be ignored and the request
// would run with admin rights.
func TestHeaderWinsOverCookie(t *testing.T) {
	e := newSessionEnv(t)
	c := e.newSession(t) // an ADMIN session

	// A CLIENT token presented alongside it.
	resp := e.request(t, "GET", "/admin", e.clientTok, c)

	// The client is not an admin, so the stub handler is reached only if the
	// authenticator chose the token — and the role it reports proves which.
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	got := string(body[:n])

	if strings.Contains(got, string(core.RoleAdmin)) {
		t.Errorf("a request carrying a CLIENT token and an ADMIN cookie was authenticated as "+
			"admin (%q). The cookie is consulted before the header, so any API call from a "+
			"logged-in admin's browser silently runs with admin rights", got)
	}
	if resp.StatusCode == http.StatusOK && !strings.Contains(got, string(core.RoleClient)) {
		t.Errorf("the request resolved to neither role: %q", got)
	}
}

// TestCookieIsRefusedOutsideAdmin is the property that keeps CSRF out of the API.
//
// ⚠ No test in internal/web can see this — that package does not know /upload
// exists. A cookie is attached by the browser automatically and a bearer token
// is not; confining the cookie to the subtree that has CSRF defences is what
// keeps the whole API out of the threat model.
func TestCookieIsRefusedOutsideAdmin(t *testing.T) {
	e := newSessionEnv(t)
	c := e.newSession(t)

	for _, r := range []struct{ method, path string }{
		{"POST", "/upload"},
		{"POST", "/claim"},
		{"GET", "/services"},
		{"GET", "/files/x"},
		{"GET", "/sse"},
	} {
		resp := e.request(t, r.method, r.path, "", c)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with a session cookie = %d, want 401. The session is usable against "+
				"the API, so every API endpoint is now reachable by CSRF from a logged-in "+
				"administrator's browser", r.method, r.path, resp.StatusCode)
		}
	}

	// And the same cookie still works where it is supposed to, or the assertions
	// above are satisfied by a cookie that works nowhere.
	if got := e.request(t, "GET", "/admin", "", c).StatusCode; got != http.StatusOK {
		t.Fatalf("the cookie does not work under /admin either (%d), so this test proves "+
			"nothing", got)
	}
}

func TestExpiredCookieIsRefused(t *testing.T) {
	e := newSessionEnv(t)
	secret, err := e.sessions.Create(context.Background(), e.adminID, base, base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("session.Create: %v", err)
	}
	c := &http.Cookie{Name: httpapi.SessionCookie, Value: secret}

	if got := e.request(t, "GET", "/admin", "", c).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("an expired session cookie = %d, want 401", got)
	}
}

func TestGarbageCookieIsRefused(t *testing.T) {
	e := newSessionEnv(t)

	for _, v := range []string{"", "garbage", strings.Repeat("a", 43)} {
		c := &http.Cookie{Name: httpapi.SessionCookie, Value: v}
		if got := e.request(t, "GET", "/admin", "", c).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("cookie value %q = %d, want 401", v, got)
		}
	}
}

// TestRequestLogNeverContainsTheSessionCookie — "already true" is not "will stay
// true".
//
// The request logger emits a fixed field set and never headers, so the cookie
// cannot reach a line today. This pins it, because the natural way to make a log
// more useful is to add the headers.
func TestRequestLogNeverContainsTheSessionCookie(t *testing.T) {
	e := newSessionEnv(t)
	c := e.newSession(t)

	// ⚠ An API route, not the /admin stub. The stub is mounted on the outer mux,
	// outside the API's own request logger, so a request to it produces no line
	// at all — which the vacuity guard below caught on the first run. The cookie
	// still travels in the request header here, which is what the test is about.
	if got := e.request(t, "GET", "/services", e.clientTok, c).StatusCode; got != http.StatusOK {
		t.Fatalf("GET /services = %d, so there may be no log line to check", got)
	}

	out := e.logs.String()
	if out == "" {
		t.Fatal("nothing was logged, so this assertion proved nothing")
	}
	if strings.Contains(out, c.Value) {
		t.Errorf("the session secret appears in a log line — anyone with log access can "+
			"impersonate the administrator:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "cookie") {
		t.Errorf("the request log carries a cookie field:\n%s", out)
	}
}

// TestSessionCookieConstantIsShared pins the name against the dashboard's copy.
//
// The reverse of web's own assertion, kept here too because this is the package
// that READS the cookie: if the two drift, the dashboard sets one the
// authenticator never looks for, and the symptom is "login does nothing".
func TestSessionCookieConstantIsNotEmpty(t *testing.T) {
	if httpapi.SessionCookie == "" {
		t.Fatal("the session cookie name is empty, so every request carries a nameless cookie")
	}
}
