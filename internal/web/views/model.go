package views

import "github.com/atvirokodosprendimai/ocr-router/internal/core"

// Dashboard is the read model the views render.
//
// It is a plain struct built by one function from the repository, and every view
// is a pure function of it — which is what lets the first paint and every SSE
// patch call the same code. A view that reached for a global or the clock would
// no longer be a function of its input, and the two callers would drift.
type Dashboard struct {
	Counts          map[core.JobState]int
	Jobs            []JobRow
	Users           []core.User
	Services        []ServiceRow
	Emails          map[string]string
	ResultsInMemory int
}

// JobRow is one row of the job table, with the display-only bits precomputed so
// the template holds no logic.
type JobRow struct {
	Job core.Job
	// StageLabel reads "2/3" for a pipeline, or "—" for a single-stage job.
	StageLabel string
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
}
