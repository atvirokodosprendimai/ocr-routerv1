package web_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// browserAccept is what every browser sends on a navigation. Spelled out in full
// rather than as "text/html", because the production rule is a substring match
// and a test that sends only the needle would pass a rule that compares equal.
const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

// navigate makes the request a browser makes, and does NOT follow the answer.
//
// The default client follows a 303, so it would report the login page's 200 and
// make a correct redirect indistinguishable from an unauthenticated page load.
func navigate(t *testing.T, e *env, method, path, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(""))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestBrowserNavigationWithoutASessionGoesToTheLoginPage is ADR-0003 task T3's
// third acceptance criterion, which shipped unmet.
//
// ⚠ It is red on the behaviour an operator actually met: GET /admin/users in a
// browser rendered `{"error":"unauthorized"}`, a dead end with no way to proceed
// and nothing naming the login page. The whole reason ADR-0003 exists is that a
// browser sends no Authorization header, so answering it in the API's dialect
// reintroduces exactly the gap that record closed.
func TestBrowserNavigationWithoutASessionGoesToTheLoginPage(t *testing.T) {
	e := newEnv(t)

	for _, path := range []string{"/admin", "/admin/users", "/admin/services"} {
		resp := navigate(t, e, http.MethodGet, path, browserAccept)
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("GET %s from a browser = %d, want 303 to the login page", path, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Location"); got != "/admin/login" {
			t.Errorf("GET %s redirected to %q, want /admin/login", path, got)
		}
	}
}

// TestAStaleSessionCookieAlsoRedirects covers the case an operator hits second:
// the session has expired, so a credential IS presented and is not valid. A rule
// keyed on "no credential" rather than on "authentication failed" would leave
// this one on the JSON 401 — the 12-hour expiry makes it the common path, not the
// edge case.
func TestAStaleSessionCookieAlsoRedirects(t *testing.T) {
	e := newEnv(t)

	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/admin", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept", browserAccept)
	req.AddCookie(&http.Cookie{Name: "ocrr_session", Value: "expired-or-forged"})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /admin: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /admin with a dead session = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/admin/login" {
		t.Errorf("redirected to %q, want /admin/login", got)
	}
}

// TestANonBrowserCallerStillGetsTheAPIs401 is the other half, and it is the half
// that makes the redirect safe to add.
//
// An API client scripting the dashboard's endpoints, and the dashboard's own
// datastar calls, must keep a status they can branch on. A 303 to an HTML page is
// worse than useless to both: datastar would morph a login document into the page,
// and a script would read the login page's 200 as success.
func TestANonBrowserCallerStillGetsTheAPIs401(t *testing.T) {
	e := newEnv(t)

	cases := []struct {
		name         string
		method, path string
		accept       string
	}{
		{"no Accept header at all", http.MethodGet, "/admin/users", ""},
		{"an API client asking for JSON", http.MethodGet, "/admin/users", "application/json"},
		{"a datastar @get", http.MethodGet, "/admin/stream", "text/event-stream"},
		// ⚠ A POST carrying the browser's own Accept header. Method alone decides
		// this one: a datastar action posts from a page whose Accept says
		// text/html, and answering it with a redirect to a document would replace
		// the operator's dashboard with a login form on a stale click.
		{"a datastar action from the page", http.MethodPost, "/admin/users", browserAccept},
	}

	for _, c := range cases {
		resp := navigate(t, e, c.method, c.path, c.accept)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %s %s = %d, want 401", c.name, c.method, c.path, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("WWW-Authenticate"); got == "" {
			t.Errorf("%s: no WWW-Authenticate header — the challenge is what tells a "+
				"client how to authenticate rather than merely that it failed", c.name)
		}
		var body struct{ Error string }
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Errorf("%s: body is not the API's JSON error: %v", c.name, err)
			continue
		}
		if body.Error != "unauthorized" {
			t.Errorf("%s: error = %q, want %q", c.name, body.Error, "unauthorized")
		}
	}
}

// TestTheLoginPageIsReachableFromWhereTheRedirectSends is the step that turns the
// redirect into a way in rather than a loop. A 303 to a route that 401s is not an
// improvement on the 401 it replaced, and nothing else in this file would notice.
func TestTheLoginPageIsReachableFromWhereTheRedirectSends(t *testing.T) {
	e := newEnv(t)

	resp := navigate(t, e, http.MethodGet, "/admin/login", browserAccept)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/login = %d, want 200 — the redirect target must serve the form",
			resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}
