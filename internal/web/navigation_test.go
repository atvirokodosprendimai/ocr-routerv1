package web_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// adminSections is every page the layout's nav offers, with the title the handler
// passes to Layout.
var adminSections = []struct{ path, title string }{
	{"/admin", "Overview"},
	{"/admin/users", "Customers"},
	{"/admin/services", "Services"},
}

// TestTheCurrentSectionIsMarked closes a gap where the STYLING existed and the
// attribute never did.
//
// `app.css` has carried `nav a[aria-current="page"]` with an accent underline
// since the dashboard shipped, and no view ever emitted the attribute — so the
// rule was dead and an operator had nothing telling them which page they were on.
// A dead selector is invisible to every test that asserts the page renders.
func TestTheCurrentSectionIsMarked(t *testing.T) {
	e := newEnv(t)

	for _, want := range adminSections {
		page := e.page(t, want.path)

		marked := currentSection(page)
		if marked == "" {
			t.Errorf("GET %s marks no nav link as the current page", want.path)
			continue
		}
		if marked != want.title {
			t.Errorf("GET %s marks %q as current, want %q", want.path, marked, want.title)
		}
		// Exactly one, WITHIN THE SECTIONS NAV. Counting the whole page is wrong:
		// the Overview page also carries the job-filter nav, which correctly marks
		// its own current filter, so a page-wide count of 2 is right there and
		// tells us nothing about this nav.
		if n := strings.Count(sectionsNav(t, page), `aria-current="page"`); n != 1 {
			t.Errorf("GET %s marks %d links in the Sections nav as current, want exactly 1",
				want.path, n)
		}
	}
}

// sectionsNav returns just the layout's section navigation.
func sectionsNav(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `<nav aria-label="Sections">`)
	if i < 0 {
		t.Fatal("the page has no Sections nav at all, so this assertion proved nothing")
	}
	end := strings.Index(page[i:], "</nav>")
	if end < 0 {
		t.Fatal("the Sections nav is never closed")
	}
	return page[i : i+end]
}

// currentSection returns the text of whichever nav link is marked current, or ""
// when none is. It reads the rendered HTML rather than the template, because the
// attribute has to survive templ's conditional-attribute rendering.
func currentSection(page string) string {
	for _, s := range adminSections {
		// templ renders a conditional attribute on the element it belongs to, so
		// the marker and the label are in the same anchor.
		i := strings.Index(page, `href="`+s.path+`"`)
		if i < 0 {
			continue
		}
		end := strings.Index(page[i:], "</a>")
		if end < 0 {
			continue
		}
		if strings.Contains(page[i:i+end], `aria-current="page"`) {
			return s.title
		}
	}
	return ""
}

// TestTheDashboardOffersAWayToSignOut is the fourth instance in this codebase of
// a handler that was written, mounted, tested and reachable from nothing.
//
// `POST /admin/logout` revokes the session server-side and clears the cookie, the
// login page explains what signing out does, and no page rendered a control for
// it — so an administrator could only end a session by deleting a cookie by hand
// or waiting twelve hours.
func TestTheDashboardOffersAWayToSignOut(t *testing.T) {
	e := newEnv(t)

	for _, s := range adminSections {
		page := e.page(t, s.path)
		if !strings.Contains(page, `data-on:click="@post('/admin/logout')"`) {
			t.Errorf("GET %s renders no sign-out control — POST /admin/logout is mounted and "+
				"unreachable from the page an administrator is looking at", s.path)
		}
		// ⚠ Not a <form>: TestNoFormTags is the house rule and the first attempt
		// here broke it. The control is a datastar action, which is why doLogout
		// answers an event-stream request with sse.Redirect.
		if strings.Contains(page, "<form") {
			t.Errorf("GET %s renders a <form>; the house rule binds inputs individually and "+
				"the login page's form is exempt only by not using this layout", s.path)
		}
	}
}

// TestSigningOutFromTheDashboardRedirectsTheBrowser is the half that makes the
// control work rather than merely exist.
//
// A datastar action gets an SSE reply; a 303 would be FOLLOWED by datastar's own
// fetch, which would then parse the login page as an event stream and fail
// silently — the session gone and the screen unchanged, which reads as a control
// that does nothing.
func TestSigningOutFromTheDashboardRedirectsTheBrowser(t *testing.T) {
	e := newEnv(t)

	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/admin/logout", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.adminTok)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/logout: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /admin/logout as a datastar action = %d, want 200 with an SSE body",
			resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream — datastar cannot act on anything else",
			ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if !strings.Contains(string(body), "/admin/login") {
		t.Errorf("the stream does not send the browser to the login page: %q", string(body))
	}
}
