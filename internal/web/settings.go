package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/ocr-router/internal/web/views"
)

// updateSettings changes a customer's buffer limit, priority and job TTL.
//
// It is the first caller identity.UpdateSettings has, and the reason ADR-0004
// exists: every one of these fields was displayed by the dashboard and editable
// only by typing UPDATE into SQLite.
func (wb *Web) updateSettings(w http.ResponseWriter, r *http.Request) {
	s, err := readSignals(r)
	if err != nil {
		wb.patch(w, r, views.CreateError("could not read the form"))
		return
	}
	userID := chi.URLParam(r, "id")

	buffer, err := parseIntField(s.BufferLimit.String(), "buffer limit")
	if err != nil {
		wb.patch(w, r, views.CreateError(err.Error()))
		return
	}
	priority, err := parseIntField(s.Priority.String(), "priority")
	if err != nil {
		wb.patch(w, r, views.CreateError(err.Error()))
		return
	}
	ttl, err := parseIntField(s.JobTTL.String(), "job TTL")
	if err != nil {
		wb.patch(w, r, views.CreateError(err.Error()))
		return
	}

	if err := wb.deps.Identity.UpdateSettings(
		r.Context(), principal(r), userID, buffer, priority, ttl); err != nil {
		// friendly() carries the service's own message through, so "buffer limit
		// must be at least 1; use the active toggle to stop a customer" reaches
		// the screen rather than becoming a generic failure.
		wb.patch(w, r, views.CreateError(friendly(err)))
		return
	}
	wb.refreshUsers(w, r)
}

// adjustCredits moves a balance by a signed delta with a reason.
//
// ⚠ ADJUST, NEVER SET, and there is no handler that sets a balance.
// credit_entries is the append-only audit of every movement; a direct write
// would move the balance with no entry and the ledger would stop being an audit
// with nothing failing to say so.
func (wb *Web) adjustCredits(w http.ResponseWriter, r *http.Request) {
	s, err := readSignals(r)
	if err != nil {
		wb.patch(w, r, views.CreateError("could not read the form"))
		return
	}
	userID := chi.URLParam(r, "id")

	delta, err := parseIntField(s.CreditDelta.String(), "credit adjustment")
	if err != nil {
		wb.patch(w, r, views.CreateError(err.Error()))
		return
	}

	if err := wb.deps.Identity.AdjustCredits(
		r.Context(), principal(r), userID, delta, s.CreditReason, wb.deps.Now()); err != nil {
		wb.patch(w, r, views.CreateError(friendly(err)))
		return
	}
	wb.refreshUsers(w, r)
}

// setActive enables or disables an account.
//
// ⚠ This is identity.SetActive's FIRST CALLER. The method was written,
// admin-gated and tested in ADR-0001 and reachable from nothing until now — the
// third instance of that defect found in this codebase.
func (wb *Web) setActive(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "id")

	user, err := wb.deps.Repo.UserByID(r.Context(), userID)
	if err != nil {
		wb.patch(w, r, views.CreateError(friendly(err)))
		return
	}

	// A toggle: the new state is the opposite of the stored one, read here rather
	// than sent by the browser. Trusting a posted boolean would let a stale page
	// re-disable an account somebody just re-enabled.
	if err := wb.deps.Identity.SetActive(r.Context(), principal(r), userID, !user.Active); err != nil {
		wb.patch(w, r, views.CreateError(friendly(err)))
		return
	}
	wb.refreshUsers(w, r)
}

// refreshUsers re-renders the customer table with current data.
//
// Every successful edit ends here, so the operator sees the result in place —
// the confirmation and the new state in one response, the same shape the token
// list uses.
func (wb *Web) refreshUsers(w http.ResponseWriter, r *http.Request) {
	d, err := wb.buildDashboard(r.Context())
	if err != nil {
		wb.patch(w, r, views.CreateError(friendly(err)))
		return
	}
	// The table AND an empty error slot: without clearing it, a refusal from a
	// previous attempt stays on screen next to the result of a successful one,
	// which reads as the success having failed.
	wb.patch(w, r, views.UserTable(d), views.CreateError(""))
}

// parseIntField turns a signal string into an int with a message an operator can
// act on.
//
// An empty field is an error rather than a zero: silently reading a blank buffer
// limit as 0 would hit the service's floor and produce a confusing refusal about
// a value the operator never typed.
func parseIntField(raw, name string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, &fieldError{name: name, msg: "is required"}
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, &fieldError{name: name, msg: "must be a whole number"}
	}
	return n, nil
}

// fieldError names the field so the message says which box is wrong.
type fieldError struct{ name, msg string }

func (e *fieldError) Error() string { return e.name + " " + e.msg }
