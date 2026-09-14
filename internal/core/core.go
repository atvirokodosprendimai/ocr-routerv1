// Package core is the shared domain kernel of the router.
//
// Every other package depends on it and it depends on nothing but the standard
// library and github.com/google/uuid. That asymmetry is deliberate: the cqrs
// skill's fan-out rule (§2c.1) makes the kernel the only thing packages may
// share, so a dependency added here is a dependency added everywhere.
//
// It holds types, sentinel errors and two pure predicates. It contains no SQL,
// no HTTP and no I/O.
package core

import "github.com/google/uuid"

// Role is what a bearer token authorises its holder to do.
//
// The role is always read from the token's database row, never parsed from the
// token string: the string carries a human-readable prefix so an operator can
// judge a leaked token's blast radius at a glance, and that prefix is a label
// with no authority behind it.
type Role string

// The three roles. A worker sees every customer's file, so worker tokens belong
// to an operator-owned account and never to a customer.
const (
	RoleAdmin  Role = "admin"
	RoleClient Role = "client"
	RoleWorker Role = "worker"
)

// Valid reports whether r is one of the three known roles.
//
// It exists so that an unrecognised value read out of the database is a caught
// error rather than a silent default — a zero-value Role must never behave like
// any real role.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleClient, RoleWorker:
		return true
	}
	return false
}

// JobState is where a job sits in its lifecycle.
type JobState string

// The six states. Three are terminal: JobDelivered, JobDead and JobExpired.
const (
	// JobQueued is waiting for a worker serving its current label.
	JobQueued JobState = "queued"
	// JobProcessing is leased to a worker that has not yet reported back.
	JobProcessing JobState = "processing"
	// JobDone holds a result in memory for its owner to collect.
	JobDone JobState = "done"
	// JobDelivered was collected and charged. Terminal.
	JobDelivered JobState = "delivered"
	// JobDead exhausted its retry attempts. Terminal, and never charged.
	JobDead JobState = "dead"
	// JobExpired sat queued past its deadline. Terminal, and never charged.
	JobExpired JobState = "expired"
)

// Valid reports whether s is one of the six known states.
func (s JobState) Valid() bool {
	switch s {
	case JobQueued, JobProcessing, JobDone, JobDelivered, JobDead, JobExpired:
		return true
	}
	return false
}

// transitions is the whole lifecycle, as data rather than as a chain of ifs.
//
// A state absent from this map, or present with an empty slice, is terminal.
// Keeping it as one table is what lets the test assert the complete matrix
// including every refusal — a permissive predicate written as nested ifs passes
// any test that only exercises the happy edges.
var transitions = map[JobState][]JobState{
	// A queued job is claimed, passes its deadline, or is abandoned by the
	// reaper after its attempts are exhausted.
	JobQueued: {JobProcessing, JobExpired, JobDead},
	// A leased job finishes, loses its lease and is requeued, or dies.
	JobProcessing: {JobDone, JobQueued, JobDead},
	// A finished job is collected, or loses its in-memory result — to the TTL
	// sweeper or to a restart — and goes back to be redone from its blob.
	JobDone: {JobDelivered, JobQueued},
}

// CanTransitionTo reports whether the job may move to next.
//
// This is the one rule the single writer enforces about shape; everything else
// it enforces is about ownership, leases and credits.
func (j Job) CanTransitionTo(next JobState) bool {
	for _, allowed := range transitions[j.State] {
		if allowed == next {
			return true
		}
	}
	return false
}

// NewID mints a UUIDv7 as its canonical string.
//
// v7 is time-ordered, so lexical order is arrival order. That is what lets the
// claim statement say ORDER BY id for FIFO-within-a-priority-tier instead of
// carrying a separate sequence column, and it is why nothing here may be
// swapped for v4.
//
// It panics if the system entropy source fails, which is not a condition any
// caller could handle: a router that cannot mint an id cannot accept work.
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		panic("core: cannot mint uuidv7: " + err.Error())
	}
	return id.String()
}

// maxParamKeyLen bounds a key at a length no real flag name exceeds.
const maxParamKeyLen = 32

// ValidParamKey reports whether k is safe to use as a subprocess flag name.
//
// This is a trust boundary and it lives in the kernel rather than in the worker
// because BOTH ends must agree: the router rejects a bad key at upload and the
// worker refuses to build argv from one. Two copies of the rule are two chances
// to disagree about what is safe.
//
// The grammar is ^[a-z][a-z0-9-]{0,31}$ — deliberately narrower than anything a
// shell could interpret. The key is constrained because it becomes a FLAG NAME;
// values are data and are not pattern-checked, because they are passed as their
// own argv element and never concatenated with anything.
func ValidParamKey(k string) bool {
	if k == "" || len(k) > maxParamKeyLen {
		return false
	}
	if k[0] < 'a' || k[0] > 'z' {
		return false
	}
	for i := 1; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}
