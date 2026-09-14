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
	DefaultLabel string
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
}

// New builds the service.
func New(repo *store.Repo, blobs *blob.Store, res *results.Store, b *bus.Bus, cfg Config) *Service {
	if cfg.DefaultLabel == "" {
		cfg.DefaultLabel = "ocr"
	}
	return &Service{
		repo: repo, blobs: blobs, results: res, bus: b, cfg: cfg,
		labelSeen: make(map[string]time.Time),
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

	job := core.Job{
		ID:        core.NewID(),
		UserID:    userID,
		Filename:  in.Filename,
		Label:     pipeline[0],
		Pipeline:  pipeline,
		Stage:     0,
		Params:    in.Params,
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
	s.bus.Publish(bus.UserTopic(job.UserID), bus.Event{
		Kind: bus.KindReady, JobID: job.ID, Units: len(out),
	})
	s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: job.ID})
	return nil
}

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
func (s *Service) Fail(ctx context.Context, workerID, jobID, reason string, now time.Time) error {
	job, err := s.repo.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if !holdsLease(job, workerID, now) {
		return core.ErrConflict
	}
	return s.failJob(ctx, job, reason, now)
}

func (s *Service) failJob(ctx context.Context, job core.Job, reason string, now time.Time) error {
	if job.Attempts+1 < s.cfg.MaxAttempts {
		if err := s.repo.RequeueJob(ctx, job.ID, reason, now); err != nil {
			return err
		}
		s.publishWork(job.Label)
		s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: job.ID})
		return nil
	}

	if err := s.repo.FailJobDead(ctx, job.ID, reason, now); err != nil {
		return err
	}
	// Terminal: the blob has no further use and nothing was charged.
	_ = s.blobs.Delete(job.ID)
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
