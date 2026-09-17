// Package router is the single writer of job state and of the credit ledger.
//
// Every rule about when a job may move, who may move it, and what it costs
// lives here. Handlers call this package for writes and the repository directly
// for reads — that asymmetry is the CQRS, and it is the whole of the CQRS this
// project takes.
package router

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// Config holds the knobs the operator sets on the command line.
type Config struct {
	// Lease is how long a worker holds a job before the reaper takes it back.
	Lease time.Duration
	// MaxAttempts bounds retries before a job is abandoned as dead.
	MaxAttempts int
	// AgingStep is how much waiting time buys one point of effective priority.
	// It tunes the whole queue, not one customer, which is why it is not a
	// per-user column.
	AgingStep time.Duration
	// LabelGrace keeps a label valid for a while after its last worker
	// disconnects, so a rolling worker restart does not start rejecting uploads.
	LabelGrace time.Duration
	// DefaultLabel is the service a client gets when it names none.
	// DefaultLabel is the service a client gets when it names none.
	DefaultLabel string
	// ResultTTL is how long a finished result stays collectable. The in-memory
	// store enforces it for units results; the reaper enforces the SAME window
	// for raw results, which live on disk and are invisible to that store.
	ResultTTL time.Duration
}

// Service is the write API. There is exactly one per process.
type Service struct {
	repo    *store.Repo
	blobs   *blob.Store
	results *results.Store
	bus     *bus.Bus
	cfg     Config

	// mu guards labelSeen only. It is deliberately NOT a lock around the
	// service: serialisation of writes comes from the single writer connection
	// in the store, not from here.
	mu sync.Mutex
	// labelSeen is when a label last had a live worker. It is what implements
	// the grace window.
	labelSeen map[string]time.Time

	// counter records what happened, for the metrics endpoint. Never nil: the
	// constructor installs a no-op so no call site needs a guard.
	counter Counter
	// logger records the same transitions in prose. Counters say three jobs
	// died; this says WHICH, on which worker, after how long in each state.
	// Never nil, same reasoning.
	logger Logger
}

// New builds the service.
func New(repo *store.Repo, blobs *blob.Store, res *results.Store, b *bus.Bus, cfg Config) *Service {
	if cfg.DefaultLabel == "" {
		cfg.DefaultLabel = "ocr"
	}
	return &Service{
		repo: repo, blobs: blobs, results: res, bus: b, cfg: cfg,
		labelSeen: make(map[string]time.Time),
		counter:   nopCounter{},
		logger:    nopLogger{},
	}
}

// UploadInput is what a client submits.
type UploadInput struct {
	Filename string
	// Body is the source file, or nil. A nil body is the CRAWLER shape: the job
	// carries only parameters and the service fetches its own input. That is a
	// normal upload, not a malformed one.
	Body io.Reader
	// Pipeline is the ordered list of service labels. Empty means a one-stage
	// pipeline of the default label.
	Pipeline []string
	// Params become subprocess flags on the worker. Keys are validated here;
	// values are data.
	Params map[string]string
	// Raw is the output mode the CLIENT asked for, and must agree with the
	// service's admin-owned mode or the upload is refused (ADR-0006).
	//
	// A plain bool, deliberately. The task planned a tri-state so that "the
	// client said nothing" stayed distinguishable from "the client said units" —
	// but the two produce the same outcome in every case: both are admitted on a
	// units service and both are refused on a raw one. A distinction nothing can
	// act on is state that can only be got wrong.
	Raw bool
}

// Upload admits a job.
//
// Order matters throughout: every refusal happens before anything is written, and
// the blob is written before the row so a row can never name a blob that does not
// exist. The publish happens last, after the write has committed — publishing
// first would show workers a job that is not there.
func (s *Service) Upload(ctx context.Context, userID string, in UploadInput, now time.Time) (core.Job, error) {
	u, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return core.Job{}, err
	}
	if !u.Active {
		return core.Job{}, core.ErrForbidden
	}

	// Admission is gated on having ANY credit, not on the eventual cost: the
	// size of a result is unknowable before the work runs.
	if u.Credits <= 0 {
		return core.Job{}, core.ErrNoCredits
	}

	inFlight, err := s.repo.CountInFlight(ctx, userID)
	if err != nil {
		return core.Job{}, err
	}
	if inFlight >= u.BufferLimit {
		return core.Job{}, core.ErrBufferFull
	}

	for k := range in.Params {
		if !core.ValidParamKey(k) {
			return core.Job{}, fmt.Errorf("%w: parameter name %q", core.ErrInvalidParam, k)
		}
	}

	pipeline := in.Pipeline
	if len(pipeline) == 0 {
		pipeline = []string{s.cfg.DefaultLabel}
	}
	for _, l := range pipeline {
		if strings.TrimSpace(l) == "" {
			return core.Job{}, fmt.Errorf("%w: empty service label in pipeline", core.ErrInvalidParam)
		}
	}

	// Only the FIRST stage is checked against live workers. A later stage's
	// workers may legitimately start after the job is queued, and refusing the
	// upload for that would make a pipeline undeployable one service at a time.
	if !s.labelAvailable(pipeline[0], now) {
		return core.Job{}, fmt.Errorf("%w: no worker is serving %q (available: %s)",
			core.ErrNotFound, pipeline[0], strings.Join(s.AvailableLabels(now), ", "))
	}

	// The mode agreement, and the LAST refusal before anything is written. The
	// client's declaration must match the service's admin-owned mode for exactly
	// the reason a worker's must (ADR-0006): raw is a price, the admin owns it,
	// and both ends only get to agree with it.
	//
	// Only pipeline[0] is checked here, consistently with the live-worker check
	// above — a later stage's mode is validated when the job advances into it.
	_, wantRaw, err := s.repo.ServiceMode(ctx, pipeline[0])
	if err != nil {
		return core.Job{}, err
	}
	if in.Raw != wantRaw {
		return core.Job{}, fmt.Errorf("%w: client asked for %s output from %q, which the operator has configured as %s",
			core.ErrModeMismatch, modeName(in.Raw), pipeline[0], modeName(wantRaw))
	}

	job := core.Job{
		ID:       core.NewID(),
		UserID:   userID,
		Filename: in.Filename,
		Label:    pipeline[0],
		Pipeline: pipeline,
		Stage:    0,
		Params:   in.Params,
		// Stamped ONCE, here. A job carries the mode it was admitted under, so an
		// administrator editing the service later cannot reprice work already
		// running.
		Raw:       in.Raw,
		State:     core.JobQueued,
		QueuedAt:  now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if job.Params == nil {
		job.Params = map[string]string{}
	}
	if u.JobTTLSecs > 0 {
		job.ExpiresAt = now.Add(time.Duration(u.JobTTLSecs) * time.Second)
	}

	if in.Body != nil {
		n, err := s.blobs.Put(job.ID, in.Body)
		if err != nil {
			return core.Job{}, err
		}
		job.HasBlob = true
		job.SizeByte = n
	}

	if err := s.repo.CreateJob(ctx, job); err != nil {
		// The blob would otherwise be orphaned; nothing references it.
		if job.HasBlob {
			_ = s.blobs.Delete(job.ID)
		}
		return core.Job{}, err
	}

	// The job's first appearance in the log, and the only place its param KEYS
	// are recorded — which is what lets a later failure be read against what was
	// actually asked for.
	s.logTransition(job, "", core.JobQueued, now, "client", "", "", now)
	s.publishWork(job.Label)
	s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: job.ID})
	return job, nil
}

// Claim leases the next job for a label.
//
// The service adds no read-then-write of its own: ordering, the label filter and
// the deadline all live in the repository's single statement, so two workers
// cannot both believe they won.
func (s *Service) Claim(ctx context.Context, workerID, label string, now time.Time) (core.Job, error) {
	s.noteLabel(label, now)
	job, err := s.repo.ClaimOneQueued(ctx, label, workerID, now, now.Add(s.cfg.Lease), s.cfg.AgingStep)
	if err != nil {
		return core.Job{}, err
	}
	// QueuedAt, not UpdatedAt: the row returned here has already been stamped by
	// the claim, and "how long did this job wait to be picked up" is the number
	// worth having.
	s.logTransition(job, core.JobQueued, core.JobProcessing, job.QueuedAt,
		"worker", workerID, "", now)
	s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: job.ID})
	return job, nil
}

// holdsLease reports whether this worker may still act on this job.
//
// A worker whose lease expired mid-run is refused: the reaper has already
// requeued the job and another worker may be running it, so accepting a late
// result could deliver one worker's output for a job another is still doing.
func holdsLease(j core.Job, workerID string, now time.Time) bool {
	if j.State != core.JobProcessing || j.WorkerID != workerID {
		return false
	}
	return j.LeaseExpiresAt.IsZero() || j.LeaseExpiresAt.After(now)
}

// Complete accepts a worker's output for the stage it holds.
//
// If another stage remains, the output becomes the next stage's input blob and
// the job is requeued under the next label — WITHOUT notifying the client, since
// a pipeline is one job from the client's point of view. Only the last stage
// produces a result and a `ready`.
func (s *Service) Complete(ctx context.Context, workerID, jobID string, out []string, now time.Time) error {
	job, err := s.repo.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if !holdsLease(job, workerID, now) {
		return core.ErrConflict
	}
	// The mirror of CompleteRaw's check, and it has to be here rather than at the
	// HTTP boundary: the job's STAMPED mode decides the result's shape, so a
	// worker posting a unit list for a raw job is refused whatever Content-Type
	// it announced. Without this pair, the header would be choosing the mode.
	if job.Raw {
		return fmt.Errorf("%w: job %s is a raw job and its result must be an opaque body",
			core.ErrModeMismatch, jobID)
	}

	rate, err := s.repo.RateForLabel(ctx, job.Label)
	if err != nil {
		return err
	}
	accrue := len(out) * rate

	next, more := job.NextLabel()
	if more {
		// The bridge between stages is a blob, because the next stage's contract
		// is `-i <file>`. One element is written verbatim; several are joined by
		// a newline.
		if _, err := s.blobs.PutBytes(job.ID, []byte(joinUnits(out))); err != nil {
			return err
		}
		if err := s.repo.AdvanceStage(ctx, job.ID, next, accrue, now); err != nil {
			return err
		}
		if !job.HasBlob {
			// A params-only job now HAS a blob: its first stage produced one.
			if err := s.repo.MarkHasBlob(ctx, job.ID); err != nil {
				return err
			}
		}
		s.counter.Inc(metricStageAdvances, nil)
		// Paired with the counter deliberately: a pipeline that stops advancing
		// looks exactly like a slow one, and the count says it happened while the
		// line says which job moved to which label after how long.
		s.logTransition(job, core.JobProcessing, core.JobQueued, job.UpdatedAt,
			"router", workerID, "", now)
		s.publishWork(next)
		s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: job.ID})
		return nil
	}

	s.results.Put(job.ID, out)
	if err := s.repo.CompleteJob(ctx, job.ID, len(out), accrue, now); err != nil {
		// The result would otherwise sit in memory for a job that is not `done`.
		s.results.Drop(job.ID)
		return err
	}
	s.logTransition(job, core.JobProcessing, core.JobDone, job.UpdatedAt,
		"worker", workerID, "", now)
	s.bus.Publish(bus.UserTopic(job.UserID), bus.Event{
		Kind: bus.KindReady, JobID: job.ID, Units: len(out),
	})
	s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: job.ID})
	return nil
}

// CompleteRaw accepts a worker's opaque output for the stage it holds.
//
// The bytes are STREAMED into the blob store and never held as a value: not a
// string, not a []string, and never through encoding/json — which replaces
// invalid UTF-8 with U+FFFD, returns no error, and changes the length. That is
// the defect ADR-0006 exists to remove.
//
// ⚠ THE LEASE CHECK IS THE SAME ONE Complete MAKES, and it is not optional
// duplication: it is what stops one customer's worker writing another's result,
// and a second write path is exactly where such a guard gets forgotten.
//
// Pricing is deliberately NOT decided here — a raw job's flat credit is T6's,
// and until then a raw job accrues what an empty unit list accrues.
func (s *Service) CompleteRaw(ctx context.Context, workerID, jobID string, body io.Reader, now time.Time) error {
	job, err := s.repo.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if !holdsLease(job, workerID, now) {
		return core.ErrConflict
	}
	if !job.Raw {
		return fmt.Errorf("%w: job %s is a units job and its result must be a JSON unit list",
			core.ErrModeMismatch, jobID)
	}

	if _, err := s.blobs.PutResult(job.ID, body); err != nil {
		return err
	}

	// A raw stage's output IS a blob, so the bridge between stages is a MOVE
	// rather than a render. joinUnits stops being involved entirely on this path,
	// which removes its lossy newline encoding from the one case where it would
	// corrupt a payload rather than merely reshape one.
	if next, more := job.NextLabel(); more {
		return s.bridgeRawStage(ctx, job, next, workerID, now)
	}

	if err := s.repo.CompleteJob(ctx, job.ID, 1, job.AccruedCredits, now); err != nil {
		// The result blob would otherwise sit on disk for a job that is not done.
		_ = s.blobs.DeleteResult(job.ID)
		return err
	}
	s.logTransition(job, core.JobProcessing, core.JobDone, job.UpdatedAt,
		"worker", workerID, "", now)
	s.bus.Publish(bus.UserTopic(job.UserID), bus.Event{
		Kind: bus.KindReady, JobID: job.ID, Units: 1,
	})
	s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: job.ID})
	return nil
}

// bridgeRawStage hands a raw stage's output to the next stage as its input.
//
// The bridge is a MOVE of the result blob onto the input key, not a render:
// joinUnits is never reached, so its newline joining — documented as lossy, and
// for binary actively wrong — cannot touch the payload.
//
// ⚠ The NEXT stage's mode is validated here rather than at admission. ADR-0001
// deliberately checks only pipeline[0] against live workers, because a later
// service may legitimately deploy after the job is queued; the same reasoning
// applies to its mode. Checking it at the moment of advance is what stops a job
// queueing forever under a label that will refuse every worker.
func (s *Service) bridgeRawStage(ctx context.Context, job core.Job, next, workerID string, now time.Time) error {
	_, nextRaw, err := s.repo.ServiceMode(ctx, next)
	if err != nil {
		return err
	}
	if !nextRaw {
		// Fail the job with a reason a human can act on. Left queued instead, it
		// would sit under a label whose every worker is refused, with nothing
		// saying why.
		return s.failJob(ctx, job, "router",
			fmt.Sprintf("stage %q is a units service and cannot accept a raw stage's output", next),
			nil, now)
	}

	src, err := s.blobs.OpenResult(job.ID)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := s.blobs.Put(job.ID, src); err != nil {
		return err
	}
	// Only after the input is safely written: the staged-write-and-rename in Put
	// has committed, so the result is no longer the only copy.
	_ = s.blobs.DeleteResult(job.ID)

	if err := s.repo.AdvanceStage(ctx, job.ID, next, job.AccruedCredits, now); err != nil {
		return err
	}
	if !job.HasBlob {
		// A params-only raw job now HAS a blob: its first stage produced one.
		if err := s.repo.MarkHasBlob(ctx, job.ID); err != nil {
			return err
		}
	}
	s.counter.Inc(metricStageAdvances, nil)
	s.logTransition(job, core.JobProcessing, core.JobQueued, job.UpdatedAt,
		"router", workerID, "", now)
	s.publishWork(next)
	s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: job.ID})
	return nil
}

// DeliverRaw hands a finished raw result to its owner and charges for it.
//
// It mirrors Deliver's ordering exactly, with one difference forced by the
// payload: the result is a FILE, so the caller streams it and this returns the
// open handle. The charge commits BEFORE the handle is returned, and the blob is
// removed by the caller once the copy succeeds — a result deleted before the
// transaction committed would be unrecoverable, while an orphan file is not.
func (s *Service) DeliverRaw(ctx context.Context, userID, jobID string, now time.Time) (*os.File, error) {
	job, err := s.repo.JobByID(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.UserID != userID {
		// Not ErrForbidden: a caller must not learn that someone else's job id
		// exists.
		return nil, core.ErrNotFound
	}
	if !job.Raw {
		return nil, core.ErrNotFound
	}

	f, err := s.blobs.OpenResult(jobID)
	if err != nil {
		return nil, err
	}

	// ⚠ FLAT ONE CREDIT, and asserted as the NUMBER rather than "a charge
	// happened". A raw job has no units, so falling through to the accrued
	// per-unit total charges ZERO — a failure in the customer's favour that
	// nothing anywhere would report. The MOMENT is unchanged: on delivery, in
	// this transaction, exactly as a units job.
	const rawCost = 1
	if err := s.repo.DeliverJob(ctx, jobID, userID, rawCost, now); err != nil {
		_ = f.Close()
		return nil, err
	}

	s.counter.Inc(metricJobsTotal, map[string]string{"state": string(core.JobDelivered)})
	s.counter.Inc(metricRawJobs, map[string]string{"label": job.Label})
	s.counter.Add(metricCreditsDebited, nil, int64(rawCost))
	s.logTransition(job, job.State, core.JobDelivered, job.UpdatedAt, "client", "", "", now)
	_ = s.blobs.Delete(jobID)
	s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: jobID})
	return f, nil
}

// DropRawResult removes a delivered raw result. Called once the caller has
// finished streaming it, so a failed copy leaves the file for a retry rather
// than destroying it mid-flight.
func (s *Service) DropRawResult(jobID string) { _ = s.blobs.DeleteResult(jobID) }

// joinUnits renders a stage's output as the next stage's input.
//
// ⚠ This encoding is a choice, not a law: one element verbatim, several joined
// by a newline. It suits the common pipeline shape (one document in, one out)
// and is lossy for a stage whose elements themselves contain newlines. It is one
// function to change if that ever becomes real.
func joinUnits(out []string) string {
	if len(out) == 1 {
		return out[0]
	}
	return strings.Join(out, "\n")
}

// Fail records a worker's failure, retrying until the attempt budget is spent.
func (s *Service) Fail(ctx context.Context, workerID, jobID, reason string, exitCode *int, now time.Time) error {
	job, err := s.repo.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if !holdsLease(job, workerID, now) {
		return core.ErrConflict
	}
	return s.failJob(ctx, job, "worker", reason, exitCode, now)
}

// failJob retries or abandons a job.
//
// `actor` says who caused it — a worker reporting failure, or the reaper taking
// back an expired lease. The two are indistinguishable in the job row afterwards
// and mean entirely different things to whoever is debugging.
func (s *Service) failJob(ctx context.Context, job core.Job, actor, reason string, exitCode *int, now time.Time) error {
	if job.Attempts+1 < s.cfg.MaxAttempts {
		if err := s.repo.RequeueJob(ctx, job.ID, reason, exitCode, now); err != nil {
			return err
		}
		// The retry is the transition nothing else records: a job that succeeds
		// on attempt 3 looks identical in the metrics to one that succeeded
		// first time.
		s.logFailureTransition(job, job.State, core.JobQueued, job.UpdatedAt,
			actor, job.WorkerID, reason, exitCode, now)
		s.publishWork(job.Label)
		s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: job.ID})
		return nil
	}

	s.counter.Inc(metricJobsTotal, map[string]string{"state": string(core.JobDead)})
	if err := s.repo.FailJobDead(ctx, job.ID, reason, exitCode, now); err != nil {
		return err
	}
	// The line the counter cannot give you: which job, on which worker, for what
	// reason, at which attempt.
	s.logFailureTransition(job, job.State, core.JobDead, job.UpdatedAt,
		actor, job.WorkerID, reason, exitCode, now)
	// Terminal: neither blob has any further use and nothing was charged.
	_ = s.blobs.Delete(job.ID)
	_ = s.blobs.DeleteResult(job.ID)
	s.results.Drop(job.ID)
	s.bus.Publish(bus.UserTopic(job.UserID), bus.Event{
		Kind: bus.KindFailed, JobID: job.ID, Reason: reason,
	})
	s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: job.ID})
	return nil
}

// Deliver hands a finished result to its owner, and is the ONLY place a job's
// credits move.
//
// The result is taken from memory first: if it is gone — already delivered, or
// swept — nothing is charged and the caller gets core.ErrNotFound. The debit,
// the ledger row and done→delivered then commit together, so a second delivery
// updates zero rows and charges nothing.
func (s *Service) Deliver(ctx context.Context, userID, jobID string, now time.Time) (core.Result, error) {
	job, err := s.repo.JobByID(ctx, jobID)
	if err != nil {
		return core.Result{}, err
	}
	if job.UserID != userID {
		// Not ErrForbidden: a caller must not learn that someone else's job id
		// exists.
		return core.Result{}, core.ErrNotFound
	}

	res, err := s.results.Take(jobID)
	if err != nil {
		return core.Result{}, err
	}

	if err := s.repo.DeliverJob(ctx, jobID, userID, job.AccruedCredits, now); err != nil {
		// Put it back: the charge did not happen, so the result must remain
		// collectable rather than vanishing along with the failed transaction.
		s.results.Put(jobID, res.Units)
		return core.Result{}, err
	}

	// Only after the transaction commits. An orphan blob is recoverable; a
	// delivered job whose blob was deleted before a failed commit is not.
	s.counter.Inc(metricJobsTotal, map[string]string{"state": string(core.JobDelivered)})
	s.counter.Add(metricCreditsDebited, nil, int64(job.AccruedCredits))
	s.logTransition(job, job.State, core.JobDelivered, job.UpdatedAt,
		"client", "", "", now)
	_ = s.blobs.Delete(jobID)
	s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: jobID})
	return res, nil
}

// BacklogItem is one finished job waiting for its owner.
type BacklogItem struct {
	JobID string `json:"job_id"`
	Units int    `json:"units"`
}

// Backlog lists the results waiting for a user.
//
// This is what makes a dropped client self-healing: it does not need to have
// been listening when its job finished.
func (s *Service) Backlog(ctx context.Context, userID string) ([]BacklogItem, error) {
	jobs, err := s.repo.JobsByUserAndState(ctx, userID, core.JobDone)
	if err != nil {
		return nil, err
	}
	out := make([]BacklogItem, 0, len(jobs))
	for _, j := range jobs {
		// Only offer what a fetch would actually serve. A `done` row whose
		// result has been swept is about to be requeued, and listing it would
		// promise something the next call refuses.
		if _, ok := s.results.Peek(j.ID); !ok {
			continue
		}
		out = append(out, BacklogItem{JobID: j.ID, Units: j.Units})
	}
	return out, nil
}

// publishWork wakes the workers serving one label.
func (s *Service) publishWork(label string) {
	s.bus.Publish(bus.WorkerTopic(label), bus.Event{Kind: bus.KindWork, Label: label})
}

// noteLabel records that a label has a live worker right now.
func (s *Service) noteLabel(label string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labelSeen[label] = now
}

// CheckWorkerMode refuses a worker whose declared output mode disagrees with the
// service's admin-owned one.
//
// ⚠ THIS IS A SECURITY BOUNDARY, not a validation nicety. `raw` is a PRICE — a
// raw job costs a flat credit instead of len(units) × rate — so a worker able to
// declare its own mode is a worker able to set what customers are charged.
// ADR-0001 split label VALIDITY (derived from live workers, harmless if wrong)
// from PRICING (admin-owned, in service_rates) for exactly that reason, and
// ADR-0006 keeps the mode on the pricing side of the split.
//
// It CHECKS and records nothing, which is the whole of its job. An earlier draft
// also stamped the label into labelSeen on agreement; a mutation proved that
// line dead — Claim already stamps, and ObserveLabels derives the registry from
// live bus topics — so it was removed rather than given a test. The admin record
// stays the single authority on a mode and is re-read on every declaration: a
// second copy in the registry would be a value able to disagree with the one
// that decides the bill.
func (s *Service) CheckWorkerMode(ctx context.Context, label string, raw bool) error {
	_, wantRaw, err := s.repo.ServiceMode(ctx, label)
	if err != nil {
		return err
	}
	if raw != wantRaw {
		return fmt.Errorf("%w: worker declares %s for %q, which the operator has configured as %s",
			core.ErrModeMismatch, modeName(raw), label, modeName(wantRaw))
	}
	return nil
}

// modeName renders a mode for a human reading a refusal.
//
// The message names BOTH values on purpose: a worker that is refused forever is
// diagnosable from one log line only if that line says what it asked for and
// what the operator configured, so the reader knows which of the two to change.
func modeName(raw bool) string {
	if raw {
		return "raw"
	}
	return "units"
}

// ObserveLabels samples which labels currently have a subscribed worker.
//
// The registry is DERIVED from live workers rather than administered: adding a
// service is starting a worker, not filling in a form.
func (s *Service) ObserveLabels(now time.Time) {
	for _, topic := range s.bus.Topics() {
		if label, ok := strings.CutPrefix(topic, "workers:"); ok {
			s.noteLabel(label, now)
		}
	}
}

// AvailableLabels returns the services a client may ask for.
//
// A label stays available for LabelGrace after its last worker disconnects. That
// window is what stops a rolling worker restart turning into a burst of rejected
// uploads — the alternative, deriving strictly from the current instant, makes
// the same request succeed or fail depending on who happens to be connected.
func (s *Service) AvailableLabels(now time.Time) []string {
	s.ObserveLabels(now)
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.labelSeen))
	for label, seen := range s.labelSeen {
		if now.Sub(seen) <= s.cfg.LabelGrace {
			out = append(out, label)
		}
	}
	return out
}

func (s *Service) labelAvailable(label string, now time.Time) bool {
	for _, l := range s.AvailableLabels(now) {
		if l == label {
			return true
		}
	}
	return false
}
