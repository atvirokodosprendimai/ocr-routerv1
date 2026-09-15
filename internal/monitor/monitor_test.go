package monitor_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/monitor"
)

var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// fixture builds a monitor whose every dependency can be broken on demand, which
// is what lets the health probes be shown to FAIL rather than merely to pass.
type fixture struct {
	dbErr   error
	blobDir string
	depth   map[string]int
	oldest  map[string]time.Time
	states  map[string]int
	labels  []string
	workers map[string]int
	results int
}

func (f *fixture) monitor(t *testing.T) (*monitor.Monitor, *monitor.Registry) {
	t.Helper()
	if f.blobDir == "" {
		f.blobDir = t.TempDir()
	}
	reg := monitor.NewRegistry()
	m := monitor.New(reg, monitor.Deps{
		Version: "test-1.2.3",
		BlobDir: f.blobDir,
		PingDB:  func(context.Context) error { return f.dbErr },
		QueueDepth: func(context.Context) (map[string]int, error) {
			return f.depth, nil
		},
		OldestQueued: func(context.Context) (map[string]time.Time, error) {
			return f.oldest, nil
		},
		JobStates:       func(context.Context) (map[string]int, error) { return f.states, nil },
		LiveLabels:      func(time.Time) []string { return f.labels },
		WorkersFor:      func(l string) int { return f.workers[l] },
		ResultsInMemory: func() int { return f.results },
		Now:             func() time.Time { return base },
	})
	return m, reg
}

func health(t *testing.T, m *monitor.Monitor) (int, healthBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.HealthHandler(rec, httptest.NewRequest("GET", "/healthz", nil))
	var body healthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /healthz: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, body
}

type healthBody struct {
	OK      bool     `json:"ok"`
	Version string   `json:"version"`
	Failed  []string `json:"failed"`
}

func scrape(t *testing.T, m *monitor.Monitor) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.MetricsHandler(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestHealthzOKWhenDependenciesWork(t *testing.T) {
	f := &fixture{}
	m, _ := f.monitor(t)

	code, body := health(t, m)
	if code != http.StatusOK || !body.OK {
		t.Fatalf("healthz = %d %+v, want 200 ok", code, body)
	}
	if body.Version != "test-1.2.3" {
		t.Errorf("version = %q", body.Version)
	}
}

// TestHealthzFailsWhenDatabaseIsUnreachable is what separates this from a
// vacuous gate.
//
// A handler that returns 200 unconditionally is WORSE than no health check,
// because it is trusted: every probe stays green through a total outage.
func TestHealthzFailsWhenDatabaseIsUnreachable(t *testing.T) {
	f := &fixture{dbErr: errors.New("database is closed")}
	m, _ := f.monitor(t)

	code, body := health(t, m)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("healthz with a dead database = %d, want 503 — the probe does not actually probe",
			code)
	}
	if body.OK {
		t.Error("body says ok with a dead database")
	}
	if len(body.Failed) != 1 || body.Failed[0] != "db" {
		t.Errorf("failed = %v, want [db] — it must name WHICH dependency broke", body.Failed)
	}
}

func TestHealthzFailsWhenBlobDirUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		// ⚠ Root ignores mode bits, so this case CANNOT fail when the suite runs
		// as root — which is the default in many CI containers. Skipping with a
		// stated reason is honest; letting it run and pass would be a green test
		// that cannot go red, which is the exact defect this task guards against
		// elsewhere.
		t.Skip("running as root: mode bits are not enforced, so this probe cannot be made to fail")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	f := &fixture{blobDir: dir}
	m, _ := f.monitor(t)

	code, body := health(t, m)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("healthz with a read-only blob dir = %d, want 503 — the database can be "+
			"perfectly healthy while every upload is about to fail", code)
	}
	if len(body.Failed) != 1 || body.Failed[0] != "blobs" {
		t.Errorf("failed = %v, want [blobs]", body.Failed)
	}
}

// TestHealthzIgnoresWorkerAvailability pins the distinction that prevents an
// outage.
//
// Zero workers is DEGRADED, not dead. An orchestrator acting on it would restart
// the one component still working, and the restart would fix nothing — so it
// would do it again. This assertion is what stops a later "improvement" turning
// the health check into a restart loop.
func TestHealthzIgnoresWorkerAvailability(t *testing.T) {
	f := &fixture{
		labels:  []string{"ocr", "crawl"},
		workers: map[string]int{}, // nobody serving anything
		depth:   map[string]int{"ocr": 500},
	}
	m, _ := f.monitor(t)

	code, body := health(t, m)
	if code != http.StatusOK || !body.OK {
		t.Errorf("healthz with zero workers and 500 queued jobs = %d %+v, want 200 ok. Worker "+
			"availability is a METRIC and an alert, never a liveness input: the router is fine "+
			"and restarting it would fix nothing", code, body)
	}
}

func TestWorkersLiveReflectsSubscribers(t *testing.T) {
	f := &fixture{
		labels:  []string{"ocr", "crawl"},
		workers: map[string]int{"ocr": 2},
	}
	m, _ := f.monitor(t)
	out := scrape(t, m)

	if !strings.Contains(out, `ocrr_workers_live{label="ocr"} 2`) {
		t.Errorf("ocr workers not reported as 2:\n%s", out)
	}
	// ★ The silent failure: a known label with nobody serving it must appear as
	// an explicit ZERO. Omitting the series would make "no worker" and "no such
	// label" indistinguishable, and no alert can fire on an absent series.
	if !strings.Contains(out, `ocrr_workers_live{label="crawl"} 0`) {
		t.Errorf("a label with no workers is not reported as 0 — an absent series cannot be "+
			"alerted on:\n%s", out)
	}
}

func TestQueueOldestAgeReflectsStall(t *testing.T) {
	f := &fixture{
		labels: []string{"ocr"},
		depth:  map[string]int{"ocr": 1},
		oldest: map[string]time.Time{"ocr": base.Add(-300 * time.Second)},
	}
	m, _ := f.monitor(t)
	out := scrape(t, m)

	if !strings.Contains(out, `ocrr_queue_oldest_age_seconds{label="ocr"} 300`) {
		t.Errorf("the stall signal is wrong; depth alone would report a harmless-looking 1:\n%s",
			out)
	}
}

func TestQueueDepthIsPerLabel(t *testing.T) {
	f := &fixture{
		labels: []string{"ocr", "crawl"},
		depth:  map[string]int{"ocr": 7, "crawl": 2},
	}
	m, _ := f.monitor(t)
	out := scrape(t, m)

	if !strings.Contains(out, `ocrr_queue_depth{label="ocr"} 7`) ||
		!strings.Contains(out, `ocrr_queue_depth{label="crawl"} 2`) {
		t.Errorf("depths are not reported per label:\n%s", out)
	}
	if strings.Contains(out, "ocrr_queue_depth 9") {
		t.Error("depths were summed across labels")
	}
}

func TestCountersAppearInTheScrape(t *testing.T) {
	f := &fixture{}
	m, reg := f.monitor(t)

	reg.Inc(monitor.MetricJobsTotal, map[string]string{"state": "delivered"})
	reg.Inc(monitor.MetricJobsTotal, map[string]string{"state": "delivered"})
	reg.Inc(monitor.MetricJobsTotal, map[string]string{"state": "expired"})
	reg.Add(monitor.MetricCreditsDebited, nil, 12)
	reg.Inc(monitor.MetricReaperActions, map[string]string{"action": "lease-expired"})

	out := scrape(t, m)
	for _, want := range []string{
		`ocrr_jobs_total{state="delivered"} 2`,
		`ocrr_jobs_total{state="expired"} 1`,
		`ocrr_credits_debited_total 12`,
		`ocrr_reaper_actions_total{action="lease-expired"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestMetricLabelAllowListPanics is the cardinality guard at registration.
func TestMetricLabelAllowListPanics(t *testing.T) {
	reg := monitor.NewRegistry()

	for _, bad := range []string{"user_id", "job_id", "email", "customer"} {
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("registering a metric with the label %q did not panic — unbounded "+
						"label values are how a metrics endpoint kills the scraper it feeds", bad)
				}
			}()
			reg.Inc("ocrr_test", map[string]string{bad: "x"})
		})
	}

	// And the permitted three are accepted, so the guard is not simply refusing
	// everything — a check that rejects all input passes every negative test.
	for _, ok := range []string{"label", "state", "action"} {
		reg.Inc("ocrr_test", map[string]string{ok: "x"})
	}
}

// TestExportedLabelNamesAreOnlyAllowListed parses the RENDERED output.
//
// Registration-time enforcement is bypassed by any hand-written exposition line,
// so the check that matters reads what actually goes over the wire.
func TestExportedLabelNamesAreOnlyAllowListed(t *testing.T) {
	f := &fixture{
		labels:  []string{"ocr"},
		depth:   map[string]int{"ocr": 1},
		oldest:  map[string]time.Time{"ocr": base},
		states:  map[string]int{"queued": 1, "delivered": 2},
		workers: map[string]int{"ocr": 1},
		results: 3,
	}
	m, reg := f.monitor(t)
	reg.Inc(monitor.MetricJobsTotal, map[string]string{"state": "dead"})
	reg.Inc(monitor.MetricReaperActions, map[string]string{"action": "result-swept"})

	out := scrape(t, m)

	allowed := map[string]bool{"label": true, "state": true, "action": true}
	labelName := regexp.MustCompile(`[{,]([a-zA-Z_][a-zA-Z0-9_]*)=`)
	found := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		for _, m := range labelName.FindAllStringSubmatch(line, -1) {
			found++
			if !allowed[m[1]] {
				t.Errorf("the scrape exports the label name %q, which is not allow-listed: %s",
					m[1], line)
			}
		}
	}
	if found == 0 {
		t.Fatal("no labelled series were found at all, so this assertion proved nothing")
	}
}

func TestPrometheusTextFormatHasTypeLines(t *testing.T) {
	f := &fixture{labels: []string{"ocr"}, workers: map[string]int{"ocr": 1}}
	m, _ := f.monitor(t)
	out := scrape(t, m)

	for _, want := range []string{
		"# TYPE ocrr_workers_live gauge",
		"# TYPE ocrr_queue_depth gauge",
		"# TYPE ocrr_queue_oldest_age_seconds gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	// No duplicate series: a scraper rejects them.
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key := strings.Fields(line)[0]
		if seen[key] {
			t.Errorf("duplicate series %q", key)
		}
		seen[key] = true
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	f := &fixture{
		labels:  []string{`we"ird\label`},
		workers: map[string]int{`we"ird\label`: 1},
	}
	m, _ := f.monitor(t)
	out := scrape(t, m)
	if !strings.Contains(out, `label="we\"ird\\label"`) {
		t.Errorf("label values are not escaped, so one quote would corrupt the scrape:\n%s", out)
	}
}

// TestMetricsServesOnItsOwnListener drives a REAL socket.
//
// httptest proves the handler renders; only a real bind proves the second
// listener exists at all, which is the whole of the exposure decision.
func TestMetricsServesOnItsOwnListener(t *testing.T) {
	f := &fixture{labels: []string{"ocr"}, workers: map[string]int{"ocr": 1}}
	m, _ := f.monitor(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, shutdown, err := m.Serve(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics on the metrics listener: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", resp.StatusCode)
	}

	// ★ And nothing else is on that listener. A metrics bind that also answers
	// /healthz, /upload or anything authenticated would be an unauthenticated
	// second front door.
	for _, path := range []string{"/healthz", "/upload", "/files/x", "/"} {
		r, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("the metrics listener answers %s with %d — it must serve /metrics and "+
				"nothing else", path, r.StatusCode)
		}
	}
}

// TestMetricsListenerBindsWhereItIsTold asserts the ACTUAL bound address.
//
// ⚠ Asserting a configured string would prove nothing about the socket. Serve
// returns what the listener resolved to, and that is what is checked here; the
// loopback DEFAULT is asserted the same way over in cmd/router, against the flag
// default the binary really ships.
func TestMetricsListenerBindsWhereItIsTold(t *testing.T) {
	f := &fixture{}
	m, _ := f.monitor(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, shutdown, err := m.Serve(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Errorf("asked for 127.0.0.1 and the socket bound %q", host)
	}
	if port == "0" {
		t.Error("Serve returned the requested port rather than the resolved one, so no test " +
			"using it can assert anything about the real socket")
	}
}

func TestMetricsListenerStopsWithContext(t *testing.T) {
	f := &fixture{}
	m, _ := f.monitor(t)

	ctx, cancel := context.WithCancel(context.Background())
	addr, _, err := m.Serve(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if _, err := http.Get("http://" + addr + "/metrics"); err != nil {
		t.Fatalf("listener was not up before cancel: %v", err)
	}

	cancel()

	// Poll rather than sleep-and-hope: the close is asynchronous, and a fixed
	// sleep either flakes or wastes time.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := http.Get("http://" + addr + "/metrics"); err != nil {
			return // refused: the listener went away with the context
		}
		if time.Now().After(deadline) {
			t.Fatal("the metrics listener is still serving after its context was cancelled — " +
				"shutdown leaks a socket on every restart")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestScrapeIsRepeatable(t *testing.T) {
	// Two scrapes with nothing happening in between must be identical. A gauge
	// that drifts on read is maintaining state it should be deriving.
	//
	// That a scrape performs no WRITES is proved where a real database exists:
	// cmd/router's TestGaugesDoNotTouchTheWritePath scrapes an app whose read
	// handle is opened query_only, so a write would fail rather than merely be
	// absent from this assertion.
	f := &fixture{labels: []string{"ocr"}, workers: map[string]int{"ocr": 1}}
	m, _ := f.monitor(t)
	before := scrape(t, m)
	after := scrape(t, m)
	if before != after {
		t.Error("two consecutive scrapes differ with no state change — a scrape is mutating " +
			"something")
	}
}
