package httpapi

import (
	"context"
	"net/http"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

type ctxKey int

const principalKey ctxKey = iota

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

// principal returns the authenticated caller.
//
// It returns a zero Principal when absent rather than panicking. The zero value
// has an empty Role, which matches no real role, so a handler reached without
// the middleware fails CLOSED — it cannot be mistaken for an admin.
func principal(r *http.Request) core.Principal {
	p, _ := r.Context().Value(principalKey).(core.Principal)
	return p
}
