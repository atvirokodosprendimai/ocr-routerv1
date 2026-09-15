package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/client"
)

// fakeRouter is a test double for the three endpoints Submit drives.
//
// It records the ORDER of the requests it received, which is the only way to
// assert the one property this record rests on.
type fakeRouter struct {
	srv *httptest.Server

	mu    sync.Mutex
	order []string
	// uploads counts POST /upload, so a test can assert nothing was uploaded.
	uploads int

	// frames is written by a test to drive the stream.
	frames chan string
	// uploadStatus and uploadBody override the upload response.
	uploadStatus int
	// collectStatus overrides the GET /files response.
	collectStatus int
	// units is what a successful collect returns.
	units []string
	// backlogJobs is sent in the backlog frame on connect.
	backlogJobs []string
	// helloDelay stalls the hello frame, widening the race the ordering closes.
	helloDelay time.Duration
	// jobID is what the upload returns.
	jobID string
}

func newFakeRouter(t *testing.T) *fakeRouter {
	t.Helper()
	f := &fakeRouter{
		frames:        make(chan string, 16),
		uploadStatus:  http.StatusCreated,
		collectStatus: http.StatusOK,
		units:         []string{"page one", "page two"},
		jobID:         "job-1",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		f.record("GET /sse")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		if f.helloDelay > 0 {
			time.Sleep(f.helloDelay)
		}
		fmt.Fprintf(w, "event: hello\ndata: {\"role\":\"client\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}

		jobs := make([]map[string]any, 0, len(f.backlogJobs))
		for _, id := range f.backlogJobs {
			jobs = append(jobs, map[string]any{"job_id": id, "units": 1})
		}
		b, _ := json.Marshal(map[string]any{"jobs": jobs})
		fmt.Fprintf(w, "event: backlog\ndata: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case raw, ok := <-f.frames:
				if !ok {
					return
				}
				_, _ = io.WriteString(w, raw)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	})

	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		f.record("POST /upload")
		f.mu.Lock()
		f.uploads++
		status := f.uploadStatus
		f.mu.Unlock()

		if status != http.StatusCreated {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"nope"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": f.jobID})
	})

	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		f.record("GET " + r.URL.Path)
		if f.collectStatus != http.StatusOK {
			w.WriteHeader(f.collectStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"units": f.units})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRouter) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, s)
}

func (f *fakeRouter) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *fakeRouter) uploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads
}

// send queues a raw SSE frame.
func (f *fakeRouter) send(event string, payload any) {
	b, _ := json.Marshal(payload)
	f.frames <- fmt.Sprintf("event: %s\ndata: %s\n\n", event, b)
}

func (f *fakeRouter) cfg() client.Config {
	return client.Config{RouterURL: f.srv.URL, Token: "ocr_c_test", HTTP: f.srv.Client()}
}

// ctx gives every test a deadline.
//
// ⚠ Submit BLOCKS by design, so a regression that stops it returning would hang
// the suite forever — and a hung suite is the failure that gets a test deleted
// rather than fixed. With a deadline it fails instead.
func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// TestStreamOpensBeforeUpload is ADR-0005's `Enforced-by:` check.
//
// ⚠ It asserts the ORDER, not that both were called. Uploading first leaves a
// window in which a fast job finishes and fires `ready` into a stream nobody is
// holding; the client then waits forever for an event that already happened. The
// window is small, which is worse than large — it passes every casual test and
// strands the caller in production on exactly the jobs that went well.
//
// The hello delay widens that window deliberately: with it, a client that
// uploads first would upload well before the stream exists.
func TestStreamOpensBeforeUpload(t *testing.T) {
	f := newFakeRouter(t)
	f.helloDelay = 150 * time.Millisecond

	go func() {
		time.Sleep(300 * time.Millisecond)
		f.send("ready", map[string]any{"job_id": "job-1", "units": 2})
	}()

	if _, err := client.Submit(ctx(t), f.cfg(),
		client.Input{Filename: "a.pdf", Body: strings.NewReader("doc")}, nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	order := f.seen()
	sse, upload := -1, -1
	for i, s := range order {
		if s == "GET /sse" && sse < 0 {
			sse = i
		}
		if s == "POST /upload" && upload < 0 {
			upload = i
		}
	}
	if sse < 0 || upload < 0 {
		t.Fatalf("both requests did not happen: %v", order)
	}
	if sse > upload {
		t.Fatalf("the upload was posted BEFORE the stream was opened (%v). A job that finishes "+
			"in that window fires `ready` into a stream nobody is holding, and the client waits "+
			"forever for an event that already happened", order)
	}
}

func TestSubmitReturnsUnits(t *testing.T) {
	f := newFakeRouter(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.send("ready", map[string]any{"job_id": "job-1"})
	}()

	res, err := client.Submit(ctx(t), f.cfg(),
		client.Input{Filename: "a.pdf", Body: strings.NewReader("doc")}, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.JobID != "job-1" {
		t.Errorf("job id = %q", res.JobID)
	}
	if len(res.Units) != 2 || res.Units[0] != "page one" {
		t.Errorf("units = %v", res.Units)
	}
}

func TestUploadSendsTheFileAsMultipart(t *testing.T) {
	var gotBody string
	var gotType string
	f := newFakeRouter(t)
	// Replace the upload handler so we can inspect the request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			fmt.Fprint(w, "event: hello\ndata: {}\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(80 * time.Millisecond)
			fmt.Fprint(w, "event: ready\ndata: {\"job_id\":\"job-1\"}\n\n")
			if fl != nil {
				fl.Flush()
			}
			<-r.Context().Done()
		case "/upload":
			gotType = r.Header.Get("Content-Type")
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"job_id": "job-1"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"units": []string{"x"}})
		}
	}))
	defer srv.Close()
	_ = f

	cfg := client.Config{RouterURL: srv.URL, Token: "t", HTTP: srv.Client()}
	if _, err := client.Submit(ctx(t), cfg,
		client.Input{Filename: "scan.pdf", Body: strings.NewReader("the document")}, nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if !strings.HasPrefix(gotType, "multipart/form-data") {
		t.Errorf("content type = %q, want multipart", gotType)
	}
	if !strings.Contains(gotBody, "the document") {
		t.Error("the document bytes did not reach the server")
	}
	if !strings.Contains(gotBody, `name="file"`) {
		t.Errorf("the part is not named file:\n%s", gotBody)
	}
	if !strings.Contains(gotBody, "scan.pdf") {
		t.Error("the filename did not reach the server")
	}
}

// TestParamsOnlyUploadSendsNoBody is ADR-0001's crawler shape.
func TestParamsOnlyUploadSendsNoBody(t *testing.T) {
	var gotQuery url.Values
	var bodyLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			fmt.Fprint(w, "event: hello\ndata: {}\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(80 * time.Millisecond)
			fmt.Fprint(w, "event: ready\ndata: {\"job_id\":\"job-1\"}\n\n")
			if fl != nil {
				fl.Flush()
			}
			<-r.Context().Done()
		case "/upload":
			gotQuery = r.URL.Query()
			b, _ := io.ReadAll(r.Body)
			bodyLen = int64(len(b))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"job_id": "job-1"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"units": []string{"x"}})
		}
	}))
	defer srv.Close()

	cfg := client.Config{RouterURL: srv.URL, Token: "t", HTTP: srv.Client()}
	if _, err := client.Submit(ctx(t), cfg, client.Input{
		Label:  "crawl",
		Params: map[string]string{"url": "https://example.com/doc"},
	}, nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if bodyLen != 0 {
		t.Errorf("a params-only job sent a %d-byte body; the crawler shape has no document",
			bodyLen)
	}
	if gotQuery.Get("url") != "https://example.com/doc" {
		t.Errorf("the url param did not reach the query: %v", gotQuery)
	}
	if gotQuery.Get("label") != "crawl" {
		t.Errorf("label = %q", gotQuery.Get("label"))
	}
}

func TestLabelAndPipelineReachTheQuery(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			fmt.Fprint(w, "event: hello\ndata: {}\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(80 * time.Millisecond)
			fmt.Fprint(w, "event: ready\ndata: {\"job_id\":\"job-1\"}\n\n")
			if fl != nil {
				fl.Flush()
			}
			<-r.Context().Done()
		case "/upload":
			gotQuery = r.URL.Query()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"job_id": "job-1"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"units": []string{"x"}})
		}
	}))
	defer srv.Close()

	cfg := client.Config{RouterURL: srv.URL, Token: "t", HTTP: srv.Client()}
	// Pipeline wins over label, matching the router's own precedence.
	if _, err := client.Submit(ctx(t), cfg, client.Input{
		Body: strings.NewReader("x"), Label: "ocr", Pipeline: []string{"crawl", "strip-html"},
	}, nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if gotQuery.Get("pipeline") != "crawl,strip-html" {
		t.Errorf("pipeline = %q", gotQuery.Get("pipeline"))
	}
	if gotQuery.Get("label") != "" {
		t.Errorf("label was sent alongside pipeline (%q); the router treats pipeline as the more "+
			"specific of the two and sending both invites a disagreement", gotQuery.Get("label"))
	}
}

func TestFailedEventReturnsTypedError(t *testing.T) {
	f := newFakeRouter(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.send("failed", map[string]any{"job_id": "job-1", "reason": "attempts exhausted"})
	}()

	_, err := client.Submit(ctx(t), f.cfg(),
		client.Input{Body: strings.NewReader("x")}, nil)

	var fe *client.FailedError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %T %v, want *client.FailedError — a typed error is what lets a caller "+
			"exit differently without parsing prose", err, err)
	}
	if fe.Reason != "attempts exhausted" {
		t.Errorf("reason = %q", fe.Reason)
	}
	if fe.JobID != "job-1" {
		t.Errorf("job id = %q", fe.JobID)
	}
}

// TestIgnoresEventsForOtherJobs sends the foreign events FIRST.
//
// ⚠ Sending them afterwards would prove nothing: the client would already have
// returned on its own event.
func TestIgnoresEventsForOtherJobs(t *testing.T) {
	f := newFakeRouter(t)
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.send("ready", map[string]any{"job_id": "someone-elses-job"})
		f.send("failed", map[string]any{"job_id": "another-job", "reason": "not ours"})
		time.Sleep(30 * time.Millisecond)
		f.send("ready", map[string]any{"job_id": "job-1"})
	}()

	res, err := client.Submit(ctx(t), f.cfg(), client.Input{Body: strings.NewReader("x")}, nil)
	if err != nil {
		t.Fatalf("Submit returned %v — a foreign job's event was acted on, so a customer with "+
			"two jobs in flight collects the wrong result or fails on someone else's failure", err)
	}
	if res.JobID != "job-1" {
		t.Errorf("collected %q", res.JobID)
	}
	// The foreign ready must not have triggered a collect of that job.
	for _, s := range f.seen() {
		if strings.Contains(s, "someone-elses-job") {
			t.Errorf("the client fetched another job's result: %v", f.seen())
		}
	}
}

// TestBacklogOnConnectIsHonoured sends NO live event at all.
//
// This is the reconnection path: a client whose stream dropped mid-wait re-runs,
// and the backlog frame is the only thing that tells it the result is ready.
func TestBacklogOnConnectIsHonoured(t *testing.T) {
	f := newFakeRouter(t)
	f.backlogJobs = []string{"job-1"}
	// Deliberately no f.send — if the client needs a live event, this hangs and
	// the context deadline fails it.

	res, err := client.Submit(ctx(t), f.cfg(), client.Input{Body: strings.NewReader("x")}, nil)
	if err != nil {
		t.Fatalf("Submit: %v — a result already waiting at connect time was not collected, so a "+
			"client whose stream dropped can never recover it", err)
	}
	if len(res.Units) != 2 {
		t.Errorf("units = %v", res.Units)
	}
}

// TestFrameSplitAcrossWrites — a reader that treats each read as a frame drops
// events under exactly the load that makes them matter.
func TestFrameSplitAcrossWrites(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			fmt.Fprint(w, "event: hello\ndata: {}\n\n")
			fl.Flush()
			time.Sleep(60 * time.Millisecond)
			// One frame, TWO writes with a flush between.
			fmt.Fprint(w, "event: ready\n")
			fl.Flush()
			time.Sleep(30 * time.Millisecond)
			fmt.Fprint(w, "data: {\"job_id\":\"job-1\"}\n\n")
			fl.Flush()
			<-r.Context().Done()
		case "/upload":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"job_id": "job-1"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"units": []string{"ok"}})
		}
	}))
	defer srv.Close()

	cfg := client.Config{RouterURL: srv.URL, Token: "t", HTTP: srv.Client()}
	res, err := client.Submit(ctx(t), cfg, client.Input{Body: strings.NewReader("x")}, nil)
	if err != nil {
		t.Fatalf("a frame split across two writes was not reassembled: %v", err)
	}
	if len(res.Units) != 1 {
		t.Errorf("units = %v", res.Units)
	}
}

// TestTwoFramesInOneWrite is the other half of the same reassembly bug.
func TestTwoFramesInOneWrite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			fmt.Fprint(w, "event: hello\ndata: {}\n\n")
			fl.Flush()
			time.Sleep(60 * time.Millisecond)
			// A foreign event and ours, in ONE write.
			fmt.Fprint(w,
				"event: ready\ndata: {\"job_id\":\"other\"}\n\n"+
					"event: ready\ndata: {\"job_id\":\"job-1\"}\n\n")
			fl.Flush()
			<-r.Context().Done()
		case "/upload":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"job_id": "job-1"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"units": []string{"ok"}})
		}
	}))
	defer srv.Close()

	cfg := client.Config{RouterURL: srv.URL, Token: "t", HTTP: srv.Client()}
	if _, err := client.Submit(ctx(t), cfg, client.Input{Body: strings.NewReader("x")}, nil); err != nil {
		t.Fatalf("two frames in one write were not both parsed: %v", err)
	}
}

// TestRetryableUploadStatuses is table-driven because one case proves one branch
// of a five-way decision.
func TestRetryableUploadStatuses(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
		why       string
	}{
		{http.StatusTooManyRequests, true, "waiting is exactly the right response"},
		{http.StatusServiceUnavailable, true, "the router is down, not the request wrong"},
		{http.StatusUnauthorized, false, "retrying a bad credential forever is the wrong action"},
		{http.StatusPaymentRequired, false, "retrying against an empty balance never succeeds"},
		{http.StatusBadRequest, false, "a human has to change the request"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := newFakeRouter(t)
			f.uploadStatus = tc.status

			_, err := client.Submit(ctx(t), f.cfg(), client.Input{Body: strings.NewReader("x")}, nil)
			if err == nil {
				t.Fatal("no error")
			}
			var re *client.RetryableError
			got := errors.As(err, &re)
			if got != tc.retryable {
				t.Errorf("retryable = %v, want %v — %s", got, tc.retryable, tc.why)
			}
		})
	}
}

func TestProgressCallbackSequence(t *testing.T) {
	f := newFakeRouter(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.send("ready", map[string]any{"job_id": "job-1"})
	}()

	var mu sync.Mutex
	var stages []client.Stage
	prog := func(s client.Stage, _ string) {
		mu.Lock()
		defer mu.Unlock()
		stages = append(stages, s)
	}

	if _, err := client.Submit(ctx(t), f.cfg(), client.Input{Body: strings.NewReader("x")}, prog); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []client.Stage{
		client.StageUploading, client.StageWaiting,
		client.StageCollecting, client.StageDone,
	}
	if len(stages) != len(want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Errorf("stage %d = %q, want %q", i, stages[i], want[i])
		}
	}
}

func TestSubmitHonoursContextCancellation(t *testing.T) {
	f := newFakeRouter(t)
	// No event is ever sent, so only cancellation can end this.

	c, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := client.Submit(c, f.cfg(), client.Input{Body: strings.NewReader("x")}, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Submit ignored a cancelled context — a caller who gave up has no way out")
	}
}

// TestCollectFailureIsNotSilent — empty units and a nil error is the shape that
// loses data downstream.
func TestCollectFailureIsNotSilent(t *testing.T) {
	f := newFakeRouter(t)
	f.collectStatus = http.StatusInternalServerError
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.send("ready", map[string]any{"job_id": "job-1"})
	}()

	res, err := client.Submit(ctx(t), f.cfg(), client.Input{Body: strings.NewReader("x")}, nil)
	if err == nil {
		t.Fatalf("a failed collect returned no error and %d units — an empty result that reads "+
			"as success is what turns a router problem into silent data loss", len(res.Units))
	}
	var re *client.RetryableError
	if !errors.As(err, &re) {
		t.Errorf("a 500 on collect should be retryable, got %T", err)
	}
}

func TestNoUploadWhenTheStreamCannotOpen(t *testing.T) {
	f := newFakeRouter(t)
	// A router that refuses the stream: the upload must not happen, or the
	// customer is charged for a job nobody is waiting on.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sse" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.record("POST " + r.URL.Path)
	}))
	defer srv.Close()

	cfg := client.Config{RouterURL: srv.URL, Token: "bad", HTTP: srv.Client()}
	if _, err := client.Submit(ctx(t), cfg, client.Input{Body: strings.NewReader("x")}, nil); err == nil {
		t.Fatal("a rejected stream did not fail the submission")
	}
	if n := f.uploadCount(); n != 0 {
		t.Errorf("%d uploads happened after the stream was refused — the job would run with "+
			"nobody waiting for it, and the customer pays for a result they never see", n)
	}
}
