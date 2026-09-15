package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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

// TestOnlyLoginAndHealthzAreUnauthenticated walks the binary's REAL route table.
//
// ⚠ THIS REPLACED TestHealthzIsTheOnlyUnauthenticatedRoute, which probed a
// hand-written list of six paths. That list was the problem: ADR-0003 added
// `GET /admin/login` and `POST /admin/login` and the old test went on passing,
// because the new routes were not in the list anyone remembered to update. A
// list kept beside the truth is a thing somebody has to maintain, and the route
// it misses is exactly the one added in a hurry.
//
// ⚠ IT WAS REWRITTEN, NOT DELETED. It is the only guard on how many
// unauthenticated routes exist, and it was being changed at the moment that
// count stopped being one — which is precisely when deleting it would have been
// easiest to justify. ADR-0003 names it in its `Enforced-by:` header.
//
// The allow-list is explicit and exact: a fourth unauthenticated route fails
// this, whoever adds it and for whatever reason.
func TestOnlyLoginAndHealthzAreUnauthenticated(t *testing.T) {
	app, srv := startApp(t, testConfig(t))

	// Routes that may answer without a credential, and why each is permitted.
	allowed := map[string]string{
		"GET /healthz":      "a load balancer and an orchestrator probe cannot hold a bearer token",
		"GET /admin/login":  "the sign-in page, which by definition precedes having a credential",
		"POST /admin/login": "the sign-in submission itself",
		// The login page is unauthenticated and needs its stylesheet, so gating
		// the assets would render the one page a locked-out operator sees as
		// unstyled HTML. Both are public, pinned, stateless static files that are
		// byte-identical for every visitor.
		"GET /admin/assets/app.css":     "the stylesheet the unauthenticated login page needs",
		"GET /admin/assets/datastar.js": "the client library, embedded in this binary",
	}

	var checked int
	for _, rt := range routesOf(t, app.Handler) {
		key := rt.method + " " + rt.path
		if _, ok := allowed[key]; ok {
			continue
		}
		// The method matters: chi answers 405 before any middleware runs, so
		// probing a POST-only route with GET would pass vacuously.
		req, err := http.NewRequest(rt.method, srv.URL+rt.path, nil)
		if err != nil {
			t.Fatalf("NewRequest %s: %v", key, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		_ = resp.Body.Close()
		checked++

		// ⚠ The property is "SERVES something", not "returns exactly 401".
		//
		// 401, 403, 404 and 405 are all safe: nothing was served. Insisting on
		// 401 specifically would make this test fail on chi's own mount
		// wildcards — `mux.Mount("/", api)` registers `/*` for every method, and
		// those 404 — and the natural repair for that noise is to loosen the
		// walk until it stops seeing real routes too. A 2xx is the thing that
		// cannot be explained away: it means an unauthenticated caller got a
		// response body.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			t.Errorf("%s served %d to a caller with NO credential. Only these may answer "+
				"unauthenticated: %v", key, resp.StatusCode, keysOf(allowed))
		}
	}

	if checked == 0 {
		t.Fatal("no routes were probed at all, so this assertion proved nothing — the walk " +
			"found nothing, or everything was allow-listed")
	}
	// And every allow-listed route must actually EXIST. An allow-list entry for a
	// route nobody mounts is a hole waiting for someone to mount it.
	mounted := map[string]bool{}
	for _, rt := range routesOf(t, app.Handler) {
		mounted[rt.method+" "+rt.path] = true
	}
	for key := range allowed {
		if !mounted[key] {
			t.Errorf("%q is allow-listed as unauthenticated but is not mounted — the exemption "+
				"outlived the route, and the next thing mounted there inherits it", key)
		}
	}
}

// route is one mounted method+path pair.
type route struct{ method, path string }

// routesOf walks a chi router, substituting a value for every path parameter so
// the result is a requestable URL.
func routesOf(t *testing.T, h http.Handler) []route {
	t.Helper()
	r, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("the binary's handler is not a chi router (%T), so the route table cannot be "+
			"walked and this test cannot do its job", h)
	}
	var out []route
	err := chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		p := pattern
		// `/files/{id}` → `/files/x`. Without this the request 404s before
		// reaching any middleware and every route looks unauthenticated.
		for {
			open := strings.Index(p, "{")
			if open < 0 {
				break
			}
			close := strings.Index(p[open:], "}")
			if close < 0 {
				break
			}
			p = p[:open] + "x" + p[open+close+1:]
		}
		p = strings.TrimSuffix(p, "/*")
		if p == "" {
			p = "/"
		}
		out = append(out, route{method: method, path: p})
		return nil
	})
	if err != nil {
		t.Fatalf("walking routes: %v", err)
	}
	return out
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
