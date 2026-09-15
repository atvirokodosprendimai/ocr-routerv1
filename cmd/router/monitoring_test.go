package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/atvirokodosprendimai/ocr-router/internal/monitor"
)

// TestHealthzIsUnauthenticatedInTheBinary is a composition-root check.
//
// internal/monitor's tests call the handler directly, so every one of them passes
// with /healthz mounted INSIDE the authenticated group — where a load balancer,
// which cannot hold a bearer token, would see 401 on a perfectly healthy process
// and take it out of rotation.
func TestHealthzIsUnauthenticatedInTheBinary(t *testing.T) {
	_, srv := startApp(t, testConfig(t))

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("/healthz requires a bearer token in the binary's own handler — a probe cannot " +
			"hold one, so every healthy process reads as dead")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", resp.StatusCode)
	}

	var body struct {
		OK      bool     `json:"ok"`
		Version string   `json:"version"`
		Failed  []string `json:"failed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding /healthz: %v", err)
	}
	if !body.OK || len(body.Failed) != 0 {
		t.Errorf("a freshly built app is not healthy: %+v", body)
	}
}

// TestHealthzIsTheOnlyUnauthenticatedRoute pins the exception so it stays one.
//
// /healthz being outside the authenticator is a deliberate hole. The risk is not
// that hole but the next one added beside it, so the rule is asserted rather than
// commented.
func TestHealthzIsTheOnlyUnauthenticatedRoute(t *testing.T) {
	_, srv := startApp(t, testConfig(t))

	// The method matters: chi answers 405 before any middleware runs, so probing
	// every route with GET would pass vacuously on the POST-only ones.
	routes := []struct{ method, path string }{
		{"POST", "/upload"},
		{"POST", "/claim"},
		{"GET", "/sse"},
		{"GET", "/admin"},
		{"GET", "/admin/users"},
		{"GET", "/files/x"},
	}
	for _, r := range routes {
		req, err := http.NewRequest(r.method, srv.URL+r.path, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", r.method, r.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential = %d, want 401 — only /healthz may answer "+
				"unauthenticated", r.method, r.path, resp.StatusCode)
		}
	}
}

// TestMetricsIsNotOnThePublicListener is the half of the exposure decision that
// protects the data.
//
// Queue depth and throughput say how much work each customer is pushing. Serving
// them on the internet-facing listener would publish that to anyone who guesses
// the path, and no test inside internal/monitor can see this — that package does
// not know the main mux exists.
func TestMetricsIsNotOnThePublicListener(t *testing.T) {
	_, srv := startApp(t, testConfig(t))

	for _, path := range []string{"/metrics", "/admin/metrics"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "ocrr_") {
			t.Errorf("the public listener serves Prometheus metrics at %s — commercially "+
				"sensitive throughput data, readable by anyone who guesses the path", path)
		}
	}
}

// TestMetricsDefaultsToLoopback asserts the ACTUAL bound address.
//
// ⚠ The tempting version asserts the flag's default *string* and proves nothing:
// a default of "127.0.0.1:9090" handed to a Serve that ignored it would still
// pass. This reads the default off the real command, binds it — port 0, so the
// test does not fight whatever else is on 9090 — and asks the socket what it got.
func TestMetricsDefaultsToLoopback(t *testing.T) {
	def := metricsAddrDefault(t)

	host, _, err := net.SplitHostPort(def)
	if err != nil {
		t.Fatalf("the --metrics-addr default %q is not a host:port: %v", def, err)
	}

	app, err := buildApp(testConfig(t))
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer func() { _ = app.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, shutdown, err := app.Monitor.Serve(ctx, net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("Serve on the default host %q: %v", host, err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	boundHost, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	ip := net.ParseIP(boundHost)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("with no --metrics-addr the metrics listener binds %q, which is not loopback. "+
			"The default is the protection: an operator who wants metrics elsewhere says so, and "+
			"one who says nothing must not be publishing throughput to the internet", boundHost)
	}
}

// metricsAddrDefault reads the flag's declared default off the real command.
func metricsAddrDefault(t *testing.T) string {
	t.Helper()
	for _, f := range newCLI().Flags {
		sf, ok := f.(*cli.StringFlag)
		if !ok {
			continue
		}
		for _, n := range sf.Names() {
			if n == "metrics-addr" {
				return sf.Value
			}
		}
	}
	t.Fatal("--metrics-addr is not on the command line at all, so it has no default to protect")
	return ""
}

// TestCountersMoveThroughTheBinarysGraph is the wiring check for metrics.
//
// internal/monitor's registry tests all pass with `rt.SetCounter(reg)` deleted
// from buildApp: the registry works, nothing increments it, and a flat line reads
// exactly like a quiet healthy system. This drives a real job through the binary
// and demands the numbers move.
func TestCountersMoveThroughTheBinarysGraph(t *testing.T) {
	app, srv := startApp(t, testConfig(t))
	clientTok, workerTok, _ := seedAccounts(t, app)

	c := client{t: t, base: srv.URL, tok: clientTok}
	w := client{t: t, base: srv.URL, tok: workerTok}

	// The worker's stream is what makes "ocr" an available label.
	workerEvents, cancelWorker := openStream(t, srv.URL+"/sse?label=ocr", workerTok)
	defer cancelWorker()
	awaitEvent(t, workerEvents, "hello", 3*time.Second)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "scan.pdf")
	_, _ = io.WriteString(fw, "source")
	_ = mw.Close()

	resp := c.req("POST", "/upload", &buf, mw.FormDataContentType())
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	var created struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)

	resp = w.req("POST", "/claim?label=ocr", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d, want 200", resp.StatusCode)
	}
	result := strings.NewReader(`{"job_id":"` + created.JobID + `","units":["p1","p2"]}`)
	resp = w.req("POST", "/upload", result, "application/json")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("worker result = %d, want 204", resp.StatusCode)
	}

	resp = c.req("GET", "/files/"+created.JobID, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("collect = %d, want 200", resp.StatusCode)
	}

	reg := app.Monitor.Registry()
	if got := reg.Value(monitor.MetricJobsTotal, map[string]string{"state": "delivered"}); got == 0 {
		t.Error(`ocrr_jobs_total{state="delivered"} is still zero after a job was delivered ` +
			"through the binary — the registry is not attached to the writer, so every metric " +
			"reads as a quiet healthy system forever")
	}
	if got := reg.Value(monitor.MetricCreditsDebited, nil); got != 2 {
		t.Errorf("ocrr_credits_debited_total = %d after two pages were charged, want 2", got)
	}
}

// TestGaugesDoNotTouchTheWritePath scrapes against the binary's real handles.
//
// Every gauge is computed on the scrape from the READ handle, which is opened
// `query_only(1)`. A gauge that wrote — a cached table, a "last scraped" row —
// would not merely be untidy: it would take the single writer's lock on a
// schedule an outsider controls. Here that fails rather than passing quietly,
// because the driver refuses the write.
func TestGaugesDoNotTouchTheWritePath(t *testing.T) {
	app, srv := startApp(t, testConfig(t))
	clientTok, workerTok, _ := seedAccounts(t, app)

	// Real rows, so the gauges have something to compute over — a scrape against
	// an empty database can pass without touching the tables at all.
	workerEvents, cancelWorker := openStream(t, srv.URL+"/sse?label=ocr", workerTok)
	defer cancelWorker()
	awaitEvent(t, workerEvents, "hello", 3*time.Second)

	c := client{t: t, base: srv.URL, tok: clientTok}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.pdf")
	_, _ = io.WriteString(fw, "x")
	_ = mw.Close()
	if resp := c.req("POST", "/upload", &buf, mw.FormDataContentType()); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, shutdown, err := app.Monitor.Serve(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics = %d against the read handle: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `ocrr_queue_depth{label="ocr"} 1`) {
		t.Errorf("the scrape did not report the queued job, so it may not have read the tables "+
			"at all and this assertion would prove nothing:\n%s", body)
	}
}

// TestHealthzReportsTheBuildVersion checks the field an operator uses to tell
// which build is actually running mid-rollout.
func TestHealthzReportsTheBuildVersion(t *testing.T) {
	cfg := testConfig(t)
	cfg.Version = "v9.9.9-test"
	_, srv := startApp(t, cfg)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "v9.9.9-test") {
		t.Errorf("/healthz does not report the configured version: %s", body)
	}
}

// TestHealthzStaysHealthyWithNoWorkers is the binary-level statement of the rule.
func TestHealthzStaysHealthyWithNoWorkers(t *testing.T) {
	app, srv := startApp(t, testConfig(t))
	clientTok, _, _ := seedAccounts(t, app)
	c := client{t: t, base: srv.URL, tok: clientTok}

	// No worker stream is open, so nothing is serving "ocr" — but the upload is
	// accepted within the label-grace window and sits queued.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.pdf")
	_, _ = io.WriteString(fw, "x")
	_ = mw.Close()
	_ = c.req("POST", "/upload", &buf, mw.FormDataContentType())

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d with a queued job and no workers. That is DEGRADED, not dead: "+
			"restarting the router would fix nothing, so an orchestrator acting on this would "+
			"restart it forever", resp.StatusCode)
	}
}
