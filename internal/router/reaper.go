package router

import (
	"context"
	"errors"
	"time"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/bus"
	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
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

	// 1. Leases whose worker went silent. The job goes back to the queue with a
	//    RECLAIM spent, or dies if the abandonment budget is gone.
	//
	//    ⚠ reclaimJob, not failJob, and this is the line ADR-0008 is about. The
	//    worker vanished — restarted, killed, off the network — which says
	//    nothing about whether its command works, so spending the retry budget
	//    here killed jobs that had never failed once.
	expiredLeases, err := s.repo.LeaseExpired(ctx, now)
	if err != nil {
		return rep, err
	}
	for _, job := range expiredLeases {
		rep.LeasesExpired++
		s.counter.Inc(metricReaperActions, map[string]string{"action": "lease-expired"})
		if job.Reclaims+1 >= s.cfg.MaxReclaims {
			rep.JobsAbandoned++
		}
		if err := s.reclaimJob(ctx, job, "lease expired, reclaimed", now); err != nil {
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

	// 3. Results nobody collected, units and raw alike: the job is requeued —
	//    the input blob is still on disk, so the work is simply redone. Dropping
	//    them instead would strand each job in `done` with no result, forever.
	//
	//    ⚠ DRIVEN BY THE DATABASE, not by the ids results.Sweep returns (F2,
	//    REPORT 2026-11-10). Sweep forgets every id it hands out, so one failed
	//    requeue used to strand the rest until a restart. Reselecting `done`
	//    rows past the TTL on every tick retries whatever failed last time, and
	//    a raw result — on disk, invisible to the result store — needs the same
	//    query anyway. Sweep stays only to free the memory.
	//
	//    No `ResultTTL > 0` gate: with a zero TTL the result store expires every
	//    result at once, and the cutoff `now` still requeues those jobs, as the
	//    old units loop did (F2 review).
	s.results.Sweep()
	var sweepErr error
	stale, err := s.repo.JobsDoneBefore(ctx, now.Add(-s.cfg.ResultTTL))
	if err != nil {
		return rep, err
	}
	for _, job := range stale {
		if !job.Raw {
			if _, live := s.results.Peek(job.ID); live {
				// Put back by a failed delivery, with a fresh expiry.
				continue
			}
		}
		const reason = "result expired before collection"
		if err := s.repo.RequeueDone(ctx, job.ID, reason, now); err != nil {
			// ErrNotFound: delivered since the read, nothing to do. Any other
			// error is kept and the loop goes on; the next tick reselects
			// this job.
			if !errors.Is(err, core.ErrNotFound) {
				sweepErr = errors.Join(sweepErr, err)
			}
			continue
		}
		rep.ResultsSwept++
		s.counter.Inc(metricReaperActions, map[string]string{"action": "result-swept"})
		// Only after the requeue: before it, the result is still the job's to
		// deliver. No results.Drop: Sweep already freed the expired entry, and a
		// Drop here could only remove the result of a rerun that won the race.
		_ = s.blobs.DeleteResult(job.ID)
		s.logTransition(job, core.JobDone, core.JobQueued, job.UpdatedAt,
			"reaper", "", reason, now)
		s.publishWork(job.Label)
	}

	if rep.LeasesExpired+rep.JobsExpired+rep.ResultsSwept > 0 {
		s.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin})
	}
	return rep, sweepErr
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
