package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/httpapi"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/ratelimit"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
	"github.com/atvirokodosprendimai/ocr-router/internal/session"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
	"github.com/atvirokodosprendimai/ocr-router/internal/web"
)

const adminPassword = "correct horse battery staple"

// tlsEnv serves the dashboard over TLS.
//
// ⚠ TLS IS NOT OPTIONAL HERE. The login handler refuses plain HTTP by design,
// because a Secure cookie set over http:// is silently discarded and the
// operator would see a sign-in that appears to work and then does not. A plain
// httptest server would make every test below fail for the right reason and the
// wrong one — and the fixture would then be "fixed" by weakening the refusal.
type tlsEnv struct {
	srv      *httptest.Server
	client   *http.Client
	ident    *identity.Service
	repo     *store.Repo
	sessions *session.Store
	adminID  string
	adminTok string
	limiter  *ratelimit.Limiter
	now      time.Time
}

// webOptions vary the server the dashboard is served from.
type webOptions struct {
	// insecureCookies sets web.Deps.InsecureCookies.
	insecureCookies bool
	// plain serves over HTTP instead of TLS — the shape an operator running on a
	// laptop actually has.
	plain   bool
	limiter *ratelimit.Limiter
}

func newTLSEnv(t *testing.T, limits *ratelimit.Limiter) *tlsEnv {
	t.Helper()
	return newEnvWith(t, webOptions{limiter: limits})
}

func newEnvWith(t *testing.T, opts webOptions) *tlsEnv {
	t.Helper()
	limits := opts.limiter
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blob.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	repo := store.NewRepo(db)
	res := results.New(time.Hour)
	b := bus.New()
	ident := identity.New(repo)
	sessions := session.New(db)
	ident.SetSessions(sessions)
	rt := router.New(repo, blobs, res, b, router.Config{
		Lease: 5 * time.Minute, MaxAttempts: 3, AgingStep: time.Minute,
		LabelGrace: 5 * time.Minute, DefaultLabel: "ocr",
	})

	ctx := context.Background()
	admin, adminTok, err := ident.Bootstrap(ctx, "admin@example.com", base)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := ident.SetPassword(ctx, admin.Email, adminPassword, base); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	now := func() time.Time { return base }
	api := httpapi.New(httpapi.Deps{
		Identity: ident, Router: rt, Repo: repo, Blobs: blobs, Bus: b,
		MaxUpload: 1 << 20, Now: now,
	})
	dash := web.New(web.Deps{
		Identity: ident, Router: rt, Repo: repo, Bus: b, Results: res,
		PingInterval: 50 * time.Millisecond, Now: now, Limiter: limits,
		InsecureCookies: opts.insecureCookies,
	})

	mux := chi.NewRouter()
	dash.Mount(mux, api.Authenticator())
	mux.Mount("/", api)

	srv := httptest.NewUnstartedServer(mux)
	if opts.plain {
		srv.Start()
	} else {
		srv.StartTLS()
	}
	t.Cleanup(srv.Close)

	client := srv.Client()
	// Redirects are followed by hand in these tests: the thing under test is
	// often the redirect itself.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &tlsEnv{
		srv: srv, client: client, ident: ident, repo: repo, sessions: sessions,
		adminID: admin.ID, adminTok: adminTok, limiter: limits, now: base,
	}
}

// newInsecureEnv is a PLAIN-HTTP dashboard with --insecure-cookies set, which is
// how an operator runs this on a laptop.
func newInsecureEnv(t *testing.T) *tlsEnv {
	t.Helper()
	e := newEnvWith(t, webOptions{insecureCookies: true, plain: true})
	return e
}

// post submits a form the way a browser would.
func (e *tlsEnv) post(t *testing.T, path string, form url.Values, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// login performs a real sign-in and returns the session cookie.
func (e *tlsEnv) login(t *testing.T, email, password string) *http.Cookie {
	t.Helper()
	resp := e.post(t, "/admin/login", url.Values{
		"email":    {email},
		"password": {password},
	}, e.srv.URL)
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login = %d, want 303: %s", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == web.SessionCookie {
			return c
		}
	}
	t.Fatal("login succeeded but set no session cookie")
	return nil
}

// getWithCookie fetches a path carrying a session cookie.
func (e *tlsEnv) getWithCookie(t *testing.T, path string, c *http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestLoginPageIsReachableWithoutCredentials is the whole point of ADR-0003, and
// it is red against the code as it stood before this task.
func TestLoginPageIsReachableWithoutCredentials(t *testing.T) {
	e := newTLSEnv(t, nil)

	resp := e.getWithCookie(t, "/admin/login", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/login with no credential = %d, want 200. The login page is mounted "+
			"inside the authenticated group, so it is a page you must already be logged in to "+
			"see", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `type="password"`) {
		t.Errorf("the login page renders no password field:\n%s", body)
	}
	if !strings.Contains(string(body), `name="email"`) {
		t.Errorf("the login page renders no email field:\n%s", body)
	}
}

func TestLoginSetsASessionCookie(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)
	if c.Value == "" {
		t.Fatal("the session cookie has no value")
	}
}

// TestSessionCookieAttributes checks each property by name, because losing
// HttpOnly costs something different from losing SameSite.
func TestSessionCookieAttributes(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	if !c.HttpOnly {
		t.Error("the session cookie is not HttpOnly — any script on the page can read the " +
			"administrator's credential")
	}
	if !c.Secure {
		t.Error("the session cookie is not Secure — it would be sent over plain HTTP")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict — this is the primary CSRF defence, and without it "+
			"a cross-site POST carries the cookie", c.SameSite)
	}
	if c.Path != "/admin" {
		t.Errorf("Path = %q, want /admin — a wider path sends the session to the API, which is "+
			"the exposure the scope exists to prevent", c.Path)
	}
}

// TestCookieValueIsNotTheUsersToken is red if someone "simplifies" this into
// putting the bearer token in the cookie — the alternative ADR-0003 rejected.
func TestCookieValueIsNotTheUsersToken(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	if c.Value == e.adminTok {
		t.Fatal("the session cookie IS the admin's bearer token. A token that leaks from a " +
			"cookie jar cannot be rotated without breaking whatever else uses it")
	}
	// And it is not any token in the table, hashed or otherwise.
	toks, err := e.repo.ListTokens(context.Background(), e.adminID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(toks) == 0 {
		t.Fatal("the admin holds no tokens, so this assertion proved nothing")
	}
	for _, tok := range toks {
		if c.Value == tok.Hash || c.Value == tok.ID {
			t.Errorf("the session cookie matches token %s", tok.ID)
		}
	}
}

func TestFailedLoginSaysNothingSpecific(t *testing.T) {
	e := newTLSEnv(t, nil)

	wrongPassword := e.post(t, "/admin/login", url.Values{
		"email": {"admin@example.com"}, "password": {"wrong password entirely"},
	}, e.srv.URL)
	unknownEmail := e.post(t, "/admin/login", url.Values{
		"email": {"nobody@example.com"}, "password": {adminPassword},
	}, e.srv.URL)

	if wrongPassword.StatusCode != http.StatusUnauthorized ||
		unknownEmail.StatusCode != http.StatusUnauthorized {
		t.Fatalf("statuses = %d and %d, want 401 for both",
			wrongPassword.StatusCode, unknownEmail.StatusCode)
	}
	a, _ := io.ReadAll(wrongPassword.Body)
	b, _ := io.ReadAll(unknownEmail.Body)
	if string(a) != string(b) {
		t.Error("a wrong password and an unknown email render different pages — identity.Login " +
			"collapses six causes into one error precisely so this page cannot become an " +
			"account-enumeration oracle, and the UI has undone it")
	}
	if strings.Contains(string(a), "nobody@example.com") ||
		strings.Contains(string(b), "nobody@example.com") {
		t.Error("the failure page echoes the submitted email")
	}
}

// TestLoginOverPlainHTTPExplainsItself — a Secure cookie over http:// is
// silently dropped, so without this the operator sees a sign-in that appears to
// succeed and then loops forever with nothing anywhere saying why.
func TestLoginOverPlainHTTPExplainsItself(t *testing.T) {
	// A PLAIN server, deliberately — this is the one test that must not use TLS.
	e := newEnv(t)

	resp, err := http.PostForm(e.srv.URL+"/admin/login", url.Values{
		"email": {"admin@example.com"}, "password": {adminPassword},
	})
	if err != nil {
		t.Fatalf("POST /admin/login: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	for _, c := range resp.Cookies() {
		if c.Name == web.SessionCookie && c.Value != "" {
			t.Error("a session cookie was set over plain HTTP — the browser will discard it and " +
				"the operator will never learn why sign-in does nothing")
		}
	}
	if !strings.Contains(strings.ToUpper(string(body)), "TLS") {
		t.Errorf("the refusal does not mention TLS, so the operator has no way to act on it:\n%s",
			body)
	}
}

// TestLogoutRevokesServerSide replays the OLD cookie value.
//
// ⚠ Asserting only that a clearing header was sent would pass against a logout
// that leaves a live credential with anyone who copied the value. The browser's
// behaviour is not a security boundary.
func TestLogoutRevokesServerSide(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	if got := e.getWithCookie(t, "/admin", c).StatusCode; got != http.StatusOK {
		t.Fatalf("the session did not work before logout: %d", got)
	}

	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/logout", nil)
	req.AddCookie(c)
	req.Header.Set("Origin", e.srv.URL)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/logout: %v", err)
	}
	_ = resp.Body.Close()

	// The SAME cookie value, replayed directly.
	if got := e.getWithCookie(t, "/admin", c).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("replaying the old session cookie after logout = %d, want 401. Logout only "+
			"cleared the cookie; the credential is still live server-side", got)
	}
}

func TestLogoutClearsTheCookie(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/logout", nil)
	req.AddCookie(c)
	req.Header.Set("Origin", e.srv.URL)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/logout: %v", err)
	}
	defer resp.Body.Close()

	var found bool
	for _, got := range resp.Cookies() {
		if got.Name == web.SessionCookie {
			found = true
			if got.MaxAge >= 0 {
				t.Errorf("Max-Age = %d, want negative so the browser drops it", got.MaxAge)
			}
		}
	}
	if !found {
		t.Error("logout sent no clearing cookie")
	}
}

// TestStateChangingRequestWithNoOriginIsRefused is the single most likely bug in
// ADR-0003.
//
// The natural spelling — `if origin != "" && origin != want { reject }` —
// permits every request that omits the header, which is every request an
// attacker writes by hand. This is the case that separates a closed guard from
// an open one.
func TestStateChangingRequestWithNoOriginIsRefused(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/users",
		strings.NewReader(`{"email":"x@example.com","role":"client"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(c)
	// NO Origin and NO Referer.
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/users: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a cookie-authenticated POST with no Origin and no Referer = %d, want 403. The "+
			"origin check fails OPEN, which permits exactly the requests an attacker writes by "+
			"hand", resp.StatusCode)
	}
}

func TestStateChangingRequestWithForeignOriginIsRefused(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/users",
		strings.NewReader(`{"email":"x@example.com","role":"client"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(c)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/users: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a POST from evil.example = %d, want 403", resp.StatusCode)
	}
}

// TestStateChangingRequestWithMatchingOriginSucceeds is the positive companion.
// Without it the two tests above are satisfied by a guard that refuses
// everything, which would break the dashboard entirely.
func TestStateChangingRequestWithMatchingOriginSucceeds(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/users",
		strings.NewReader(`{"email":"newuser@example.com","role":"client"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.srv.URL)
	req.AddCookie(c)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/users: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("the dashboard's own POST was refused as cross-origin: %s. The guard rejects "+
			"everything, which makes the two negative tests above meaningless", body)
	}
}

func TestRefererIsAcceptedWhenOriginIsAbsent(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/users",
		strings.NewReader(`{"email":"referer@example.com","role":"client"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", e.srv.URL+"/admin/users")
	req.AddCookie(c)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/users: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		t.Error("a matching Referer was refused — the fallback for browsers that omit Origin " +
			"does not work, so those browsers cannot use the dashboard at all")
	}
}

// TestOriginGuardDoesNotApplyToTokenAuth — a bearer token is not attached
// automatically, so it has no CSRF exposure and must not pay for one.
func TestOriginGuardDoesNotApplyToTokenAuth(t *testing.T) {
	e := newTLSEnv(t, nil)

	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/users",
		strings.NewReader(`{"email":"api@example.com","role":"client"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.adminTok)
	// No Origin — an API client has no reason to send one.
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/users: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		t.Error("a token-authenticated POST was refused for a missing Origin. Scripting the " +
			"dashboard's endpoints with a bearer token now breaks, for no security benefit")
	}
}

func TestGetRequestsAreNotOriginChecked(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	// A typed URL carries no Origin. Guarding GET would break every navigation.
	if got := e.getWithCookie(t, "/admin", c).StatusCode; got != http.StatusOK {
		t.Errorf("GET /admin with a session and no Origin = %d, want 200", got)
	}
}

func TestLoginIsRateLimitedPerEmail(t *testing.T) {
	// A tight limit in the fixture: at the production default this would pass
	// without ever throttling, proving nothing.
	lim := ratelimit.New(func() time.Time { return base }, time.Hour)
	e := newTLSEnv(t, lim)

	var throttled bool
	for range 12 {
		resp := e.post(t, "/admin/login", url.Values{
			"email": {"admin@example.com"}, "password": {"wrong"},
		}, e.srv.URL)
		if resp.StatusCode == http.StatusTooManyRequests {
			throttled = true
			if resp.Header.Get("Retry-After") == "" {
				t.Error("the throttled login carries no Retry-After")
			}
			break
		}
	}
	if !throttled {
		t.Fatal("twelve failed logins for one email were never throttled — argon2 costs 64MB " +
			"and tens of milliseconds per attempt, reachable with no credential")
	}

	// ★ A DIFFERENT email still gets through. A global limit would pass the
	// assertion above while locking out every administrator at once.
	resp := e.post(t, "/admin/login", url.Values{
		"email": {"someone-else@example.com"}, "password": {"wrong"},
	}, e.srv.URL)
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Error("a different email was throttled by the first one's failures — the limit is " +
			"global, so one attacker locks out every administrator")
	}
}

// TestLoginRateLimitIsNotALockout — an attacker must not be able to lock out the
// real administrator by guessing at their email.
func TestLoginRateLimitIsNotALockout(t *testing.T) {
	clock := base
	lim := ratelimit.New(func() time.Time { return clock }, time.Hour)
	e := newTLSEnv(t, lim)

	for range 12 {
		e.post(t, "/admin/login", url.Values{
			"email": {"admin@example.com"}, "password": {"wrong"},
		}, e.srv.URL)
	}

	// Time passes and the bucket refills.
	clock = base.Add(time.Hour)

	resp := e.post(t, "/admin/login", url.Values{
		"email": {"admin@example.com"}, "password": {adminPassword},
	}, e.srv.URL)
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Error("the real administrator is still locked out an hour after an attacker's failed " +
			"guesses. A rate limit must refill; a lockout keyed on something an attacker " +
			"supplies is a denial of service against the victim")
	}
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("login after refill = %d, want 303: %s", resp.StatusCode, body)
	}
}

func TestSessionAuthenticatesTheDashboard(t *testing.T) {
	e := newTLSEnv(t, nil)
	c := e.login(t, "admin@example.com", adminPassword)

	resp := e.getWithCookie(t, "/admin/users", c)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/users with a session = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ocr-router") {
		t.Error("the dashboard did not render for a session-authenticated request")
	}
}

func TestNoCookieStillRedirectsToLogin(t *testing.T) {
	e := newTLSEnv(t, nil)

	if got := e.getWithCookie(t, "/admin", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("GET /admin with no credential = %d, want 401", got)
	}
}

func TestRetryAfterOnThrottledLoginIsAnInteger(t *testing.T) {
	lim := ratelimit.New(func() time.Time { return base }, time.Hour)
	e := newTLSEnv(t, lim)

	for range 12 {
		resp := e.post(t, "/admin/login", url.Values{
			"email": {"admin@example.com"}, "password": {"wrong"},
		}, e.srv.URL)
		if resp.StatusCode == http.StatusTooManyRequests {
			raw := resp.Header.Get("Retry-After")
			secs, err := strconv.Atoi(raw)
			if err != nil {
				t.Fatalf("Retry-After = %q, which is not an integer", raw)
			}
			if secs < 1 {
				t.Errorf("Retry-After = %d — a client told to retry in zero seconds retries "+
					"straight into another refusal", secs)
			}
			return
		}
	}
	t.Fatal("never throttled, so there was no header to check")
}

// TestInsecureCookiesAllowsPlainHTTPLogin covers the local-development escape
// hatch.
//
// ⚠ Without it there is NO way to use the dashboard on http://localhost: the
// login page can only tell the operator to go and get TLS, which made the very
// first run of this feature impossible. That gap was found by an operator
// hitting the page, not by any test here.
func TestInsecureCookiesAllowsPlainHTTPLogin(t *testing.T) {
	e := newInsecureEnv(t)

	// ⚠ e.post, not http.PostForm. DefaultClient FOLLOWS the 303 and has no
	// cookie jar, so the followed GET /admin arrives with no session and 401s —
	// and the test then reports a 401 for a login that actually succeeded. That
	// cost a debugging round; e.client has CheckRedirect disabled.
	resp := e.post(t, "/admin/login", url.Values{
		"email": {"admin@example.com"}, "password": {adminPassword},
	}, e.srv.URL)

	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login over plain HTTP with --insecure-cookies = %d, want 303: %s",
			resp.StatusCode, body)
	}
	var c *http.Cookie
	for _, got := range resp.Cookies() {
		if got.Name == web.SessionCookie {
			c = got
		}
	}
	if c == nil {
		t.Fatal("no session cookie was set")
	}
	if c.Secure {
		t.Error("the cookie is still Secure, so the browser will discard it over plain HTTP — " +
			"which is the whole thing this flag exists to avoid")
	}
	// The other protections are NOT relaxed: this flag is about the transport,
	// not about the cookie's reach or its CSRF properties.
	if !c.HttpOnly {
		t.Error("--insecure-cookies also dropped HttpOnly, which it has no business touching")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Error("--insecure-cookies also dropped SameSite=Strict, which it has no business " +
			"touching — the CSRF defence is unrelated to the transport")
	}
	if c.Path != "/admin" {
		t.Errorf("--insecure-cookies widened the cookie path to %q", c.Path)
	}
}

// TestSecureIsTheDefault is the companion that keeps the flag honest.
//
// The test above would pass against a build that simply never set Secure. This
// one asserts the safe behaviour is what you get by not asking for anything.
func TestSecureIsTheDefault(t *testing.T) {
	e := newTLSEnv(t, nil) // InsecureCookies deliberately unset
	c := e.login(t, "admin@example.com", adminPassword)
	if !c.Secure {
		t.Error("the session cookie is not Secure by default — the dangerous behaviour is what " +
			"an operator gets without asking for it")
	}
}

// TestSessionCookieNameMatchesHttpapi pins the two constants together.
//
// Two packages each spelling out the cookie name would compile, never match, and
// hand every request an empty session — which fails closed, so the symptom would
// be "login does nothing" with nothing pointing at the cause. The same failure
// httpapi.PrincipalFrom exists to prevent for the context key.
func TestSessionCookieNameMatchesHttpapi(t *testing.T) {
	if web.SessionCookie != httpapi.SessionCookie {
		t.Errorf("web.SessionCookie = %q but httpapi.SessionCookie = %q — the dashboard sets a "+
			"cookie the authenticator never reads", web.SessionCookie, httpapi.SessionCookie)
	}
}
