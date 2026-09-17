package router

import (
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// Logger is the slice of a structured logger the single writer needs.
//
// An interface at the CONSUMER, matching Counter: router is the lower layer and
// must not depend on how — or whether — anything is being recorded. It also
// means a Service built without a logger still works, which is what keeps every
// test written before ADR-0002 unchanged.
type Logger interface {
	Transition(TransitionEvent)
}

// TransitionEvent is one movement of a job between states.
//
// ⚠ IT CARRIES THE PARAM MAP, and the adapter that renders it is responsible for
// emitting only the keys. The map crosses exactly one boundary inside the
// process and must never reach a line: ADR-0001 lets a crawler take `?url=…`,
// and a URL carries credentials often enough to treat as certain.
type TransitionEvent struct {
	JobID  string
	UserID string
	Label  string
	From   core.JobState
	To     core.JobState
	// Attempt is the attempt number this transition belongs to, so three
	// requeues of one job are distinguishable in a log.
	Attempt int
	// WorkerID is the worker that caused this, empty when the router or the
	// reaper did.
	WorkerID string
	// Actor names what moved the job: "client", "worker" or "reaper". It is the
	// field that answers "did this expire, or did someone fail it".
	Actor string
	// Stage is the pipeline index the job is on after this transition.
	Stage int
	// Params are the job's parameters. Keys only ever reach a log line.
	Params map[string]string
	// Reason is the failure text, empty on a successful transition.
	Reason string
	// ExitCode is the forked command's status on a failure line, nil when the
	// failure never reached one (ADR-0007). Additive to ADR-0002's schema: it
	// appears only when there is one, and changes no attribute already emitted.
	ExitCode *int
	// InState is how long the job spent in the state it just left.
	//
	// ⚠ Computed from the job row's stored timestamp, never from anything held
	// in memory: an in-memory start time is wrong after a restart, and a
	// duration that is silently wrong is worse than an absent one. This is the
	// field that turns "the job died" into "it sat queued for forty minutes and
	// then failed in two seconds".
	InState time.Duration
}

// nopLogger is the default, so no call site needs a nil check.
type nopLogger struct{}

func (nopLogger) Transition(TransitionEvent) {}

// SetLogger attaches a structured logger to the service.
func (s *Service) SetLogger(l Logger) {
	if l == nil {
		l = nopLogger{}
	}
	s.logger = l
}

// logTransition emits one event.
//
// ⚠ `from` and `since` are EXPLICIT arguments rather than read off the job,
// because whether `job` is the pre- or post-transition row differs by call site:
// Claim's `UPDATE … RETURNING` hands back the row it just wrote, so inferring
// them there would report leased→leased with a duration of zero — a
// plausible-looking number that is always wrong. Making the caller say what it
// means is the only version that cannot be silently incorrect.
//
// `since` is always a STORED timestamp, never a value held in memory, so the
// duration survives a restart.
// logTransition records a state change that carries no exit code, which is every
// transition but a command failure.
func (s *Service) logTransition(job core.Job, from, to core.JobState, since time.Time,
	actor, workerID, reason string, now time.Time,
) {
	s.logFailureTransition(job, from, to, since, actor, workerID, reason, nil, now)
}

// logFailureTransition is the same line plus the forked command's exit code.
//
// A separate entry point rather than a widened one: eleven of this service's
// thirteen transitions have no exit code by construction, and threading a nil
// through all of them would make the two that DO carry one indistinguishable at
// the call site from the ones that never could.
func (s *Service) logFailureTransition(job core.Job, from, to core.JobState, since time.Time,
	actor, workerID, reason string, exitCode *int, now time.Time,
) {
	s.logger.Transition(TransitionEvent{
		JobID:    job.ID,
		UserID:   job.UserID,
		Label:    job.Label,
		From:     from,
		To:       to,
		Attempt:  job.Attempts,
		WorkerID: workerID,
		Actor:    actor,
		Stage:    job.Stage,
		Params:   job.Params,
		Reason:   reason,
		ExitCode: exitCode,
		InState:  now.Sub(since),
	})
}
