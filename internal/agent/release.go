package agent

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// releaseTimeout bounds the whole shutdown handover.
//
// ⚠ The process is ALREADY STOPPING. A release that hangs turns a one-second
// restart into a minute of waiting, which is the delay this feature exists to
// remove — so the bound is short and the failure mode is deliberately today's
// behaviour: the lease lapses and the reaper reclaims the job.
const releaseTimeout = 3 * time.Second

// inflight is the set of claims this worker currently holds a lease on.
//
// It exists only so shutdown knows what to hand back. The package comment says
// the agent holds no durable state and that is still true — this is in-memory,
// and losing it costs a lease expiry rather than a job.
//
// Keyed by the CLAIM (job id and lease token, ADR-0013), not the job id: two
// slots can hold the same job under two claims after a reclaim, and the stale
// slot finishing must not erase the current slot's entry.
type inflight struct {
	mu     sync.Mutex
	claims map[claim]struct{}
}

// claim is one lease: the job and the token the router gave for it.
type claim struct{ jobID, lease string }

func newInflight() *inflight { return &inflight{claims: map[claim]struct{}{}} }

func (i *inflight) add(jobID, lease string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.claims[claim{jobID, lease}] = struct{}{}
}

// done drops a claim whose outcome has already been reported. A reported job
// must NOT be released: the lease is gone, and the release would 409 for a job
// that completed perfectly well.
func (i *inflight) done(jobID, lease string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.claims, claim{jobID, lease})
}

func (i *inflight) snapshot() []claim {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]claim, 0, len(i.claims))
	for c := range i.claims {
		out = append(out, c)
	}
	return out
}

// releaseAll hands every held lease back, best effort.
//
// ⚠ It takes a FRESH context. The caller's is already cancelled — that is what
// triggered this — and a request built on it would fail before it left the
// process, which would look exactly like a release the router refused.
func (a *Agent) releaseAll() {
	claims := a.inflight.snapshot()
	if len(claims) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()

	for _, c := range claims {
		a.release(ctx, c)
	}
}

// release hands one lease back. Every failure is logged and none is fatal: the
// lease expiry is still the safety net, so the worst case of a failed release is
// the behaviour that existed before this route.
func (a *Agent) release(ctx context.Context, c claim) {
	req, err := a.newRequest(ctx, http.MethodPost, withLease("/release?job_id="+c.jobID, c.lease), nil)
	if err != nil {
		a.Log("job %s: building release: %v", c.jobID, err)
		return
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.Log("job %s: releasing: %v", c.jobID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		a.Log("job %s: router refused the release (%s)", c.jobID, resp.Status)
		return
	}
	a.inflight.done(c.jobID, c.lease)
	a.Log("job %s: handed back to the queue", c.jobID)
}
