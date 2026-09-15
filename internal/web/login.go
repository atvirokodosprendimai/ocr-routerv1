package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/ratelimit"
	"github.com/atvirokodosprendimai/ocr-router/internal/web/views"
)

// SessionCookie is the cookie the dashboard authenticates with.
//
// Exported so httpapi reads the same name. Two packages each spelling it out
// would compile, never match, and hand every request an empty session — which
// fails closed, so the symptom would be "login does nothing" with nothing
// pointing at the cause. The same reasoning as httpapi.PrincipalFrom.
const SessionCookie = "ocrr_session"

// loginLimit is deliberately tighter than the API's.
//
// Keyed on the normalised EMAIL, not the IP: ADR-0001 puts callers anywhere on
// the internet, so one NAT is many administrators and one attacker is many
// addresses. It is a rate limit and never a lockout — the real administrator
// must always get through eventually, or guessing at their email becomes a
// denial of service against them.
var loginLimit = ratelimit.Limit{RPS: 0.2, Burst: 5}

// cookieOK reports whether a session cookie set now would actually be kept.
//
// Over plain HTTP a Secure cookie is silently discarded, so "can we sign in" and
// "is this connection TLS" are the same question — unless the operator has
// explicitly turned Secure off for local development.
func (wb *Web) cookieOK(r *http.Request) bool {
	return wb.deps.InsecureCookies || isSecure(r)
}

// showLogin renders the sign-in page.
func (wb *Web) showLogin(w http.ResponseWriter, r *http.Request) {
	renderPage(w, r, views.Login("", !wb.cookieOK(r)))
}

// doLogin verifies a password and starts a session.
func (wb *Web) doLogin(w http.ResponseWriter, r *http.Request) {
	// ⚠ Refuse over plain HTTP with an explanation, rather than setting a Secure
	// cookie the browser will silently discard. Without this the operator sees a
	// sign-in that appears to succeed and lands back on the form forever, with
	// nothing anywhere saying why.
	if !wb.cookieOK(r) {
		w.WriteHeader(http.StatusBadRequest)
		renderPage(w, r, views.Login("", true))
		return
	}

	if err := r.ParseForm(); err != nil {
		wb.loginFailed(w, r)
		return
	}
	email := r.PostFormValue("email")
	password := r.PostFormValue("password")

	// Throttle before the KDF runs: argon2 costs 64MB and tens of milliseconds
	// per attempt, and this endpoint is reachable without a credential.
	if wb.deps.Limiter != nil {
		key := "login:" + strings.ToLower(strings.TrimSpace(email))
		if !wb.deps.Limiter.Allow(key, loginLimit) {
			secs := ratelimit.RetryAfterSeconds(wb.deps.Limiter.RetryAfter(key, loginLimit))
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			w.WriteHeader(http.StatusTooManyRequests)
			renderPage(w, r, views.Login("Too many attempts. Try again shortly.", false))
			return
		}
	}

	secret, err := wb.deps.Identity.Login(r.Context(), email, password, wb.deps.Now())
	if err != nil {
		wb.loginFailed(w, r)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:  SessionCookie,
		Value: secret,
		// Path scopes the cookie to the dashboard, so it is not even sent to
		// /upload or /claim. The API's credential is the bearer token.
		Path: "/admin",
		// HttpOnly keeps it out of reach of any script on the page.
		HttpOnly: true,
		// Secure means TLS only — which is why plain HTTP is refused above.
		// ⚠ Secure unless explicitly disabled for local development. Dropping it
		// is what lets the cookie survive plain HTTP, and it is also what lets
		// the cookie travel in clear text — hence the boot warning.
		Secure: !wb.deps.InsecureCookies,
		// SameSite=Strict is the primary CSRF defence: a cross-site POST does
		// not carry this cookie at all.
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(identity.SessionTTL / time.Second),
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// loginFailed renders ONE generic message.
//
// ⚠ Never "no such user" or "wrong password". identity.Login collapses six
// distinct causes into one error precisely so this page cannot become an
// account-enumeration oracle, and saying more here would undo that.
func (wb *Web) loginFailed(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
	renderPage(w, r, views.Login("Incorrect email or password.", false))
}

// doLogout revokes the session and clears the cookie.
func (wb *Web) doLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		// ⚠ Revoke FIRST, server-side. Clearing the cookie alone leaves a live
		// credential with anyone who copied its value, and the browser's
		// behaviour is not a security boundary.
		_ = wb.deps.Identity.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		// The clearing cookie must match the attributes of the one it replaces,
		// or the browser treats it as a different cookie and the original
		// survives. Getting this wrong makes logout appear to work and leave the
		// session in place.
		Secure:   !wb.deps.InsecureCookies,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// requireSameOrigin refuses a cross-site state-changing request.
//
// ⚠ IT FAILS CLOSED, AND THAT IS THE WHOLE POINT. The natural spelling —
//
//	if origin != "" && origin != want { reject }
//
// permits every request that omits the header, which is every request an
// attacker writes by hand. This one requires a matching Origin, falling back to
// Referer, and refuses when neither is present.
//
// It applies only to requests authenticated BY COOKIE. A bearer token is not
// attached automatically by the browser, so it has no CSRF exposure and must not
// pay for one — an API client scripting the dashboard's endpoints would
// otherwise break for no benefit.
func (wb *Web) requireSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStateChanging(r.Method) || r.Header.Get("Authorization") != "" {
			next.ServeHTTP(w, r)
			return
		}
		if !originMatches(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isStateChanging reports whether a method may change server state.
//
// GET and HEAD are excluded: applying the guard to them would break every
// ordinary navigation, since a typed URL carries no Origin.
func isStateChanging(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// originMatches compares the request's declared origin to its own host.
func originMatches(r *http.Request) bool {
	host := r.Host
	if host == "" {
		return false
	}

	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil {
			return false
		}
		return u.Host == host
	}
	// Referer is the fallback for a browser that omits Origin. Same comparison,
	// same closed default.
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil {
			return false
		}
		return u.Host == host
	}
	// Neither header. Refused.
	return false
}

// isSecure reports whether the request reached us over TLS.
//
// The X-Forwarded-Proto fallback exists because the intended deployment
// terminates TLS in a proxy, where r.TLS is nil however the client connected.
func isSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// renderPage writes a templ component as a complete HTML response.
//
// The dashboard's own helper renders fragments into an already-open response;
// the login page is a whole document, so it sets its own content type.
func renderPage(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The error is unrecoverable here: the status is already written and the
	// client has half a page. Nothing useful remains to say.
	_ = c.Render(r.Context(), w)
}
