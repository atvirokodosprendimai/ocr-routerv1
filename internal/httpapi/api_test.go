package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/httpapi"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

type env struct {
	api       *httpapi.API
	srv       *httptest.Server
	repo      *store.Repo
	bus       *bus.Bus
	blobs     *blob.Store
	results   *results.Store
	rt        *router.Service
	ident     *identity.Service
	adminTok  string
	clientTok string
	workerTok string
	clientID  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "api.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blob.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	repo := store.NewRepo(db)
	res := results.New(time.Hour)
	b := bus.New()
	ident := identity.New(repo)
	rt := router.New(repo, blobs, res, b, router.Config{
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
	if err := repo.AddCredits(ctx, client.ID, 100, "test", base); err != nil {
		t.Fatalf("AddCredits: %v", err)
	}
	_, clientTok, _ := ident.MintToken(ctx, ap, client.ID, core.RoleClient, "c", base)

	worker, _ := ident.CreateUser(ctx, ap, "w@example.com", core.RoleWorker, base)
	_, workerTok, _ := ident.MintToken(ctx, ap, worker.ID, core.RoleWorker, "w", base)

	api := httpapi.New(httpapi.Deps{
		Identity: ident, Router: rt, Repo: repo, Blobs: blobs, Bus: b,
		MaxUpload: 1 << 20, PingInterval: 50 * time.Millisecond,
		Now: func() time.Time { return base },
	})
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	return &env{
		api: api, srv: srv, repo: repo, bus: b, blobs: blobs, results: res,
		rt: rt, ident: ident,
		adminTok: adminTok, clientTok: clientTok, workerTok: workerTok,
		clientID: client.ID,
	}
}

// liveWorker makes a label valid by subscribing to it.
func (e *env) liveWorker(t *testing.T, label string) {
	t.Helper()
	_, cancel := e.bus.Subscribe(bus.WorkerTopic(label))
	t.Cleanup(cancel)
}

func (e *env) do(t *testing.T, method, path, token string, body io.Reader, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func multipartBody(t *testing.T, name, content string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		t.Fatalf("writing part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// TestUnauthenticatedIsRejected is driven from the REAL route table.
//
// A hand-written list of routes goes stale the first time someone adds one in a
// hurry — and the route added in a hurry is exactly the one most likely to be
// missing its guard. Walking what is actually mounted means a new unguarded
// route fails this test the moment it appears.
func TestUnauthenticatedIsRejected(t *testing.T) {
	e := newEnv(t)
	routes := e.api.Routes()
	if len(routes) == 0 {
		t.Fatal("Routes() returned nothing — the table-driven guard would be vacuous")
	}
	for _, rt := range routes {
		path := strings.ReplaceAll(rt.Pattern, "{id}", "0192f8aa-0000-7000-8000-000000000000")
		resp := e.do(t, rt.Method, path, "", nil, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", rt.Method, path, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: no WWW-Authenticate challenge", rt.Method, path)
		}
	}
}

func TestEveryRouteIsMounted(t *testing.T) {
	e := newEnv(t)
	want := map[string]bool{
		"POST /upload":    false,
		"GET /sse":        false,
		"GET /files/{id}": false,
		"POST /claim":     false,
		"GET /services":   false,
	}
	for _, rt := range e.api.Routes() {
		key := rt.Method + " " + rt.Pattern
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, mounted := range want {
		if !mounted {
			t.Errorf("%s is not mounted — a handler that exists and is not routed is finished, "+
				"tested, and called by nothing", key)
		}
	}
}

func TestClaimRequiresWorkerRole(t *testing.T) {
	e := newEnv(t)
	for name, tok := range map[string]string{"client": e.clientTok, "admin": e.adminTok} {
		resp := e.do(t, "POST", "/claim?label=ocr", tok, nil, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s posting /claim = %d, want 403", name, resp.StatusCode)
		}
	}
}

// TestUploadBranchesOnRoleNotBody is the role-confusion guard.
func TestUploadBranchesOnRoleNotBody(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")

	// A CLIENT token posting a worker-shaped JSON result. If the handler chose
	// its branch from the body, this client would complete its own job with
	// output it wrote itself — and be charged for it.
	body := strings.NewReader(`{"job_id":"whatever","units":["forged"]}`)
	resp := e.do(t, "POST", "/upload", e.clientTok, body, "application/json")
	if resp.StatusCode == http.StatusNoContent {
		t.Fatal("a client token completed a job by posting a worker-shaped body — the handler " +
			"branched on the PAYLOAD instead of on the authenticated role")
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("client posting a JSON body = %d, want 400", resp.StatusCode)
	}
}

func TestUploadClientCreatesJob(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")

	body, ct := multipartBody(t, "doc.pdf", "hello")
	resp := e.do(t, "POST", "/upload", e.clientTok, body, ct)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	var got struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	job, err := e.repo.JobByID(context.Background(), got.JobID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job.State != core.JobQueued || job.Filename != "doc.pdf" || !job.HasBlob {
		t.Errorf("job = %+v", job)
	}
}

func TestUploadReadsLabelAndParams(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "crawl")

	resp := e.do(t, "POST", "/upload?label=crawl&url=https%3A%2F%2Fexample.com&max-depth=2",
		e.clientTok, nil, "")
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload = %d (%s), want 201", resp.StatusCode, b)
	}
	var got struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)

	job, _ := e.repo.JobByID(context.Background(), got.JobID)
	if job.Label != "crawl" {
		t.Errorf("label = %q, want crawl", job.Label)
	}
	if job.Params["url"] != "https://example.com" || job.Params["max-depth"] != "2" {
		t.Errorf("params = %v", job.Params)
	}
	// The routing keys must not leak into the subprocess parameters.
	if _, leaked := job.Params["label"]; leaked {
		t.Error("the reserved key 'label' leaked into the job's params")
	}
}

func TestUploadAcceptsPipeline(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "crawl")
	e.liveWorker(t, "strip-html")

	resp := e.do(t, "POST", "/upload?pipeline=crawl,strip-html&url=x", e.clientTok, nil, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	var got struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	job, _ := e.repo.JobByID(context.Background(), got.JobID)
	if len(job.Pipeline) != 2 || job.Pipeline[0] != "crawl" || job.Label != "crawl" {
		t.Errorf("pipeline = %v, label = %q", job.Pipeline, job.Label)
	}
}

func TestUploadWithNoBodyIsValid(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "crawl")

	// The crawler shape: no body at all.
	resp := e.do(t, "POST", "/upload?label=crawl&url=x", e.clientTok, nil, "")
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("a params-only upload = %d, want 201 — it is a legitimate shape, not a "+
			"malformed request", resp.StatusCode)
	}
}

func TestUploadRejectsUnknownLabelWithAvailable(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")

	resp := e.do(t, "POST", "/upload?label=strip-htm", e.clientTok, nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown label = %d, want 404", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "ocr") {
		t.Errorf("the refusal %q does not name the available labels — a typo should fail "+
			"immediately and say what IS possible, not wait for the deadline", b)
	}
}

func TestUploadRejectsOversizeBody(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")

	body, ct := multipartBody(t, "big.pdf", strings.Repeat("x", 2<<20)) // 2 MiB vs a 1 MiB cap
	resp := e.do(t, "POST", "/upload", e.clientTok, body, ct)
	if resp.StatusCode == http.StatusCreated {
		t.Errorf("an oversize upload was accepted (%d)", resp.StatusCode)
	}
}

func TestErrorStatusMapping(t *testing.T) {
	// Table-driven over EVERY sentinel in core, so a new one cannot quietly
	// default to 500 in a handler nobody revisited.
	cases := []struct {
		err  error
		want int
	}{
		{core.ErrUnauthorized, http.StatusUnauthorized},
		{core.ErrForbidden, http.StatusForbidden},
		{core.ErrNotFound, http.StatusNotFound},
		{core.ErrConflict, http.StatusConflict},
		{core.ErrNoCredits, http.StatusPaymentRequired},
		{core.ErrBufferFull, http.StatusTooManyRequests},
		{core.ErrInvalidParam, http.StatusBadRequest},
		{core.ErrInvalidState, http.StatusBadRequest},
	}
	for _, c := range cases {
		if got := httpapi.StatusForTest(c.err); got != c.want {
			t.Errorf("statusFor(%v) = %d, want %d", c.err, got, c.want)
		}
		// Wrapped errors must map identically: every service here adds context.
		wrapped := fmt.Errorf("context: %w", c.err)
		if got := httpapi.StatusForTest(wrapped); got != c.want {
			t.Errorf("statusFor(wrapped %v) = %d, want %d — the mapper must use errors.Is",
				c.err, got, c.want)
		}
	}
}
