package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubRouter is the three endpoints the client drives, with knobs for each
// failure this binary has to classify.
//
// ⚠ It is a stub rather than the real router because the properties under test
// are the BINARY's — exit codes, stream separation, atomic writes — and driving
// a real router would need a real worker to make a job succeed or die on demand.
// internal/client's tests cover the protocol against the wire format;
// cmd/router's tests cover the router. This covers what neither can.
type stubRouter struct {
	srv *httptest.Server

	mu      sync.Mutex
	uploads int

	uploadStatus  int
	collectStatus int
	units         []string
	// outcome is the event sent for the job: "ready", "failed", or "" to send
	// nothing at all (which is how the timeout case is driven).
	outcome string
	reason  string
}

func newStub(t *testing.T) *stubRouter {
	t.Helper()
	s := &stubRouter{
		uploadStatus:  http.StatusCreated,
		collectStatus: http.StatusOK,
		units:         []string{"page one", "page two"},
		outcome:       "ready",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "event: hello\ndata: {}\n\n")
		fl.Flush()
		fmt.Fprint(w, "event: backlog\ndata: {\"jobs\":[]}\n\n")
		fl.Flush()

		if s.outcome != "" {
			time.Sleep(40 * time.Millisecond)
			payload, _ := json.Marshal(map[string]any{"job_id": "job-1", "reason": s.reason})
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", s.outcome, payload)
			fl.Flush()
		}
		<-r.Context().Done()
	})

	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.uploads++
		s.mu.Unlock()

		if s.uploadStatus != http.StatusCreated {
			w.WriteHeader(s.uploadStatus)
			_, _ = io.WriteString(w, `{"error":"refused"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": "job-1"})
	})

	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		if s.collectStatus != http.StatusOK {
			w.WriteHeader(s.collectStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"units": s.units})
	})

	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubRouter) uploadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uploads
}

// invoke runs the REAL command with stdout and stderr captured SEPARATELY.
//
// ⚠ Separately is the point: merging them would pass while stdout was being
// corrupted by progress output, which is the exact failure the split exists to
// prevent.
func invoke(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(append([]string{"client"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func writeInput(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(p, []byte("the document"), 0o600); err != nil {
		t.Fatalf("writing input: %v", err)
	}
	return p
}

func TestSubmitWritesResultAndExitsZero(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)
	out := filepath.Join(t.TempDir(), "result.txt")

	code, _, stderr := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", out)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0. stderr:\n%s", code, stderr)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the result: %v", err)
	}
	if string(body) != "page one\npage two\n" {
		t.Errorf("result = %q", body)
	}
}

// TestFailedJobExitsThree is the code that matters most.
func TestFailedJobExitsThree(t *testing.T) {
	s := newStub(t)
	s.outcome = "failed"
	s.reason = "attempts exhausted"
	in := writeInput(t)
	out := filepath.Join(t.TempDir(), "result.txt")

	code, _, stderr := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", out)
	if code != exitJobFailed {
		t.Fatalf("exit = %d, want %d. A client that exits 0 on a dead job turns a failure into "+
			"silent data loss in whatever pipeline called it", code, exitJobFailed)
	}
	if !strings.Contains(stderr, "attempts exhausted") {
		t.Errorf("the reason did not reach stderr:\n%s", stderr)
	}
}

// TestNoOutputFileOnFailure — an empty file beside a non-zero exit invites a
// caller to use it anyway.
func TestNoOutputFileOnFailure(t *testing.T) {
	s := newStub(t)
	s.outcome = "failed"
	in := writeInput(t)
	out := filepath.Join(t.TempDir(), "result.txt")

	if code, _, _ := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", out); code != exitJobFailed {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("an output file exists after a failed job (err=%v)", err)
	}
}

func TestBadTokenExitsOne(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)

	code, _, _ := invoke(t,
		"--router", s.srv.URL, "--token", "wrong-token", "-i", in, "-o", "-")
	if code != exitFix {
		t.Errorf("exit = %d, want %d — retrying a bad credential forever is the wrong action",
			code, exitFix)
	}
}

func TestRateLimitedExitsTwo(t *testing.T) {
	s := newStub(t)
	s.uploadStatus = http.StatusTooManyRequests
	in := writeInput(t)

	code, _, _ := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", "-")
	if code != exitRetry {
		t.Errorf("exit = %d, want %d — a 429 is exactly the case where waiting and retrying is "+
			"correct, and a single failure code would hide it", code, exitRetry)
	}
}

func TestNoCreditsExitsOne(t *testing.T) {
	s := newStub(t)
	s.uploadStatus = http.StatusPaymentRequired
	in := writeInput(t)

	code, _, _ := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", "-")
	if code != exitFix {
		t.Errorf("exit = %d, want %d — retrying against an empty balance never succeeds",
			code, exitFix)
	}
}

// TestDashOWritesToStdout is the composability claim.
func TestDashOWritesToStdout(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)

	code, stdout, _ := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", "-")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if stdout != "page one\npage two\n" {
		t.Errorf("stdout = %q, want exactly the result", stdout)
	}
}

// TestProgressGoesToStderrNotStdout is red if a progress line ever reaches
// stdout, which corrupts every pipe the tool is used in.
func TestProgressGoesToStderrNotStdout(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)

	code, stdout, stderr := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", "-")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}

	// stdout is EXACTLY the result — nothing else.
	if stdout != "page one\npage two\n" {
		t.Errorf("stdout carries something besides the result: %q", stdout)
	}
	for _, noise := range []string{"uploading", "waiting", "collecting", "done"} {
		if strings.Contains(stdout, noise) {
			t.Errorf("progress word %q reached stdout, corrupting the pipe", noise)
		}
	}
	// And the progress really was produced, or this proves nothing.
	if !strings.Contains(stderr, "uploading") {
		t.Errorf("no progress on stderr either, so the assertion above is vacuous:\n%s", stderr)
	}
}

func TestQuietSuppressesProgress(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)
	out := filepath.Join(t.TempDir(), "r.txt")

	code, _, stderr := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", out, "--quiet")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Errorf("--quiet still wrote to stderr: %q", stderr)
	}
}

// TestNonTTYProgressHasNoCarriageReturns — a rewriting progress line is
// unreadable in a CI log and corrupt in a pipe.
func TestNonTTYProgressHasNoCarriageReturns(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)
	out := filepath.Join(t.TempDir(), "r.txt")

	_, _, stderr := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", out)
	if strings.Contains(stderr, "\r") {
		t.Errorf("progress used carriage returns when the output is not a terminal: %q", stderr)
	}
	if !strings.Contains(stderr, "uploading") {
		t.Errorf("no progress at all, so this proves nothing:\n%s", stderr)
	}
}

// TestUnwritableDestinationFailsBeforeUpload asserts the router saw NO upload.
//
// ⚠ Failing AFTER the upload still charges the customer for a result that
// cannot be written and cannot be fetched again — which is the outcome the probe
// exists to prevent, and a test that only checks the exit code would miss it.
func TestUnwritableDestinationFailsBeforeUpload(t *testing.T) {
	if os.Geteuid() == 0 {
		// Root ignores mode bits, so this cannot be made to fail — and a green
		// test that cannot go red is the defect this suite guards against
		// elsewhere.
		t.Skip("running as root: mode bits are not enforced, so the probe cannot be made to fail")
	}

	s := newStub(t)
	in := writeInput(t)

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	code, _, _ := invoke(t, "--router", s.srv.URL, "--token", "good-token",
		"-i", in, "-o", filepath.Join(dir, "result.txt"))
	if code != exitFix {
		t.Errorf("exit = %d, want %d", code, exitFix)
	}
	if n := s.uploadCount(); n != 0 {
		t.Errorf("%d uploads happened before the destination was found unwritable. Collecting a "+
			"result charges the credits, so the customer would pay for output that goes nowhere",
			n)
	}
}

func TestOutputToADirectoryIsRefused(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)

	code, _, stderr := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", t.TempDir())
	if code != exitFix {
		t.Errorf("exit = %d, want %d", code, exitFix)
	}
	if !strings.Contains(stderr, "directory") {
		t.Errorf("the message does not say the path is a directory: %s", stderr)
	}
}

func TestParamsOnlyJobNeedsNoInputFile(t *testing.T) {
	s := newStub(t)
	out := filepath.Join(t.TempDir(), "r.txt")

	code, _, stderr := invoke(t, "--router", s.srv.URL, "--token", "good-token",
		"--param", "url=https://example.com/doc", "--label", "crawl", "-o", out)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 — a params-only job is ADR-0001's crawler shape: %s",
			code, stderr)
	}
}

func TestMissingInputAndParamsIsAUsageError(t *testing.T) {
	s := newStub(t)

	code, _, stderr := invoke(t, "--router", s.srv.URL, "--token", "good-token", "-o", "-")
	if code != exitFix {
		t.Errorf("exit = %d, want %d", code, exitFix)
	}
	for _, want := range []string{"--input", "--param"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the message does not name %s: %s", want, stderr)
		}
	}
}

func TestBadParamFormatIsRejected(t *testing.T) {
	s := newStub(t)

	code, _, stderr := invoke(t, "--router", s.srv.URL, "--token", "good-token",
		"--param", "nokey", "-o", "-")
	if code != exitFix {
		t.Errorf("exit = %d, want %d", code, exitFix)
	}
	if !strings.Contains(stderr, "k=v") {
		t.Errorf("the message does not show the expected form: %s", stderr)
	}
}

func TestTokenFromEnvironment(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)
	out := filepath.Join(t.TempDir(), "r.txt")

	t.Setenv("OCRR_TOKEN", "good-token")
	code, _, stderr := invoke(t, "--router", s.srv.URL, "-i", in, "-o", out)
	if code != exitOK {
		t.Fatalf("exit = %d with OCRR_TOKEN set: %s", code, stderr)
	}
}

func TestMissingTokenIsAUsageError(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)

	t.Setenv("OCRR_TOKEN", "")
	code, _, stderr := invoke(t, "--router", s.srv.URL, "-i", in, "-o", "-")
	if code != exitFix {
		t.Errorf("exit = %d, want %d", code, exitFix)
	}
	if !strings.Contains(stderr, "OCRR_TOKEN") {
		t.Errorf("the message does not mention the environment variable: %s", stderr)
	}
}

func TestJSONFlagWritesTheEnvelope(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)

	code, stdout, _ := invoke(t, "--router", s.srv.URL, "--token", "good-token",
		"-i", in, "-o", "-", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}

	var env struct {
		JobID string   `json:"job_id"`
		Units []string `json:"units"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("--json did not produce JSON: %v\n%s", err, stdout)
	}
	if len(env.Units) != 2 || env.JobID != "job-1" {
		t.Errorf("envelope = %+v", env)
	}
}

// TestTimeoutExitsTwo — a timeout says nothing about whether the job will
// eventually succeed, so it is retryable rather than a job failure.
func TestTimeoutExitsTwo(t *testing.T) {
	s := newStub(t)
	s.outcome = "" // no event is ever sent, so only the timeout can end this
	in := writeInput(t)

	code, _, stderr := invoke(t, "--router", s.srv.URL, "--token", "good-token",
		"-i", in, "-o", "-", "--timeout", "300ms")
	if code != exitRetry {
		t.Errorf("exit = %d, want %d — a timeout is not evidence the job failed", code, exitRetry)
	}
	if !strings.Contains(stderr, "still be running") {
		t.Errorf("the message does not say the job may still be running: %s", stderr)
	}
}

// TestOutputIsWrittenAtomically — the result is collected and CHARGED before it
// is written, so a partial file would lose work the customer paid for.
func TestOutputIsWrittenAtomically(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "result.txt")

	if code, _, _ := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", out); code != exitOK {
		t.Fatalf("exit != 0")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ocrr-") {
			t.Errorf("a temporary file survived beside the destination: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("the destination directory holds %d entries, want 1", len(entries))
	}
}

func TestExistingOutputIsReplaced(t *testing.T) {
	s := newStub(t)
	in := writeInput(t)
	out := filepath.Join(t.TempDir(), "result.txt")
	if err := os.WriteFile(out, []byte("stale content from a previous run"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if code, _, stderr := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", out); code != exitOK {
		t.Fatalf("exit != 0: %s", stderr)
	}
	body, _ := os.ReadFile(out)
	if strings.Contains(string(body), "stale") {
		t.Error("the previous run's content survived")
	}
}

func TestMissingInputFileExitsOne(t *testing.T) {
	s := newStub(t)

	code, _, _ := invoke(t, "--router", s.srv.URL, "--token", "good-token",
		"-i", filepath.Join(t.TempDir(), "nope.pdf"), "-o", "-")
	if code != exitFix {
		t.Errorf("exit = %d, want %d", code, exitFix)
	}
}

func TestRouterUnreachableExitsTwo(t *testing.T) {
	in := writeInput(t)

	// A port nothing is listening on.
	code, _, _ := invoke(t, "--router", "http://127.0.0.1:1", "--token", "t",
		"-i", in, "-o", "-")
	if code != exitRetry {
		t.Errorf("exit = %d, want %d — a router that is down is worth retrying, and a caller "+
			"cannot tell that from exit 1", code, exitRetry)
	}
}

func TestCLIExposesEveryFlag(t *testing.T) {
	// Rung 3: a flag in the code but not on the command line is unreachable.
	cmd := newCLI(io.Discard, io.Discard)
	have := map[string]bool{}
	for _, f := range cmd.Flags {
		for _, n := range f.Names() {
			have[n] = true
		}
	}
	for _, want := range []string{
		"router", "token", "input", "i", "output", "o",
		"label", "pipeline", "param", "json", "quiet", "timeout",
	} {
		if !have[want] {
			t.Errorf("--%s is not on the command line", want)
		}
	}
}

// TestExpiredJobExitsThree — an expired job reaches the client as the same
// `failed` event with a different reason, so one path serves both.
//
// It is asserted separately anyway: "dead" and "expired" are different things to
// an operator, and a caller that treated expiry as retryable would resubmit work
// the router deliberately gave up on.
func TestExpiredJobExitsThree(t *testing.T) {
	s := newStub(t)
	s.outcome = "failed"
	s.reason = "expired"
	in := writeInput(t)
	out := filepath.Join(t.TempDir(), "result.txt")

	code, _, stderr := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", out)
	if code != exitJobFailed {
		t.Errorf("exit = %d, want %d for an expired job", code, exitJobFailed)
	}
	if !strings.Contains(stderr, "expired") {
		t.Errorf("the reason did not reach stderr: %s", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("an output file exists after an expired job")
	}
}

// TestDevNullDestination covers a gap found by running the binary, not by any
// test written beforehand.
//
// ⚠ `-o /dev/null` is a common idiom — "I only want the exit code" — and the
// destination probe refused it, because it tried to create a temp file in
// /dev. Worse, had the probe passed, the atomic rename would have REPLACED THE
// DEVICE NODE with a regular file: protecting against a partial write that
// cannot happen on a character device, by destroying something the system needs.
func TestDevNullDestination(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skip("no /dev/null on this platform")
	}
	s := newStub(t)
	in := writeInput(t)

	code, stdout, stderr := invoke(t,
		"--router", s.srv.URL, "--token", "good-token", "-i", in, "-o", "/dev/null", "--quiet")
	if code != exitOK {
		t.Fatalf("exit = %d writing to /dev/null: %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty — the result went to /dev/null", stdout)
	}

	// The device node survived as a device node.
	info, err := os.Stat("/dev/null")
	if err != nil {
		t.Fatalf("/dev/null is gone: %v", err)
	}
	if info.Mode().IsRegular() {
		t.Fatal("/dev/null is now a REGULAR FILE — the atomic rename replaced the device node")
	}
}
