// Package agent is the worker side of the protocol: it listens for work,
// claims it, runs it, and posts the result back.
//
// It holds no durable state. A worker can be killed at any instant and the
// router's lease expiry recovers whatever it was holding, so the agent never has
// to be careful about shutdown.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/runner"
)

// Config is how a worker is pointed at a router and a service.
type Config struct {
	RouterURL string
	Token     string
	// Label is the ONE service this process serves. Several services means
	// several processes: a bad command for one cannot then take down a process
	// serving the others, and the flag set stays two strings.
	Label  string
	TmpDir string
	// Slots bounds concurrent subprocesses, so the router's queue — not this
	// process's memory — is where work waits.
	Slots int
	// PollInterval is a floor on how often the agent claims when nothing is
	// pushing it. The SSE stream is the primary trigger; this is the safety net.
	PollInterval time.Duration
	// Raw means this worker's service emits opaque bytes (ADR-0006). It is
	// DECLARED to the router on every subscribe and claim, and the router refuses
	// the worker if it disagrees with the service's admin-owned mode — a worker
	// cannot set its own mode, because the mode is a price.
	Raw bool
}

// Agent runs the claim/run/report loop.
type Agent struct {
	cfg    Config
	run    runner.Runner
	client *http.Client
	// Log is where progress goes. Injectable so tests can stay quiet.
	Log func(format string, args ...any)
}

// New builds an agent.
func New(cfg Config, r runner.Runner) *Agent {
	if cfg.Slots < 1 {
		cfg.Slots = 1
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 30 * time.Second
	}
	return &Agent{
		cfg: cfg,
		run: r,
		// No overall timeout: this client opens the SSE stream, which is meant
		// to stay open indefinitely. Per-request bounds come from the context.
		client: &http.Client{},
		Log:    func(string, ...any) {},
	}
}

// Run is the agent's whole life: connect, listen, claim, repeat.
//
// It returns only when ctx is cancelled. A dropped stream is reconnected with
// capped exponential backoff and jitter — the jitter matters because a router
// restart drops EVERY worker at once, and without it the whole fleet would
// reconnect on the same tick and do it again on the next failure.
func (a *Agent) Run(ctx context.Context) error {
	slots := make(chan struct{}, a.cfg.Slots)
	for range a.cfg.Slots {
		slots <- struct{}{}
	}

	backoff := time.Second
	for ctx.Err() == nil {
		err := a.listen(ctx, slots)
		if ctx.Err() != nil {
			return nil
		}
		a.Log("stream ended (%v); reconnecting in %s", err, backoff)

		jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff + jitter):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}

// listen holds one SSE connection and drains work for as long as it lasts.
func (a *Agent) listen(ctx context.Context, slots chan struct{}) error {
	url := fmt.Sprintf("%s/sse?label=%s&raw=%s",
		strings.TrimRight(a.cfg.RouterURL, "/"), a.cfg.Label, rawParam(a.cfg.Raw))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream returned %s", resp.Status)
	}
	a.Log("connected to %s serving %q", a.cfg.RouterURL, a.cfg.Label)

	// Drain immediately: work may already be queued from before this worker
	// existed, and nothing will push an event for a job that is already waiting.
	a.drain(ctx, slots)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		name, ok := strings.CutPrefix(sc.Text(), "event: ")
		if !ok {
			continue
		}
		switch name {
		case "work", "backlog":
			a.drain(ctx, slots)
		case "ping":
			// ⚠ Claiming on PING too is the safety net that matters. The bus
			// drops a superseded event when a subscriber's buffer is full, so a
			// `work` notification can legitimately be lost — and without this a
			// worker would then sit idle until the next upload happened to
			// arrive. With it, a dropped event costs one ping interval.
			a.drain(ctx, slots)
		}
	}
	return sc.Err()
}

// drain claims and dispatches until there is no work or no free slot.
func (a *Agent) drain(ctx context.Context, slots chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-slots:
		default:
			return // every slot is busy; the queue can hold the rest
		}

		job, ok, err := a.claim(ctx)
		if err != nil || !ok {
			slots <- struct{}{}
			if err != nil {
				a.Log("claim failed: %v", err)
			}
			return
		}

		go func() {
			a.process(ctx, job)

			// Release the slot, then immediately look for more.
			//
			// ⚠ Without this re-drain the agent STALLS once every slot is busy:
			// drain returns when it cannot take a slot, and nothing else runs
			// until the next stream event arrives. With a batch queued before
			// this worker connected — or simply more jobs than slots — the
			// queue would sit untouched for up to a ping interval while the
			// worker is idle. The ping is the safety net; this is the fast path.
			slots <- struct{}{}
			a.drain(ctx, slots)
		}()
	}
}

// claimResponse mirrors the router's reply.
type claimResponse struct {
	JobID    string            `json:"job_id"`
	Label    string            `json:"label"`
	Filename string            `json:"filename"`
	Params   map[string]string `json:"params"`
	HasBlob  bool              `json:"has_blob"`
}

func (a *Agent) claim(ctx context.Context) (claimResponse, bool, error) {
	req, err := a.newRequest(ctx, http.MethodPost,
		"/claim?label="+a.cfg.Label+"&raw="+rawParam(a.cfg.Raw), nil)
	if err != nil {
		return claimResponse{}, false, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return claimResponse{}, false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		return claimResponse{}, false, nil // an empty queue is normal
	case http.StatusOK:
	default:
		return claimResponse{}, false, fmt.Errorf("claim returned %s", resp.Status)
	}

	var out claimResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return claimResponse{}, false, err
	}
	return out, true, nil
}

// process runs one job end to end and reports the outcome.
func (a *Agent) process(ctx context.Context, job claimResponse) {
	a.Log("job %s (%s) started", job.ID(), job.Label)

	inputPath, cleanup, err := a.materialise(ctx, job)
	// ⚠ The cleanup is deferred IMMEDIATELY and unconditionally, before any
	// error is handled. Every exit path from here — success, non-zero exit,
	// timeout, a panic in the runner — must remove the temp file, or a busy
	// worker fills its tmpdir with other people's documents.
	defer cleanup()
	if err != nil {
		a.report(ctx, job.JobID, nil, fmt.Sprintf("fetching input: %v", err))
		return
	}

	rj := runner.Job{ID: job.JobID, InputPath: inputPath, Params: job.Params}

	if a.cfg.Raw {
		out, runErr := a.run.RunRaw(ctx, rj)
		if runErr != nil {
			a.Log("job %s failed: %v", job.JobID, runErr)
			a.report(ctx, job.JobID, nil, runErr.Error())
			return
		}
		a.Log("job %s produced %d byte(s)", job.JobID, len(out))
		a.reportRaw(ctx, job.JobID, out)
		return
	}

	units, runErr := a.run.Run(ctx, rj)
	if runErr != nil {
		// A failed job is reported and the agent carries on. One malformed
		// document must not stop a worker serving every other customer.
		a.Log("job %s failed: %v", job.JobID, runErr)
		a.report(ctx, job.JobID, nil, runErr.Error())
		return
	}

	a.Log("job %s produced %d unit(s)", job.JobID, len(units))
	a.report(ctx, job.JobID, units, "")
}

// materialise downloads the job's source file, if it has one.
func (a *Agent) materialise(ctx context.Context, job claimResponse) (string, func(), error) {
	noop := func() {}
	if !job.HasBlob {
		// The crawler shape: the service fetches its own input from a parameter.
		return "", noop, nil
	}

	if err := os.MkdirAll(a.cfg.TmpDir, 0o700); err != nil {
		return "", noop, err
	}
	path := filepath.Join(a.cfg.TmpDir, job.JobID)

	// O_EXCL so a stale file from a previous run can never be silently reused as
	// though it were this job's input.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", noop, err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(path)
	}

	req, err := a.newRequest(ctx, http.MethodGet, "/files/"+job.JobID, nil)
	if err != nil {
		return "", cleanup, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", cleanup, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", cleanup, fmt.Errorf("download returned %s", resp.Status)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", cleanup, err
	}
	if err := f.Close(); err != nil {
		return "", cleanup, err
	}
	return path, cleanup, nil
}

type resultBody struct {
	JobID string   `json:"job_id"`
	Units []string `json:"units,omitempty"`
	Error string   `json:"error,omitempty"`
}

// rawParam renders the declared mode for a query string.
//
// Sent EXPLICITLY in both modes, including the units one. Omitting it would be
// read by the router as units, which is the same answer — but an explicit value
// is what makes a mismatch a disagreement between two stated positions rather
// than between a statement and a default.
func rawParam(raw bool) string {
	if raw {
		return "1"
	}
	return "0"
}

// reportRaw posts a raw job's stdout back as an opaque body.
//
// ⚠ THE BYTES DO NOT GO THROUGH JSON, and that is the entire point of ADR-0006.
// The units channel is []string end to end, and encoding/json replaces every
// invalid UTF-8 byte with U+FFFD — no error, different bytes, different length —
// so a PNG posted that way arrives corrupted and is stored, delivered and billed
// as though it were whole. The job id therefore rides the QUERY STRING, because
// the body is the payload and has no room for an envelope.
//
// A FAILURE still goes through report() as JSON in both modes: a failure is a
// reason string, which is text whatever the service emits, and giving failures
// two encodings would double the router's parse surface for nothing.
func (a *Agent) reportRaw(ctx context.Context, jobID string, out []byte) {
	// Same fresh, bounded context as report, and for the same reason: a result
	// that is never reported costs the customer a full lease timeout.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	req, err := a.newRequest(ctx, http.MethodPost, "/upload?job_id="+jobID, bytes.NewReader(out))
	if err != nil {
		a.Log("job %s: building raw report: %v", jobID, err)
		return
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := a.client.Do(req)
	if err != nil {
		a.Log("job %s: reporting: %v", jobID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		a.Log("job %s: router rejected the raw report (%s)", jobID, resp.Status)
	}
}

// report posts the outcome back to the router.
func (a *Agent) report(ctx context.Context, jobID string, units []string, failure string) {
	// A fresh context with its own bound: the caller's may already be cancelled
	// (a shutting-down worker), and a result that is not reported costs the
	// customer a full lease timeout before anyone retries.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	body, err := json.Marshal(resultBody{JobID: jobID, Units: units, Error: failure})
	if err != nil {
		a.Log("job %s: encoding result: %v", jobID, err)
		return
	}
	req, err := a.newRequest(ctx, http.MethodPost, "/upload", bytes.NewReader(body))
	if err != nil {
		a.Log("job %s: building report: %v", jobID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		a.Log("job %s: reporting: %v", jobID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		a.Log("job %s: router rejected the report (%s)", jobID, resp.Status)
	}
}

func (a *Agent) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method,
		strings.TrimRight(a.cfg.RouterURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	return req, nil
}

// ID is a small convenience for logging.
func (c claimResponse) ID() string { return c.JobID }
