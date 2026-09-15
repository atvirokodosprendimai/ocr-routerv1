package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/httpapi"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/logging"
	"github.com/atvirokodosprendimai/ocr-router/internal/ratelimit"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// countingCounter records metric increments for assertion.
type countingCounter struct {
	mu  sync.Mutex
	got map[string]int64
}

func newCounter() *countingCounter { return &countingCounter{got: map[string]int64{}} }

func (c *countingCounter) Inc(name string, labels map[string]string) { c.Add(name, labels, 1) }

func (c *countingCounter) Add(name string, labels map[string]string, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := name
	for k, v := range labels {
		key += "|" + k + "=" + v
	}
	c.got[key] += n
}

func (c *countingCounter) value(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[key]
}

// limitedEnv is newEnv with a limiter, a real logger and a counter attached.
//
// It builds the graph rather than reusing newEnv because the limits have to be
// set at construction — and because the log assertions need to read real emitted
// bytes, not a mock: a mock proves the call was made, and only the bytes show
// whether a secret was in them.
type limitedEnv struct {
	srv       *httptest.Server
	logs      *lockedBuffer
	counter   *countingCounter
	limiter   *ratelimit.Limiter
	clientTok string
	client2   string
	workerTok string
	adminTok  string
	bus       *bus.Bus
}

// lockedBuffer is a bytes.Buffer safe for the server goroutine to write while
// the test goroutine reads.
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

func newLimitedEnv(t *testing.T, limits httpapi.RoleLimits) *limitedEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir + "/api.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blob.New(dir + "/blobs")
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	repo := store.NewRepo(db)
	b := bus.New()
	ident := identity.New(repo)
	rt := router.New(repo, blobs, results.New(time.Hour), b, router.Config{
		Lease: 5 * time.Minute, MaxAttempts: 3, AgingStep: time.Minute,
		LabelGrace: 5 * time.Minute, DefaultLabel: "ocr",
	})

	ctx := context.Background()
	_, adminTok, err := ident.Bootstrap(ctx, "admin@example.com", base)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ap, _ := ident.Authenticate(ctx, "Bearer "+adminTok, base)

	client, _ := ident.CreateUser(ctx, ap, "c@example.com", core.RoleClient, base)
	_ = repo.AddCredits(ctx, client.ID, 1000, "test", base)
	// ★ TWO tokens for the SAME user. Per-user and per-token limiting are
	// indistinguishable until this exists.
	_, clientTok, _ := ident.MintToken(ctx, ap, client.ID, core.RoleClient, "c1", base)
	_, client2, _ := ident.MintToken(ctx, ap, client.ID, core.RoleClient, "c2", base)

	worker, _ := ident.CreateUser(ctx, ap, "w@example.com", core.RoleWorker, base)
	_, workerTok, _ := ident.MintToken(ctx, ap, worker.ID, core.RoleWorker, "w", base)

	logs := &lockedBuffer{}
	log, err := logging.New(logging.Options{Out: logs})
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	counter := newCounter()
	limiter := ratelimit.New(func() time.Time { return base }, time.Hour)

	api := httpapi.New(httpapi.Deps{
		Identity: ident, Router: rt, Repo: repo, Blobs: blobs, Bus: b,
		MaxUpload: 1 << 20, PingInterval: 50 * time.Millisecond,
		Now:     func() time.Time { return base },
		Limiter: limiter, Limits: limits, Logger: log, Counter: counter,
	})
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	return &limitedEnv{
		srv: srv, logs: logs, counter: counter, limiter: limiter,
		clientTok: clientTok, client2: client2, workerTok: workerTok,
		adminTok: adminTok, bus: b,
	}
}

func (e *limitedEnv) get(t *testing.T, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// tightClient is a burst small enough to trigger quickly, with an RPS low enough
// that the frozen clock never refills it.
var tightClient = httpapi.RoleLimits{
	Client: ratelimit.Limit{RPS: 1, Burst: 3},
	Worker: ratelimit.Limit{RPS: 1, Burst: 3},
	Admin:  ratelimit.Limit{RPS: 1, Burst: 3},
}

func TestRequestPastBurstIs429(t *testing.T) {
	e := newLimitedEnv(t, tightClient)

	for i := 0; i < 3; i++ {
		if got := e.get(t, "/services", e.clientTok).StatusCode; got != http.StatusOK {
			t.Fatalf("call %d = %d, want 200 — the burst is not being honoured", i+1, got)
		}
	}
	resp := e.get(t, "/services", e.clientTok)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the call past the burst = %d, want 429 — the limiter is not applied to the "+
			"route table at all", resp.StatusCode)
	}
}

func TestRetryAfterHeaderIsPresentAndPositive(t *testing.T) {
	e := newLimitedEnv(t, tightClient)
	for i := 0; i < 3; i++ {
		e.get(t, "/services", e.clientTok)
	}

	resp := e.get(t, "/services", e.clientTok)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("not throttled, so there is no header to check")
	}
	raw := resp.Header.Get("Retry-After")
	if raw == "" {
		t.Fatal("no Retry-After on a 429 — the client has to guess when to come back")
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After = %q, which is not an integer", raw)
	}
	if secs < 1 {
		t.Errorf("Retry-After = %d — rounding down tells a client to retry straight into "+
			"another refusal", secs)
	}
}

// TestTwoTokensDoNotShareABucket is red if the limiter runs outside the
// authenticator, where every caller presents the zero Principal.
func TestTwoTokensDoNotShareABucket(t *testing.T) {
	e := newLimitedEnv(t, tightClient)

	for i := 0; i < 4; i++ {
		e.get(t, "/services", e.clientTok)
	}
	if got := e.get(t, "/services", e.clientTok).StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("the first token is not exhausted (%d), so this test proves nothing", got)
	}

	if got := e.get(t, "/services", e.workerTok).StatusCode; got == http.StatusTooManyRequests {
		t.Error("a second caller was throttled by the first's traffic — the limiter is keyed on " +
			"something shared, which means it runs before authentication and every caller in the " +
			"system shares one bucket")
	}
}

// TestTwoTokensOfTheSameUserDoNotShareABucket is the one that distinguishes
// per-token from per-user.
//
// The backlog asked for per-TOKEN deliberately: the threat is a leaked
// credential, and a per-user limit lets a compromised token consume the
// legitimate one's allowance. A per-user implementation passes the test above
// and fails this one.
func TestTwoTokensOfTheSameUserDoNotShareABucket(t *testing.T) {
	e := newLimitedEnv(t, tightClient)

	for i := 0; i < 4; i++ {
		e.get(t, "/services", e.clientTok)
	}
	if got := e.get(t, "/services", e.clientTok).StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("the first token is not exhausted (%d), so this test proves nothing", got)
	}

	if got := e.get(t, "/services", e.client2).StatusCode; got == http.StatusTooManyRequests {
		t.Error("a second token of the SAME user was throttled by the first's traffic — the " +
			"limit is per user, so one leaked credential consumes the legitimate one's allowance")
	}
}

func TestUnauthenticatedRequestConsumesNoBudget(t *testing.T) {
	e := newLimitedEnv(t, tightClient)

	for i := 0; i < 50; i++ {
		if got := e.get(t, "/services", "").StatusCode; got != http.StatusUnauthorized {
			t.Fatalf("tokenless call %d = %d, want 401", i, got)
		}
	}
	if got := e.get(t, "/services", e.clientTok).StatusCode; got != http.StatusOK {
		t.Errorf("a valid caller got %d after a tokenless flood — the limiter runs before "+
			"authentication, so anyone can exhaust everyone's budget without a credential",
			got)
	}
}

func TestWorkerAndClientLimitsDiffer(t *testing.T) {
	e := newLimitedEnv(t, httpapi.RoleLimits{
		Client: ratelimit.Limit{RPS: 1, Burst: 2},
		Worker: ratelimit.Limit{RPS: 1, Burst: 10},
		Admin:  ratelimit.Limit{RPS: 1, Burst: 10},
	})

	for i := 0; i < 3; i++ {
		e.get(t, "/services", e.clientTok)
	}
	if got := e.get(t, "/services", e.clientTok).StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("the client at burst 2 was not throttled after 4 calls (%d)", got)
	}

	for i := 0; i < 5; i++ {
		if got := e.get(t, "/services", e.workerTok).StatusCode; got == http.StatusTooManyRequests {
			t.Fatalf("the worker at burst 10 was throttled on call %d — the role lookup is being "+
				"dropped and one limit is applied to everyone", i+1)
		}
	}
}

// TestUnauthorizedRequestIsLogged is red if the logger is applied inside the
// auth group — the mistake that reads as working.
func TestUnauthorizedRequestIsLogged(t *testing.T) {
	e := newLimitedEnv(t, tightClient)
	e.get(t, "/services", "")

	line := findLogLine(t, e.logs.String(), 401)
	if line == nil {
		t.Fatalf("no log line for a 401. The request log is mounted inside the authenticator, "+
			"so it cannot answer 'is someone hammering us with a revoked token' — which is the "+
			"question it is most needed for. Captured:\n%s", e.logs.String())
	}
}

func TestThrottledRequestIsLogged(t *testing.T) {
	e := newLimitedEnv(t, tightClient)
	for i := 0; i < 5; i++ {
		e.get(t, "/services", e.clientTok)
	}
	if findLogLine(t, e.logs.String(), 429) == nil {
		t.Errorf("no log line for a 429:\n%s", e.logs.String())
	}
}

// TestLogUsesRoutePatternNotRawPath asserts the pattern IS present, so an empty
// route field fails too.
func TestLogUsesRoutePatternNotRawPath(t *testing.T) {
	e := newLimitedEnv(t, tightClient)
	const id = "0192f1a2-b3c4-7d5e-8f90-123456789abc"
	e.get(t, "/files/"+id, e.clientTok)

	out := e.logs.String()
	if !strings.Contains(out, `"route":"/files/{id}"`) {
		t.Errorf("the log does not carry the route PATTERN, so grouping by route is impossible:"+
			"\n%s", out)
	}
	if strings.Contains(out, id) {
		t.Errorf("the raw job id appears in the log's route field, making every line unique:\n%s",
			out)
	}
}

func TestLogCarriesPrincipalFields(t *testing.T) {
	e := newLimitedEnv(t, tightClient)
	e.get(t, "/services", e.clientTok)

	out := e.logs.String()
	for _, want := range []string{`"user_id"`, `"token_id"`, `"role":"client"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in the request log:\n%s", want, out)
		}
	}
	// ★ The bearer secret must never appear. It is the one string in the request
	// that grants access if it leaks into an aggregator.
	if strings.Contains(out, e.clientTok) {
		t.Errorf("the bearer token itself is in the log line:\n%s", out)
	}
}

func TestThrottleCounterIncrements(t *testing.T) {
	e := newLimitedEnv(t, tightClient)
	for i := 0; i < 5; i++ {
		e.get(t, "/services", e.clientTok)
	}
	if got := e.counter.value("ocrr_requests_throttled_total|role=client"); got == 0 {
		t.Error("throttling moved no counter, so an operator cannot see it happening before the " +
			"customer complains")
	}
}

func TestNilLimiterMeansNoLimiting(t *testing.T) {
	// newEnv builds the API with no Limiter at all — the shape every test written
	// before ADR-0002 uses, and the one that must keep working.
	e := newEnv(t)
	for i := 0; i < 200; i++ {
		resp := e.do(t, "GET", "/services", e.clientTok, nil, "")
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("call %d was throttled with no limiter configured", i+1)
		}
	}
}

func TestNilLoggerDoesNotPanic(t *testing.T) {
	e := newEnv(t)
	if got := e.do(t, "GET", "/services", e.clientTok, nil, "").StatusCode; got != http.StatusOK {
		t.Errorf("GET /services with no logger = %d", got)
	}
}

func TestRateLimitedErrorMapsTo429(t *testing.T) {
	e := newLimitedEnv(t, tightClient)
	for i := 0; i < 5; i++ {
		e.get(t, "/services", e.clientTok)
	}
	resp := e.get(t, "/services", e.clientTok)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("the 429 body is not the standard error shape: %v", err)
	}
	if body.Error != core.ErrRateLimited.Error() {
		t.Errorf("error = %q, want %q", body.Error, core.ErrRateLimited.Error())
	}
}

// findLogLine returns the first JSON log record with the given status.
func findLogLine(t *testing.T, out string, status int) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if s, ok := rec["status"].(float64); ok && int(s) == status {
			return rec
		}
	}
	return nil
}
