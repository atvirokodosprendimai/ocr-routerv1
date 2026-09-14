package core

import "errors"

// Sentinel errors every package matches on with errors.Is.
//
// They exist so that no caller has to match on an error STRING, and so that the
// HTTP boundary can map domain outcomes to status codes in exactly one place.
// A new sentinel added here without a corresponding entry in that mapper
// becomes a 500, which is why the mapper's test is table-driven over all of them.
var (
	// ErrNotFound covers a missing row, a missing blob, a missing result, and an
	// empty claim. Deliberately one error: distinguishing them at the boundary
	// would leak whether an id exists to a caller who does not own it.
	ErrNotFound = errors.New("not found")

	// ErrUnauthorized is an unknown token, a revoked token, and a token on a
	// deactivated user — ONE error for all three, so the response cannot be used
	// to enumerate which tokens once existed.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrForbidden is an authenticated caller doing something their role does
	// not permit. Distinct from ErrUnauthorized because the caller is known.
	ErrForbidden = errors.New("forbidden")

	// ErrNoCredits refuses an upload from a customer with a non-positive
	// balance. Checked at admission because the cost of a job is unknowable
	// before it runs.
	ErrNoCredits = errors.New("no credits")

	// ErrBufferFull refuses an upload from a customer already at their in-flight
	// limit. It is back-pressure, not a failure: the customer retries later.
	ErrBufferFull = errors.New("buffer full")

	// ErrConflict is a lost race — a worker completing a job whose lease it no
	// longer holds, or a second delivery of a result already taken.
	ErrConflict = errors.New("conflict")

	// ErrInvalidState is a transition the lifecycle does not allow. Reaching it
	// means the single writer tried something CanTransitionTo forbids, so it is
	// a programming error surfacing as a value rather than a panic.
	ErrInvalidState = errors.New("invalid state transition")

	// ErrInvalidParam is a param key that failed ValidParamKey, or an unknown
	// service label. Both are client input that must be refused at the edge.
	ErrInvalidParam = errors.New("invalid parameter")
)
