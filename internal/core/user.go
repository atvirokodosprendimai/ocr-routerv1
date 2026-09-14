package core

import "time"

// User is a customer or an administrator.
//
// The metering knobs live here rather than on a token because they are
// properties of the CUSTOMER: a customer with three tokens has one balance, one
// buffer limit and one priority, and issuing another token must not multiply any
// of them.
type User struct {
	ID    string
	Email string
	Role  Role

	// Credits is the balance the single writer maintains; credit_entries is the
	// append-only audit of every movement. It may go negative by at most one
	// document, because the size of a result is unknowable before the work runs
	// and refusing to hand over finished work the customer already paid for in
	// worker time would be worse.
	Credits int
	// BufferLimit caps in-flight jobs — queued, processing and done together —
	// so one customer cannot monopolise the worker pool, while still keeping
	// enough queued that workers never idle waiting for them.
	BufferLimit int
	// Priority orders the queue, higher first. It is an admin-set integer
	// rather than a named tier so that adding a tier is an UPDATE and not a
	// deploy.
	Priority int
	// JobTTLSecs is the queued deadline applied to this customer's jobs at
	// upload. Zero means no deadline — some customers would rather wait than
	// fail, and some would rather fail fast than get a stale answer.
	JobTTLSecs int

	Active    bool
	CreatedAt time.Time
}

// Token is a bearer credential. The plaintext is never stored.
type Token struct {
	ID     string
	UserID string
	Role   Role
	// Hash is the hex-encoded SHA-256 of the plaintext. The plaintext exists
	// only in the mint call's return value and in the holder's configuration;
	// nothing in the router can recover it, which is what makes "shown once"
	// true rather than merely a policy.
	Hash       string
	Label      string
	Revoked    bool
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// Principal is an authenticated caller, as every handler sees it.
//
// Authenticate returns this rather than the Token row so that no caller can
// reach the hash by accident, and so the authorisation decision is made from
// one small value with nothing else attached to it.
type Principal struct {
	UserID  string
	Role    Role
	TokenID string
}

// IsAdmin reports whether this principal may administer the system.
func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }
