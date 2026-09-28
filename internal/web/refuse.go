package web

import (
	"net/http"
	"strings"

	"github.com/atvirokodosprendimai/ocr-router/internal/httpapi"
)

// loginPath is where a browser that is not signed in is sent.
const loginPath = "/admin/login"

// Unauthorized is the dashboard's answer to a request that failed authentication.
//
// ⚠ IT IS NOT A SECOND AUTHENTICATION PATH, and the distinction is the whole
// reason it can exist at all: httpapi still decides WHO the caller is, and this
// decides only what a refusal looks like. The two audiences cannot use each
// other's answer — `{"error":"unauthorized"}` is unusable from a browser, which
// is why ADR-0003 was written, and its task T3 names "a browser hitting /admin
// unauthenticated is redirected to /admin/login" as an acceptance criterion. The
// dashboard nonetheless served that JSON 401 to browsers, because borrowing the
// API's authenticator borrowed its refusal too.
//
// Hand it to httpapi's AuthenticatorFor; see cmd/router/wire.go.
func (wb *Web) Unauthorized() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isNavigation(r) {
			// Everything that is not a browser navigation keeps the API's
			// answer, challenge header and all: an API client scripting these
			// endpoints, and the dashboard's own datastar calls, can act on a
			// 401 and can do nothing with a page of HTML.
			httpapi.WriteUnauthorized(w)
			return
		}
		// 303 rather than 302: the answer to this GET is "look over there", and
		// 303 says so without inviting a client to re-issue the original method.
		//
		// ⚠ No `?next=` parameter. The requested URL is attacker-influenced
		// input, and validating it as a same-origin relative path is its own
		// piece of work — deferred in docs/adr/BACKLOG.md, because an open
		// redirect on the sign-in page would be a worse defect than one extra
		// click.
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
	})
}

// isNavigation reports whether this request is a browser opening a page.
//
// It reads the method AND Accept, because either alone is wrong. A datastar
// action is a POST that must not be answered with a page; a datastar `@get` asks
// for `text/event-stream` and would be broken by one. A typed URL, a bookmark and
// a followed link all send `Accept: text/html,…`, and that header is the only
// thing in the request that tells them apart from the calls the page itself makes.
func isNavigation(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return strings.Contains(r.Header.Get("Accept"), "text/html")
	default:
		return false
	}
}
