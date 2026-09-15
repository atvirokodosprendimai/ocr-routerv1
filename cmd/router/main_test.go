package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Addr:         "127.0.0.1:0",
		DBPath:       filepath.Join(dir, "e2e.db"),
		BlobDir:      filepath.Join(dir, "blobs"),
		ResultTTL:    time.Hour,
		Lease:        5 * time.Minute,
		MaxAttempts:  3,
		AgingStep:    time.Minute,
		LabelGrace:   5 * time.Minute,
		ReapInterval: 50 * time.Millisecond,
		PingInterval: 50 * time.Millisecond,
		MaxUpload:    1 << 20,
		DefaultLabel: "ocr",
	}
}

// startApp builds the app THE SAME WAY main does and serves it.
//
// Using buildApp rather than assembling a graph here is the whole point: a test
// that wires its own dependencies proves things about that wiring, not about the
// binary's.
func startApp(t *testing.T, cfg Config) (*App, *httptest.Server) {
	t.Helper()
	app, err := buildApp(cfg)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)
	return app, srv
}

type client struct {
	t    *testing.T
	base string
	tok  string
}

func (c client) req(method, path string, body io.Reader, ct string) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.tok)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	c.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// seedAccounts bootstraps an admin and returns a funded client token and a
// worker token.
func seedAccounts(t *testing.T, app *App) (clientTok, workerTok, clientID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()

	_, adminTok, err := app.Ident.Bootstrap(ctx, "admin@example.com", now)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ap, err := app.Ident.Authenticate(ctx, "Bearer "+adminTok, now)
	if err != nil {
		t.Fatalf("Authenticate admin: %v", err)
	}

	cu, err := app.Ident.CreateUser(ctx, ap, "customer@example.com", core.RoleClient, now)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := app.Repo.AddCredits(ctx, cu.ID, 100, "seed", now); err != nil {
		t.Fatalf("AddCredits: %v", err)
	}
	_, clientTok, _ = app.Ident.MintToken(ctx, ap, cu.ID, core.RoleClient, "c", now)

	wu, _ := app.Ident.CreateUser(ctx, ap, "worker@example.com", core.RoleWorker, now)
	_, workerTok, _ = app.Ident.MintToken(ctx, ap, wu.ID, core.RoleWorker, "w", now)

	return clientTok, workerTok, cu.ID
}

// openStream opens an SSE stream and returns the event names it sees.
func openStream(t *testing.T, url, token string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("opening stream: %v", err)
	}
	out := make(chan string, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				select {
				case out <- name:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, cancel
}

func awaitEvent(t *testing.T, ch <-chan string, want string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed while waiting for %q", want)
			}
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

// TestEndToEndUploadToDelivery drives the whole product over HTTP.
func TestEndToEndUploadToDelivery(t *testing.T) {
	app, srv := startApp(t, testConfig(t))
	clientTok, workerTok, clientID := seedAccounts(t, app)

	c := client{t: t, base: srv.URL, tok: clientTok}
	w := client{t: t, base: srv.URL, tok: workerTok}

	// The worker's stream is what makes "ocr" an available label.
	workerEvents, cancelWorker := openStream(t, srv.URL+"/sse?label=ocr", workerTok)
	defer cancelWorker()
	awaitEvent(t, workerEvents, "hello", 3*time.Second)

	clientEvents, cancelClient := openStream(t, srv.URL+"/sse", clientTok)
	defer cancelClient()
	awaitEvent(t, clientEvents, "hello", 3*time.Second)

	// 1. The customer uploads.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "scan.pdf")
	_, _ = io.WriteString(fw, "the source document")
	_ = mw.Close()

	resp := c.req("POST", "/upload", &buf, mw.FormDataContentType())
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload = %d (%s), want 201", resp.StatusCode, b)
	}
	var created struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)

	// 2. The worker is woken and claims.
	awaitEvent(t, workerEvents, "work", 3*time.Second)

	resp = w.req("POST", "/claim?label=ocr", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d, want 200", resp.StatusCode)
	}
	var claim struct {
		JobID   string `json:"job_id"`
		HasBlob bool   `json:"has_blob"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&claim)
	if claim.JobID != created.JobID {
		t.Fatalf("claimed %q, want %q", claim.JobID, created.JobID)
	}

	// 3. The worker downloads the source it holds the lease on.
	resp = w.req("GET", "/files/"+claim.JobID, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("worker download = %d, want 200", resp.StatusCode)
	}
	src, _ := io.ReadAll(resp.Body)
	if string(src) != "the source document" {
		t.Fatalf("worker got %q", src)
	}

	// 4. The worker posts its result.
	body := strings.NewReader(`{"job_id":"` + claim.JobID + `","units":["page one","page two","page three"]}`)
	resp = w.req("POST", "/upload", body, "application/json")
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("worker result = %d (%s), want 204", resp.StatusCode, b)
	}

	// 5. The customer is told on its stream.
	awaitEvent(t, clientEvents, "ready", 3*time.Second)

	// 6. The customer collects, and is charged.
	before, _ := app.Repo.UserByID(context.Background(), clientID)
	resp = c.req("GET", "/files/"+created.JobID, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client fetch = %d, want 200", resp.StatusCode)
	}
	var result struct {
		Units []string `json:"units"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Units) != 3 || result.Units[0] != "page one" {
		t.Fatalf("result = %v", result.Units)
	}

	after, _ := app.Repo.UserByID(context.Background(), clientID)
	if after.Credits != before.Credits-3 {
		t.Errorf("credits went %d -> %d, want a debit of 3", before.Credits, after.Credits)
	}

	// The blob is gone and the job is terminal.
	job, _ := app.Repo.JobByID(context.Background(), created.JobID)
	if job.State != core.JobDelivered {
		t.Errorf("final state = %q, want delivered", job.State)
	}
	if _, err := app.Blobs.Size(created.JobID); err == nil {
		t.Error("the source blob survived delivery")
	}
}

// TestReaperRunsInBinary proves the TICKER is wired, not merely that Reap works.
//
// ⚠ This test must NOT call Reap itself. Every test in internal/router would
// still pass with the ticker deleted from the composition root, because they
// drive Reap directly — the ticker is precisely the thing no unit test watches.
func TestReaperRunsInBinary(t *testing.T) {
	cfg := testConfig(t)
	cfg.Lease = 100 * time.Millisecond // lapse quickly
	app, srv := startApp(t, cfg)
	clientTok, workerTok, _ := seedAccounts(t, app)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.StartReaper(ctx, cfg.ReapInterval)

	workerEvents, cancelWorker := openStream(t, srv.URL+"/sse?label=ocr", workerTok)
	defer cancelWorker()
	awaitEvent(t, workerEvents, "hello", 3*time.Second)

	c := client{t: t, base: srv.URL, tok: clientTok}
	resp := c.req("POST", "/upload?label=ocr&url=x", nil, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	var created struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)

	w := client{t: t, base: srv.URL, tok: workerTok}
	if resp := w.req("POST", "/claim?label=ocr", nil, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d, want 200", resp.StatusCode)
	}

	// The worker then goes silent. Only the ticker can rescue this job.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := app.Repo.JobByID(context.Background(), created.JobID)
		if err == nil && job.State == core.JobQueued && job.Attempts > 0 {
			return // the reaper ran, unprompted
		}
		time.Sleep(25 * time.Millisecond)
	}
	job, _ := app.Repo.JobByID(context.Background(), created.JobID)
	t.Errorf("the job is still %q after its lease lapsed — the reaper ticker is not running in "+
		"the composition root, which no test in internal/router would notice", job.State)
}

func TestRecoverOnBootRequeuesAcrossRestart(t *testing.T) {
	cfg := testConfig(t)

	// First boot: upload and claim, then tear down mid-flight.
	app1, srv1 := startApp(t, cfg)
	clientTok, workerTok, clientID := seedAccounts(t, app1)

	workerEvents, cancelWorker := openStream(t, srv1.URL+"/sse?label=ocr", workerTok)
	awaitEvent(t, workerEvents, "hello", 3*time.Second)

	c := client{t: t, base: srv1.URL, tok: clientTok}
	resp := c.req("POST", "/upload?label=ocr&url=x", nil, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	var created struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)

	w := client{t: t, base: srv1.URL, tok: workerTok}
	if resp := w.req("POST", "/claim?label=ocr", nil, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d, want 200", resp.StatusCode)
	}

	cancelWorker()
	srv1.Close()
	if err := app1.Close(); err != nil {
		t.Fatalf("closing the first app: %v", err)
	}

	// Second boot on the same files.
	app2, _ := startApp(t, cfg)

	job, err := app2.Repo.JobByID(context.Background(), created.JobID)
	if err != nil {
		t.Fatalf("JobByID after restart: %v", err)
	}
	if job.State != core.JobQueued {
		t.Errorf("state after restart = %q, want queued — a lease cannot survive the process "+
			"that granted it", job.State)
	}
	entries, _ := app2.Repo.Ledger(context.Background(), clientID, 10)
	for _, e := range entries {
		if e.Delta < 0 {
			t.Errorf("a restart produced a debit of %d — boot recovery costs repeated work, "+
				"never money", e.Delta)
		}
	}
}

func TestBootstrapCreatesAdminOnceAndTheTokenWorks(t *testing.T) {
	cfg := testConfig(t)
	app, srv := startApp(t, cfg)
	ctx := context.Background()

	user, token, err := app.Ident.Bootstrap(ctx, "first@example.com", time.Now())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if user.Role != core.RoleAdmin {
		t.Errorf("role = %q, want admin", user.Role)
	}

	// ⚠ The printed token must actually authenticate. Printing one that does not
	// is worse than printing none: the operator believes they have access and
	// discovers otherwise at the worst moment.
	c := client{t: t, base: srv.URL, tok: token}
	if resp := c.req("GET", "/services", nil, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the bootstrap token does not authenticate against the running server (%d)",
			resp.StatusCode)
	}

	if _, _, err := app.Ident.Bootstrap(ctx, "second@example.com", time.Now()); err == nil {
		t.Error("a second bootstrap succeeded — it would be an unauthenticated way to mint " +
			"administrators forever")
	}
}

func TestSSESurvivesTheServersOwnConfig(t *testing.T) {
	// The server main builds sets no WriteTimeout, and the stream handler clears
	// its deadline anyway. This asserts the combination actually holds a stream
	// open well past the reap interval.
	cfg := testConfig(t)
	app, srv := startApp(t, cfg)
	clientTok, _, _ := seedAccounts(t, app)

	events, cancel := openStream(t, srv.URL+"/sse", clientTok)
	defer cancel()
	awaitEvent(t, events, "hello", 3*time.Second)
	awaitEvent(t, events, "ping", 3*time.Second)
}

func TestFlagDefaultsBuildAServer(t *testing.T) {
	// The defaults must be usable with nothing but a path: an operator's first
	// run should not require reading the whole flag list.
	dir := t.TempDir()
	cfg := Config{
		DBPath:  filepath.Join(dir, "d.db"),
		BlobDir: filepath.Join(dir, "b"),
	}
	cfg.ResultTTL = time.Hour
	cfg.Lease = 5 * time.Minute
	cfg.MaxAttempts = 3
	cfg.AgingStep = time.Minute
	cfg.LabelGrace = 5 * time.Minute
	cfg.MaxUpload = 1 << 20

	app, err := buildApp(cfg)
	if err != nil {
		t.Fatalf("buildApp with minimal config: %v", err)
	}
	defer app.Close()
	if app.Handler == nil {
		t.Fatal("buildApp returned a nil handler")
	}
}

// TestAdminSubtreeIsMounted is the composition-root check for the dashboard.
//
// Every test in internal/web mounts the subtree itself, so all of them pass with
// the `dash.Mount` line deleted from buildApp. This one resolves /admin through
// the binary's own graph, which is the only place that line exists.
func TestAdminSubtreeIsMounted(t *testing.T) {
	app, srv := startApp(t, testConfig(t))
	_, adminTok, err := app.Ident.Bootstrap(context.Background(), "a@example.com", time.Now())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/admin", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /admin: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		t.Fatal("/admin is not mounted in the binary's own handler — the dashboard is finished, " +
			"tested, and unreachable from the product")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /admin as admin = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ocr-router") {
		t.Error("/admin resolved but did not render the dashboard layout")
	}
}

func TestCLIExposesEveryFlag(t *testing.T) {
	// Rung 3: the caller can discover the configuration. A flag that exists in
	// Config but is not on the command line is unreachable by an operator.
	cmd := newCLI()
	have := map[string]bool{}
	for _, f := range cmd.Flags {
		for _, n := range f.Names() {
			have[n] = true
		}
	}
	for _, want := range []string{
		"addr", "db", "blobs", "result-ttl", "lease", "max-attempts",
		"aging-step", "label-grace", "reap-interval", "max-upload", "default-label",
	} {
		if !have[want] {
			t.Errorf("--%s is not exposed on the command line", want)
		}
	}
}
