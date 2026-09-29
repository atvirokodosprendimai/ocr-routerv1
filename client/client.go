// Package client submits one job to the router and waits for its result.
//
// It is the protocol and nothing else: it holds no flags, writes nothing to a
// terminal, and decides no exit codes. cmd/client does all of that, the same way
// cmd/worker wraps internal/agent.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

// Config is how to reach the router.
type Config struct {
	// RouterURL is the base address, e.g. https://ocr.example.com.
	RouterURL string
	// Token is the client's bearer token.
	Token string
	// HTTP is the client to use. Nil means http.DefaultClient — a caller that
	// needs a timeout, a proxy or a custom TLS config supplies one.
	//
	// ⚠ It must NOT carry a per-request Timeout: the SSE stream is held open for
	// the life of the job, and a Timeout on the client aborts it mid-wait. A
	// caller bounding the whole operation uses the context instead.
	HTTP *http.Client
}

// Input is the job to submit.
type Input struct {
	// Filename is the name sent with the upload. Ignored when Body is nil.
	Filename string
	// Body is the document. Nil is the CRAWLER shape: a params-only job whose
	// service fetches its own input, which ADR-0001 supports deliberately.
	Body io.Reader
	// Label names one service; Pipeline names several in order. Pipeline wins
	// when both are set, matching the router's own precedence.
	Label    string
	Pipeline []string
	// Params become subprocess flags on the worker.
	Params map[string]string
	// Raw asks for a raw service, whose output is opaque bytes rather than a
	// unit list (ADR-0006). It must agree with the service's admin-owned mode or
	// the router refuses the upload — a client cannot choose a mode any more
	// than a worker can, because the mode is a price.
	Raw bool
}

// Result is a finished job's output.
type Result struct {
	JobID string
	Units []string
	// Raw is a raw service's output, byte for byte. Exactly one of Units and Raw
	// is populated, decided by the RESPONSE's content type rather than by what
	// was requested — the two differ precisely when something is wrong, and that
	// is the case worth reporting instead of misreading.
	Raw []byte
}

// Stage names what the client is doing, for a progress callback.
type Stage string

const (
	StageUploading  Stage = "uploading"
	StageWaiting    Stage = "waiting"
	StageCollecting Stage = "collecting"
	StageDone       Stage = "done"
)

// Progress is called as the submission advances. Nil is fine.
type Progress func(Stage, string)

func (p Progress) report(s Stage, detail string) {
	if p != nil {
		p(s, detail)
	}
}

// FailedError is a job that ran and did not produce a result.
//
// ⚠ A TYPED ERROR, so a caller can exit differently without parsing prose. This
// is the difference between a client that reports "the job died" and one that
// writes an empty file and exits 0 — which turns a dead job into silent data
// loss in whatever pipeline called it.
type FailedError struct {
	JobID  string
	Reason string
}

func (e *FailedError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("job %s failed", e.JobID)
	}
	return fmt.Sprintf("job %s failed: %s", e.JobID, e.Reason)
}

// RetryableError is a failure worth trying again: rate limiting, a router that
// is down, a stream that died. Distinct from a fatal error because the caller's
// next action genuinely differs.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// Submit uploads a job and blocks until it produces a result or fails.
//
// ⚠ THE ORDERING IS THE WHOLE CORRECTNESS ARGUMENT, and it is the opposite of
// what reads naturally. The SSE stream is opened and ESTABLISHED before the
// upload is posted. Uploading first leaves a window in which a fast job finishes
// and fires `ready` into a stream nobody is holding — and the client then waits
// forever for an event that already happened. The window is small, which is
// worse than large: it passes every casual test and strands the caller in
// production on exactly the jobs that went well.
//
// "Established" means the `hello` frame has arrived, not that the socket
// connected: the router subscribes a stream to the bus inside its handler, so a
// dialled-but-unaccepted connection is not yet receiving anything.
func Submit(ctx context.Context, cfg Config, in Input, prog Progress) (Result, error) {
	httpc := cfg.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	base := strings.TrimSuffix(cfg.RouterURL, "/")

	// The stream is cancelled when we return, whatever happens.
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()

	frames, backlog, err := openStream(streamCtx, httpc, base, cfg.Token)
	if err != nil {
		return Result{}, err
	}

	prog.report(StageUploading, "")
	jobID, err := upload(ctx, httpc, base, cfg.Token, in)
	if err != nil {
		return Result{}, err
	}

	// A result that was ALREADY waiting when we connected. This is what makes a
	// re-run after a dropped stream correct rather than hopeful.
	if backlog[jobID] {
		return collect(ctx, httpc, base, cfg.Token, jobID, prog)
	}

	prog.report(StageWaiting, jobID)
	for {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()

		case f, ok := <-frames:
			if !ok {
				// The stream ended without our event. Retryable: the job may well
				// still be running, and a fresh Submit would find it in the
				// backlog.
				return Result{}, &RetryableError{Err: errors.New("the event stream closed before the job finished")}
			}

			switch f.Event {
			case "ready":
				var e event
				if err := f.decode(&e); err != nil {
					return Result{}, fmt.Errorf("decoding ready event: %w", err)
				}
				// ⚠ MATCH ON THE JOB ID. A customer with two jobs in flight gets
				// events for both on one stream; acting on the wrong one collects
				// somebody else's result or fails on their failure.
				if e.JobID != jobID {
					continue
				}
				return collect(ctx, httpc, base, cfg.Token, jobID, prog)

			case "failed":
				var e event
				if err := f.decode(&e); err != nil {
					return Result{}, fmt.Errorf("decoding failed event: %w", err)
				}
				if e.JobID != jobID {
					continue
				}
				return Result{}, &FailedError{JobID: jobID, Reason: e.Reason}

			case "backlog":
				// A later backlog frame can name our job if the stream
				// reconnected underneath us.
				var b backlogPayload
				if err := f.decode(&b); err != nil {
					continue
				}
				for _, j := range b.Jobs {
					if j.JobID == jobID {
						return collect(ctx, httpc, base, cfg.Token, jobID, prog)
					}
				}
			}
		}
	}
}

// openStream connects to GET /sse and waits for the hello frame.
//
// It returns the frame channel and whatever the backlog frame named, so the
// caller can answer "was my result already waiting" without a round trip.
func openStream(ctx context.Context, httpc *http.Client, base, token string) (<-chan frame, map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/sse", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := httpc.Do(req)
	if err != nil {
		return nil, nil, &RetryableError{Err: fmt.Errorf("opening the event stream: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, nil, classify(resp.StatusCode, "opening the event stream", body)
	}

	frames := make(chan frame, 32)
	go func() {
		defer resp.Body.Close()
		readFrames(resp.Body, frames)
	}()

	// ⚠ WAIT FOR hello. Until it arrives the router has not run its handler, so
	// the connection is dialled and not yet subscribed — and an upload sent now
	// is racing exactly the gap this function exists to close.
	backlog := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case f, ok := <-frames:
			if !ok {
				return nil, nil, &RetryableError{Err: errors.New("the event stream closed before it was established")}
			}
			switch f.Event {
			case "hello":
				return frames, backlog, nil
			case "backlog":
				// The router sends backlog immediately after hello; if the order
				// ever changes, record it and keep waiting.
				var b backlogPayload
				if err := f.decode(&b); err == nil {
					for _, j := range b.Jobs {
						backlog[j.JobID] = true
					}
				}
			}
		}
	}
}

// upload posts the job and returns its id.
func upload(ctx context.Context, httpc *http.Client, base, token string, in Input) (string, error) {
	q := url.Values{}
	if len(in.Pipeline) > 0 {
		q.Set("pipeline", strings.Join(in.Pipeline, ","))
	} else if in.Label != "" {
		q.Set("label", in.Label)
	}
	for k, v := range in.Params {
		q.Set(k, v)
	}
	// Sent in BOTH modes, never omitted. Absent would be read as units, which is
	// the same answer — but an explicit value makes a mismatch a disagreement
	// between two stated positions.
	q.Set("raw", rawParam(in.Raw))

	endpoint := base + "/upload"
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}

	var body io.Reader
	contentType := ""
	if in.Body != nil {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		name := in.Filename
		if name == "" {
			name = "input"
		}
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(fw, in.Body); err != nil {
			return "", fmt.Errorf("reading the input: %w", err)
		}
		if err := mw.Close(); err != nil {
			return "", err
		}
		body = &buf
		contentType = mw.FormDataContentType()
	}
	// With no body this sends nothing at all — ADR-0001's crawler shape, where
	// the job is its parameters and the service fetches its own input.

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := httpc.Do(req)
	if err != nil {
		return "", &RetryableError{Err: fmt.Errorf("uploading: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", classify(resp.StatusCode, "uploading", b)
	}

	var created struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", fmt.Errorf("decoding the upload response: %w", err)
	}
	if created.JobID == "" {
		return "", errors.New("the router accepted the upload and returned no job id")
	}
	return created.JobID, nil
}

// collect fetches the finished result.
//
// ⚠ THIS CALL CHARGES THE CREDITS and deletes the in-memory result, so it
// happens exactly once and only when the job is known to be ready.
func collect(ctx context.Context, httpc *http.Client, base, token, jobID string, prog Progress) (Result, error) {
	prog.report(StageCollecting, jobID)

	req, err := http.NewRequestWithContext(ctx, "GET", base+"/files/"+jobID, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpc.Do(req)
	if err != nil {
		return Result{}, &RetryableError{Err: fmt.Errorf("collecting the result: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Result{}, classify(resp.StatusCode, "collecting the result", b)
	}

	// ⚠ BRANCH ON WHAT THE SERVER SAID, never on what was requested. The two
	// disagree exactly when something is wrong — a raw request answered with
	// units, or the reverse — and that is the case worth surfacing rather than
	// misreading. Reading a JSON envelope as bytes would hand the caller a file
	// full of `{"job_id":…}` and call it a result.
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/octet-stream") {
		// io.ReadAll, not a decoder: these bytes never become a string and never
		// meet encoding/json, which is the whole of ADR-0006.
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return Result{}, fmt.Errorf("reading the result: %w", err)
		}
		prog.report(StageDone, jobID)
		return Result{JobID: jobID, Raw: raw}, nil
	}

	var out struct {
		Units []string `json:"units"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("decoding the result: %w", err)
	}

	prog.report(StageDone, jobID)
	return Result{JobID: jobID, Units: out.Units}, nil
}

// rawParam renders the requested mode for a query string.
//
// Sent in both modes rather than omitted for units: an explicit value makes a
// mismatch a disagreement between two stated positions rather than between a
// statement and a default.
func rawParam(raw bool) string {
	if raw {
		return "1"
	}
	return "0"
}

// classify turns an HTTP status into a fatal or retryable error.
//
// ⚠ The split is about what the CALLER SHOULD DO NEXT, not about error
// taxonomy. 402 is fatal because retrying against an empty balance never
// succeeds; 429 is retryable because waiting is exactly the right response. A
// single error type would collapse both into "it failed" and leave a script
// unable to tell a pointless retry from a correct one.
func classify(status int, what string, body []byte) error {
	msg := strings.TrimSpace(string(body))
	err := fmt.Errorf("%s: %s", what, describe(status, msg))

	switch {
	case status == http.StatusTooManyRequests:
		return &RetryableError{Err: err}
	case status >= 500:
		return &RetryableError{Err: err}
	default:
		// 400, 401, 403, 402, 404 — all need a human to change something.
		return err
	}
}

func describe(status int, msg string) string {
	if msg == "" {
		return http.StatusText(status)
	}
	return fmt.Sprintf("%s (%s)", msg, http.StatusText(status))
}
