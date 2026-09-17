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

// inflight is the set of jobs this worker currently holds a lease on.
//
// It exists only so shutdown knows what to hand back. The package comment says
// the agent holds no durable state and that is still true — this is in-memory,
// and losing it costs a lease expiry rather than a job.
type inflight struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func newInflight() *inflight { return &inflight{ids: map[string]struct{}{}} }

func (i *inflight) add(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ids[id] = struct{}{}
}

// done drops a job whose outcome has already been reported. A reported job must
// NOT be released: the lease is gone, and the release would 409 for a job that
// completed perfectly well.
func (i *inflight) done(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.ids, id)
}

func (i *inflight) snapshot() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]string, 0, len(i.ids))
	for id := range i.ids {
		out = append(out, id)
	}
	return out
}

// releaseAll hands every held lease back, best effort.
//
// ⚠ It takes a FRESH context. The caller's is already cancelled — that is what
// triggered this — and a request built on it would fail before it left the
// process, which would look exactly like a release the router refused.
func (a *Agent) releaseAll() {
	ids := a.inflight.snapshot()
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()

	for _, id := range ids {
		a.release(ctx, id)
	}
}

// release hands one lease back. Every failure is logged and none is fatal: the
// lease expiry is still the safety net, so the worst case of a failed release is
// the behaviour that existed before this route.
func (a *Agent) release(ctx context.Context, jobID string) {
	req, err := a.newRequest(ctx, http.MethodPost, "/release?job_id="+jobID, nil)
	if err != nil {
		a.Log("job %s: building release: %v", jobID, err)
		return
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.Log("job %s: releasing: %v", jobID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		a.Log("job %s: router refused the release (%s)", jobID, resp.Status)
		return
	}
	a.inflight.done(jobID)
	a.Log("job %s: handed back to the queue", jobID)
}
