package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
)

// lockedBuffer is safe for the server's goroutines to write while the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startCaptured builds the app with its stdout logging redirected to a buffer.
//
// ⚠ It swaps os.Stdout around buildApp ONLY, and that is sufficient rather than
// lucky: logging.New resolves the default writer once, at construction, and the
// handler keeps that *os.File. Restoring os.Stdout immediately afterwards
// therefore leaves the app still writing into the pipe while the test's own
// output goes back where it belongs.
//
// Config deliberately carries no writer field. Adding one to make this test
// easier would mean asserting against a path production never takes — the binary
// logs to stdout, so the test has to capture stdout.
func startCaptured(t *testing.T, cfg Config) (*App, *httptest.Server, *lockedBuffer) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	buf := &lockedBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(buf, r)
	}()

	orig := os.Stdout
	os.Stdout = w
	app, buildErr := buildApp(cfg)
	os.Stdout = orig

	t.Cleanup(func() {
		_ = w.Close()
		<-done
		_ = r.Close()
	})

	if buildErr != nil {
		t.Fatalf("buildApp: %v", buildErr)
	}
	t.Cleanup(func() { _ = app.Close() })

	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)
	return app, srv, buf
}

func uploadOnce(t *testing.T, srv *httptest.Server, token string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "a.pdf")
	_, _ = io.WriteString(fw, "src")
	_ = mw.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/upload", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /upload: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestBinaryAppliesTheRateLimit is red if wire.go stops passing the limiter into
// httpapi.Deps — which every test in internal/httpapi and internal/ratelimit
// survives.
func TestBinaryAppliesTheRateLimit(t *testing.T) {
	cfg := testConfig(t)
	cfg.RateClient = 1
	cfg.RateBurst = 2
	cfg.RateIdle = time.Hour

	app, srv := startApp(t, cfg)
	clientTok, _, _ := seedAccounts(t, app)

	var throttled bool
	for i := 0; i < 6; i++ {
		req, _ := http.NewRequest("GET", srv.URL+"/services", nil)
		req.Header.Set("Authorization", "Bearer "+clientTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /services: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			throttled = true
			if resp.Header.Get("Retry-After") == "" {
				t.Error("the binary's 429 carries no Retry-After")
			}
			break
		}
	}
	if !throttled {
		t.Error("six requests at a burst of 2 were never throttled — the limiter is constructed " +
			"in the composition root and never handed to the API")
	}
}

// TestRateFlagZeroDisablesLimiting asserts the documented rollback rather than
// describing it.
func TestRateFlagZeroDisablesLimiting(t *testing.T) {
	cfg := testConfig(t)
	cfg.RateClient = 0 // the operator's switch
	cfg.RateBurst = 20
	cfg.RateIdle = time.Hour

	app, srv := startApp(t, cfg)
	clientTok, _, _ := seedAccounts(t, app)

	for i := 0; i < 200; i++ {
		req, _ := http.NewRequest("GET", srv.URL+"/services", nil)
		req.Header.Set("Authorization", "Bearer "+clientTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /services: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("call %d was throttled with --rate-client 0 — the rollback an operator "+
				"is told to reach for does not work", i+1)
		}
	}
}

// TestEvictionRunsOnTheReaperTick is red if the EvictIdle call is deleted from
// StartReaper.
//
// ⚠ It asserts Len() DROPPED. Asserting that eviction was called would prove
// someone wrote a line of code about the leak, not that the leak is gone — and
// no test inside internal/ratelimit can see this line at all.
func TestEvictionRunsOnTheReaperTick(t *testing.T) {
	cfg := testConfig(t)
	cfg.RateClient = 100
	cfg.RateBurst = 100
	cfg.ReapInterval = 20 * time.Millisecond
	// Anything untouched for a millisecond is idle, so the very next tick after
	// the requests must sweep them.
	cfg.RateIdle = time.Millisecond

	app, srv := startApp(t, cfg)
	clientTok, workerTok, _ := seedAccounts(t, app)

	for _, tok := range []string{clientTok, workerTok} {
		req, _ := http.NewRequest("GET", srv.URL+"/services", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /services: %v", err)
		}
		_ = resp.Body.Close()
	}
	if got := app.Limiter.Len(); got == 0 {
		t.Fatal("the limiter holds no entries, so there is nothing for eviction to remove and " +
			"this test proves nothing")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.StartReaper(ctx, cfg.ReapInterval)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if app.Limiter.Len() == 0 {
			return // swept
		}
		if time.Now().After(deadline) {
			t.Fatalf("the limiter still holds %d entries after the reaper ran. Eviction is not "+
				"wired to the tick, so the map grows one entry per token forever",
				app.Limiter.Len())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBinaryLogsRequestsAndTransitions is red if either wiring line is deleted.
func TestBinaryLogsRequestsAndTransitions(t *testing.T) {
	cfg := testConfig(t)
	app, srv, buf := startCaptured(t, cfg)

	clientTok, workerTok, _ := seedAccounts(t, app)
	// A live worker makes "ocr" available so the upload is accepted.
	_, cancel := openStream(t, srv.URL+"/sse?label=ocr", workerTok)
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	if resp := uploadOnce(t, srv, clientTok); resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload = %d (%s)", resp.StatusCode, b)
	}
	time.Sleep(100 * time.Millisecond)

	out := buf.String()
	if !strings.Contains(out, `"msg":"request"`) {
		t.Errorf("no request line reached stdout — the logger is not passed into httpapi.Deps:"+
			"\n%s", out)
	}
	if !strings.Contains(out, `"msg":"transition"`) {
		t.Errorf("no transition line reached stdout — rt.SetLogger is not called in the "+
			"composition root, so every state change is invisible:\n%s", out)
	}
}

func TestBinaryLogsAreValidJSONLines(t *testing.T) {
	cfg := testConfig(t)
	app, srv, buf := startCaptured(t, cfg)
	clientTok, _, _ := seedAccounts(t, app)

	req, _ := http.NewRequest("GET", srv.URL+"/services", nil)
	req.Header.Set("Authorization", "Bearer "+clientTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /services: %v", err)
	}
	_ = resp.Body.Close()
	time.Sleep(100 * time.Millisecond)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var parsed int
	for _, line := range lines {
		if !strings.HasPrefix(line, "{") {
			continue // the startup fmt.Printf lines, deliberately left plain
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("a log line is not valid JSON: %v\n%s", err, line)
			continue
		}
		parsed++
	}
	if parsed == 0 {
		t.Errorf("no JSON log lines at all, so this assertion proved nothing:\n%s", buf.String())
	}
}

// TestBadLogLevelFailsBoot — a silent fallback would leave an operator with
// logging they cannot discover is wrong.
func TestBadLogLevelFailsBoot(t *testing.T) {
	cfg := testConfig(t)
	cfg.LogLevel = "verbose"

	app, err := buildApp(cfg)
	if err == nil {
		_ = app.Close()
		t.Fatal("buildApp accepted --log-level verbose and started anyway")
	}
	if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("the error does not name the offending value: %v", err)
	}
}

func TestBadLogFormatFailsBoot(t *testing.T) {
	cfg := testConfig(t)
	cfg.LogFormat = "logfmt"

	app, err := buildApp(cfg)
	if err == nil {
		_ = app.Close()
		t.Fatal("buildApp accepted --log-format logfmt and started anyway")
	}
}

// TestBinaryNeverLogsAParamValue closes a gap a surviving mutant found.
//
// ⚠ internal/router's redaction test builds its OWN adapter. That proves the
// logging package redacts and that an adapter CAN be written correctly — it
// proves nothing about `routerLogger` in wire.go, which is the one the binary
// actually uses. A mutant that made that adapter smuggle a value survived the
// whole suite until this test existed.
func TestBinaryNeverLogsAParamValue(t *testing.T) {
	cfg := testConfig(t)
	app, srv, buf := startCaptured(t, cfg)

	clientTok, workerTok, _ := seedAccounts(t, app)
	_, cancel := openStream(t, srv.URL+"/sse?label=ocr", workerTok)
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	const secret = "hunter2-do-not-log"
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "a.pdf")
	_, _ = io.WriteString(fw, "src")
	_ = mw.Close()

	req, _ := http.NewRequest("POST",
		srv.URL+"/upload?url=https%3A%2F%2Falice%3A"+secret+"%40example.com%2Fx", &body)
	req.Header.Set("Authorization", "Bearer "+clientTok)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /upload: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d (%s)", resp.StatusCode, respBody)
	}
	time.Sleep(150 * time.Millisecond)

	out := buf.String()
	// ★ The key must be present, or the absence check below is satisfied by an
	// upload that carried no params at all.
	if !strings.Contains(out, `"url"`) {
		t.Fatalf("the param KEY never reached the log, so the absence check proves nothing:\n%s",
			out)
	}
	for _, leak := range []string{secret, "alice", "example.com"} {
		if strings.Contains(out, leak) {
			t.Errorf("the binary's log leaks %q from a customer-supplied param value:\n%s",
				leak, out)
		}
	}
}

// TestConfigFromReadsEveryNewFlag closes the second gap a mutant found.
//
// ⚠ Every other test in this package builds a Config STRUCT directly, so
// `configFrom` — the function that turns command-line flags into that struct —
// was executed by nothing. A mutant that read `--rate-burst` and threw the value
// away survived the entire suite: the flag existed, `--help` advertised it, and
// it did nothing.
func TestConfigFromReadsEveryNewFlag(t *testing.T) {
	cmd := newCLI()
	var got Config
	cmd.Action = func(_ context.Context, c *cli.Command) error {
		got = configFrom(c)
		return nil
	}

	args := []string{
		"router",
		"--rate-client", "7",
		"--rate-worker", "11",
		"--rate-admin", "13",
		"--rate-burst", "17",
		"--rate-idle", "3m",
		"--log-level", "warn",
		"--log-format", "text",
	}
	if err := cmd.Run(context.Background(), args); err != nil {
		t.Fatalf("running with the new flags: %v", err)
	}

	// Distinct primes throughout, so a field wired to the wrong flag is visible
	// rather than coincidentally right.
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"rate-client", got.RateClient, 7.0},
		{"rate-worker", got.RateWorker, 11.0},
		{"rate-admin", got.RateAdmin, 13.0},
		{"rate-burst", got.RateBurst, 17},
		{"rate-idle", got.RateIdle, 3 * time.Minute},
		{"log-level", got.LogLevel, "warn"},
		{"log-format", got.LogFormat, "text"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("--%s reached Config as %v, want %v — the flag is advertised and discarded",
				c.name, c.got, c.want)
		}
	}
}

func TestCLIExposesTheNewFlags(t *testing.T) {
	// Rung 3: a flag that exists in Config but not on the command line is
	// unreachable by an operator, and seven of them is a large surface to leave
	// undiscoverable.
	cmd := newCLI()
	have := map[string]bool{}
	for _, f := range cmd.Flags {
		for _, n := range f.Names() {
			have[n] = true
		}
	}
	for _, want := range []string{
		"rate-client", "rate-worker", "rate-admin", "rate-burst", "rate-idle",
		"log-level", "log-format",
	} {
		if !have[want] {
			t.Errorf("--%s is not exposed on the command line", want)
		}
	}
}

// TestWorkerKeepsPollingThroughA429 covers ADR-0002 Risk 12.
//
// A worker that treated a 429 as fatal would back off into a permanent stall,
// and the fixture must actually TRIGGER one — a burst left at the default would
// pass this test without ever throttling.
func TestWorkerKeepsPollingThroughA429(t *testing.T) {
	cfg := testConfig(t)
	// ⚠ 4 rps, not 1000. The original fixture used 1000 so the bucket would
	// refill quickly for the recovery half — and at 1000 rps a token returns
	// every millisecond, which is less than one claim round trip once `-race`
	// slows everything down. The loop below then never saw a 429 and the test
	// failed on its own vacuity guard, intermittently, months after it was
	// written. A rate slow enough to outlast a request is what makes the
	// throttle reachable; 250ms is still fast enough for the recovery half.
	cfg.RateWorker = 4
	cfg.RateBurst = 1
	cfg.RateIdle = time.Hour

	app, srv := startApp(t, cfg)
	_, workerTok, _ := seedAccounts(t, app)

	claim := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/claim?label=ocr", nil)
		req.Header.Set("Authorization", "Bearer "+workerTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /claim: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	var saw429 bool
	for i := 0; i < 20 && !saw429; i++ {
		if claim() == http.StatusTooManyRequests {
			saw429 = true
		}
	}
	if !saw429 {
		t.Fatal("the fixture never produced a 429, so it proves nothing about recovering from one")
	}

	// After the bucket refills, claiming must work again — a 429 is back-pressure
	// and never a terminal state for a poller.
	time.Sleep(100 * time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := claim(); got != http.StatusTooManyRequests {
			return // recovered
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker is still throttled three seconds after a 429 at 4 rps — a " +
				"poller facing this would stall permanently")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
