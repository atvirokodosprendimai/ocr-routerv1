package router

import (
	"context"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// ReapReport says what one sweep did. Returned so the caller can count it as a
// metric and so tests can assert on it without reading the database.
type ReapReport struct {
	LeasesExpired int
	JobsExpired   int
	ResultsSwept  int
	JobsAbandoned int
}

// Reap is one pass of every clock-driven cleanup.
//
// The three sweeps live in ONE pass deliberately. They are all clock-driven
// passes over the same aggregate, and splitting them into separate tickers would
// mean three goroutines racing to write the same rows — which is the two-writer
// bug this whole design is shaped to avoid.
//
// It takes `now` rather than reading the clock so the tests drive it directly
// instead of sleeping.
func (s *Service) Reap(ctx context.Context, now time.Time) (ReapReport, error) {
	var rep ReapReport

	// Keep the live-label registry fresh even when nobody is uploading; without
	// this, the grace window would only ever be refreshed by traffic.
	s.ObserveLabels(now)

	// 1. Leases whose worker went silent. The job goes back to the queue with an
	//    attempt spent, or dies if the budget is gone.
	expiredLeases, err := s.repo.LeaseExpired(ctx, now)
	if err != nil {
		return rep, err
	}
	for _, job := range expiredLeases {
		rep.LeasesExpired++
		s.counter.Inc(metricReaperActions, map[string]string{"action": "lease-expired"})
		if job.Attempts+1 >= s.cfg.MaxAttempts {
			rep.JobsAbandoned++
		}
		// nil: a lease reclaimed from a vanished worker never ran to an exit.
		if err := s.failJob(ctx, job, "reaper", "lease expired", nil, now); err != nil {
			return rep, err
		}
	}

	// 2. Queued jobs past their deadline. Nothing is charged, the blob goes, and
	//    the owner is told why — an expired job that failed silently is
	//    indistinguishable from one still waiting.
	expiredJobs, err := s.repo.ExpireOverdue(ctx, now)
	if err != nil {
		return rep, err
	}
	for _, job := range expiredJobs {
		rep.JobsExpired++
		s.counter.Inc(metricReaperActions, map[string]string{"action": "deadline-expired"})
		s.counter.Inc(metricJobsTotal, map[string]string{"state": string(core.JobExpired)})
		s.logTransition(job, core.JobQueued, core.JobExpired, job.QueuedAt,
			"reaper", "", "deadline passed", now)
		// BOTH keys. A raw job has an input blob and an output blob, and a
		// deletion added to one path and forgotten on the other leaks silently
		// — which is why every terminal path below calls the same pair.
		_ = s.blobs.Delete(job.ID)
		_ = s.blobs.DeleteResult(job.ID)
		s.results.Drop(job.ID)
		s.bus.Publish(bus.UserTopic(job.UserID), bus.Event{
			Kind: bus.KindFailed, JobID: job.ID, Reason: "expired",
		})
	}

	// 3. Results nobody collected. The ids returned by Sweep are requeued — the
	//    blob is still on disk, so the work is simply redone. Dropping them
	//    instead would strand each job in `done` with no result, forever.
	for _, jobID := range s.results.Sweep() {
		rep.ResultsSwept++
		s.counter.Inc(metricReaperActions, map[string]string{"action": "result-swept"})
		job, err := s.repo.JobByID(ctx, jobID)
		if err != nil {
			// The job is gone; the orphaned result has already been removed by
			// the sweep itself, so there is nothing left to reconcile.
			continue
		}
		if job.State != core.JobDone {
			continue
		}
		// nil: a result swept for age never ran a command that exited.
		if err := s.repo.RequeueJob(ctx, jobID, "result expired before collection", nil, now); err != nil {
			return rep, err
		}
		s.logTransition(job, core.JobDone, core.JobQueued, job.UpdatedAt,
			"reaper", "", "result expired before collection", now)
		s.publishWork(job.Label)
	}

	// 3b. The same sweep for RAW results, which live on disk rather than in the
	//     result store and are therefore invisible to the loop above. Same
	//     outcome, deliberately: the input blob is still there, so the job is
	//     requeued and the work is redone rather than stranded in `done`.
	if s.cfg.ResultTTL > 0 {
		stale, err := s.repo.RawJobsDoneBefore(ctx, now.Add(-s.cfg.ResultTTL))
		if err != nil {
			return rep, err
		}
		for _, job := range stale {
			rep.ResultsSwept++
			s.counter.Inc(metricReaperActions, map[string]string{"action": "result-swept"})
			_ = s.blobs.DeleteResult(job.ID)
			if err := s.repo.RequeueJob(ctx, job.ID, "result expired before collection", nil, now); err != nil {
				return rep, err
			}
			s.logTransition(job, core.JobDone, core.JobQueued, job.UpdatedAt,
				"reaper", "", "result expired before collection", now)
			s.publishWork(job.Label)
		}
	}

	if rep.LeasesExpired+rep.JobsExpired+rep.ResultsSwept > 0 {
		s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin})
	}
	return rep, nil
}

// RecoverOnBoot returns every in-flight job to the queue.
//
// Leases and in-memory results both died with the previous process, so neither
// can be honoured. Nothing is charged and no blob is deleted: the source is
// still on disk and the work is simply redone. Because nothing is ever charged
// before delivery, a restart can only cost repeated work — never a double
// charge, and never a silently lost document.
func (s *Service) RecoverOnBoot(ctx context.Context, now time.Time) (int, error) {
	n, err := s.repo.ResetInFlightOnBoot(ctx, now)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		// Wake every label that now has work waiting. Without this the recovered
		// jobs would sit until the next upload or the next worker reconnect
		// happened to nudge them.
		depth, err := s.repo.QueueDepthByLabel(ctx)
		if err != nil {
			return n, err
		}
		for label := range depth {
			s.publishWork(label)
		}
	}
	return n, nil
}
