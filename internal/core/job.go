package core

import "time"

// Job is one unit of work moving through the router.
//
// It is a mutable row, not an event log: nobody will ask what a job looked like
// at time T, so the storage shape is state-based (cqrs §0). The one part of this
// domain that IS append-only is the credit ledger, because money is the
// canonical case where the log is the domain.
type Job struct {
	ID       string
	UserID   string
	Filename string
	SizeByte int64

	// Label is the service this job wants RIGHT NOW. For a pipeline it is
	// Pipeline[Stage]; it is denormalised onto the row so the claim statement
	// can filter and index on it without parsing JSON per row.
	Label string
	// Pipeline is the ordered list of service labels. A single-stage job has
	// one entry. It is fixed at upload — there is no defined semantics for
	// changing it mid-flight once cost has accrued.
	Pipeline []string
	// Stage indexes Pipeline. Advancing it is the router's job, never a
	// worker's: the pipeline is router-owned state precisely so that a worker
	// token, which lives on an untrusted host, cannot create or redirect work.
	Stage int

	// Params are client-supplied and become subprocess flags on the worker.
	// Keys are validated with ValidParamKey at upload; values are data.
	Params map[string]string
	// HasBlob records whether a source file exists, explicitly rather than by
	// inferring it from an empty path. A crawler-style job has parameters and
	// no file at all, and that is a normal shape rather than a corrupt upload.
	HasBlob bool
	// Raw records that this job's output is opaque bytes rather than a list of
	// units, and is stamped ONCE at admission (ADR-0006). It is deliberately a
	// property of the JOB rather than a lookup of the service's current mode: an
	// administrator editing a service must not reprice work already running.
	Raw bool
	// ExitCode is the forked command's exit status, or nil when the failure never
	// reached one (ADR-0007).
	//
	// ⚠ A POINTER, deliberately. A timeout, an output-limit trip and a contract
	// violation all fail WITHOUT exiting, and 0 is the code for SUCCESS — so an
	// int would make "never exited" indistinguishable from "exited cleanly", which
	// is the one comparison an operator most wants to make.
	ExitCode *int

	State    JobState
	Attempts int
	// Units is the size of the final stage's output — pages, documents,
	// whatever the service produced — and is what the client sees.
	Units int
	// AccruedCredits is the running cost across every completed stage, at each
	// stage's own rate. It is debited once, at delivery.
	AccruedCredits int

	WorkerID       string
	LeaseExpiresAt time.Time
	LastError      string

	// QueuedAt is stamped once at upload and never rewritten — not on retry and
	// not on a stage advance. A job that has already waited keeps the priority
	// age it accrued, which is what stops long pipelines and retried jobs
	// starving behind freshly uploaded work.
	QueuedAt time.Time
	// ExpiresAt is the whole-pipeline deadline, zero when the customer's TTL is
	// unset. It bounds only the queued state: once a worker holds the lease the
	// job runs to completion, because discarding work already paid for in
	// worker time helps nobody.
	ExpiresAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsLastStage reports whether the current stage is the final one.
//
// It decides whether completing this stage notifies the client or silently
// advances the pipeline, so getting it wrong delivers a half-processed
// intermediate as though it were the finished result.
//
// An empty pipeline counts as its own last stage: a job with no declared
// pipeline is a single-stage job whose label stands alone.
func (j Job) IsLastStage() bool {
	if len(j.Pipeline) == 0 {
		return true
	}
	return j.Stage >= len(j.Pipeline)-1
}

// NextLabel returns the label of the stage after the current one.
//
// The second return is false on the last stage, so a caller cannot read a zero
// value as a real label.
func (j Job) NextLabel() (string, bool) {
	if j.IsLastStage() {
		return "", false
	}
	return j.Pipeline[j.Stage+1], true
}

// LedgerEntry is one movement of credits, append-only.
//
// The ledger is the system of record for money: a correction is a compensating
// entry and never an edit, which is why this type has no update path and why the
// table it maps to is written but never modified.
type LedgerEntry struct {
	ID     string
	UserID string
	// JobID is empty for movements that are not a delivery — an admin top-up,
	// a manual adjustment.
	JobID string
	// Delta is negative for a charge and positive for a grant.
	Delta     int
	Reason    string
	CreatedAt time.Time
}

// Result is a finished job's output, held only in memory.
//
// It never touches disk: the source blob on disk is the durable copy, and a
// result lost to a restart or to its TTL simply returns the job to the queue to
// be produced again. Nothing is charged until it reaches the customer.
type Result struct {
	JobID string
	Units []string
}
