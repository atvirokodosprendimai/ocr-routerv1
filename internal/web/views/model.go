package views

import (
	"time"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
)

// Dashboard is the read model the views render.
//
// It is a plain struct built by one function from the repository, and every view
// is a pure function of it — which is what lets the first paint and every SSE
// patch call the same code. A view that reached for a global or the clock would
// no longer be a function of its input, and the two callers would drift.
type Dashboard struct {
	Counts map[core.JobState]int
	Jobs   []JobRow
	// FailedOnly says the job rows have been filtered down to failures. It is a
	// property of the read model rather than of the request so that the SSE
	// patch and the first paint render the same table — a stream that pushed the
	// unfiltered list over a filtered page would undo the filter on the first
	// event.
	FailedOnly      bool
	Users           []core.User
	Services        []ServiceRow
	Emails          map[string]string
	ResultsInMemory int
	// Usage is each customer's counts over the four windows, keyed by user id
	// (ADR-0010). A customer with no jobs is absent from the map rather than
	// present with zeros, and the zero value renders as zeros, so the view needs
	// no branch for it.
	Usage map[string]core.Usage
	// Now is the instant the read model was built. The usage windows were
	// computed from it, so the view labels its month column from the SAME value
	// rather than reading the clock again — a header that disagreed with the
	// numbers beneath it would be worse than no header.
	Now time.Time
}

// prevMonthLabel names the previous complete calendar month, UTC — "Aug 2026"
// when `now` is in September 2026.
//
// It takes `now` rather than reading the clock so it cannot disagree with the
// counts it heads, and it derives the month the same way the query does: subtract
// one from the month number and let time.Date normalise, which handles January →
// December of the previous year without a special case.
func prevMonthLabel(now time.Time) string {
	utc := now.UTC()
	prev := time.Date(utc.Year(), utc.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	return prev.Format("Jan 2006")
}

// JobRow is one row of the job table, with the display-only bits precomputed so
// the template holds no logic.
type JobRow struct {
	Job core.Job
	// StageLabel reads "2/3" for a pipeline, or "—" for a single-stage job.
	StageLabel string
	// Exit is the forked command's status as text: "3", or "—" for a job that
	// never exited at all — a timeout, a reclaimed lease, a job still running.
	//
	// ⚠ It is a string precisely so that absence cannot render as 0. 0 is the
	// code for SUCCESS, so a timeout displayed as 0 reads as a command that
	// finished cleanly and failed anyway, which is the confusion the nullable
	// column exists to prevent (ADR-0007).
	Exit string
}

// TokenList is one user's tokens, for the expandable row under the user table.
//
// ⚠ It exists because minting a token was the ONLY token operation the dashboard
// had. `Repo.ListTokens` and `identity.RevokeToken` were written, tested, and
// called by nothing — so an operator could create credentials and never see
// which existed, which were revoked, or when each was last used, and could not
// revoke one without opening the database.
type TokenList struct {
	UserID string
	Email  string
	Tokens []TokenRow
}

// TokenRow is one token, with the display-only bits precomputed so the template
// holds no logic.
type TokenRow struct {
	Token core.Token
	// LastSeen reads "3m ago", or "never" for a token that has never been used —
	// which is the single most useful fact on this table, because it is how an
	// operator tells a live integration from one nobody cleaned up.
	LastSeen string
	// Created is the same relative rendering for created_at.
	Created string
}

// ServiceRow is one label's live state.
type ServiceRow struct {
	Label   string
	Workers int
	Queued  int
	Rate    int
	// Raw is the service's output mode. It is shown beside the rate because it
	// is part of the price: a raw service bills a flat credit per job rather
	// than Rate per unit.
	Raw bool
}
