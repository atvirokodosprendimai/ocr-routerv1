package web_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/httpapi"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
	"github.com/atvirokodosprendimai/ocr-router/internal/web"
)

var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

type env struct {
	srv       *httptest.Server
	repo      *store.Repo
	bus       *bus.Bus
	ident     *identity.Service
	rt        *router.Service
	adminTok  string
	clientTok string
	workerTok string
	clientID  string
}

// newEnv builds the dashboard behind the API's authenticator, exactly as
// cmd/router does — a dashboard mounted behind its own auth in a test would
// prove nothing about the one that ships.
func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "w.db"))
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
		LabelGrace: 5 * time.Minute, DefaultLabel: "ocr", ResultTTL: time.Hour,
	})

	ctx := context.Background()
	_, adminTok, err := ident.Bootstrap(ctx, "admin@example.com", base)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ap, _ := ident.Authenticate(ctx, "Bearer "+adminTok, base)

	cu, _ := ident.CreateUser(ctx, ap, "c@example.com", core.RoleClient, base)
	_ = repo.AddCredits(ctx, cu.ID, 100, "seed", base)
	_, clientTok, _ := ident.MintToken(ctx, ap, cu.ID, core.RoleClient, "c", base)

	wu, _ := ident.CreateUser(ctx, ap, "w@example.com", core.RoleWorker, base)
	_, workerTok, _ := ident.MintToken(ctx, ap, wu.ID, core.RoleWorker, "w", base)

	api := httpapi.New(httpapi.Deps{
		Identity: ident, Router: rt, Repo: repo, Blobs: blobs, Bus: b,
		MaxUpload: 1 << 20, Now: func() time.Time { return base },
	})
	dash := web.New(web.Deps{
		Identity: ident, Router: rt, Repo: repo, Bus: b, Results: res,
		PingInterval: 50 * time.Millisecond, Now: func() time.Time { return base },
	})

	mux := chi.NewRouter()
	dash.Mount(mux, api.Authenticator())
	mux.Mount("/", api)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &env{
		srv: srv, repo: repo, bus: b, ident: ident, rt: rt,
		adminTok: adminTok, clientTok: clientTok, workerTok: workerTok, clientID: cu.ID,
	}
}

func (e *env) do(t *testing.T, method, path, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

var adminPaths = []struct {
	method, path string
}{
	{"GET", "/admin"},
	{"GET", "/admin/users"},
	{"GET", "/admin/services"},
	{"GET", "/admin/stream"},
	{"POST", "/admin/users"},
	{"POST", "/admin/rates"},
}

func TestAdminRoutesRejectNonAdmin(t *testing.T) {
	e := newEnv(t)
	for _, p := range adminPaths {
		for name, tok := range map[string]string{"client": e.clientTok, "worker": e.workerTok} {
			resp := e.do(t, p.method, p.path, tok, strings.NewReader("{}"))
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s as %s = %d, want 403", p.method, p.path, name, resp.StatusCode)
			}
		}
		// And unauthenticated is 401, from the SAME middleware the API uses.
		resp := e.do(t, p.method, p.path, "", strings.NewReader("{}"))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated = %d, want 401", p.method, p.path, resp.StatusCode)
		}
	}
}

func TestAdminPagesRenderForAdmin(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/admin", "/admin/users", "/admin/services"} {
		resp := e.do(t, "GET", path, e.adminTok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "ocr-router") {
			t.Errorf("GET %s did not render the layout", path)
		}
	}
}

// TestPrincipalReachesTheDashboard is the guard for the ONE context key.
//
// Two packages each defining their own unexported key would compile and never
// match, handing every handler a zero Principal. That fails closed, so the
// symptom is "the admin is always forbidden" with nothing pointing at the cause.
func TestPrincipalReachesTheDashboard(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "GET", "/admin", e.adminTok, nil)
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("an ADMIN token was forbidden on /admin — the dashboard is reading a different " +
			"context key than the middleware writes, so every principal arrives zero-valued")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin as admin = %d, want 200", resp.StatusCode)
	}
}

func TestCreateUserRoundTrip(t *testing.T) {
	e := newEnv(t)
	body := strings.NewReader(`{"newEmail":"new@example.com","newRole":"client"}`)
	resp := e.do(t, "POST", "/admin/users", e.adminTok, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create = %d, want 200", resp.StatusCode)
	}

	u, err := e.repo.UserByEmail(context.Background(), "new@example.com")
	if err != nil {
		t.Fatalf("the user was not created: %v", err)
	}
	if u.Role != core.RoleClient {
		t.Errorf("role = %q, want client", u.Role)
	}

	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "user-table") {
		t.Error("the response did not patch the user table, so the page would not update")
	}
}

// TestCreateUserDuplicateShowsInlineError — a failure is 200 with a fragment.
//
// A 4xx carries no fragment for datastar to morph, so the page would show
// nothing at all and the operator would conclude the button is broken.
func TestCreateUserDuplicateShowsInlineError(t *testing.T) {
	e := newEnv(t)
	body := strings.NewReader(`{"newEmail":"c@example.com","newRole":"client"}`)
	resp := e.do(t, "POST", "/admin/users", e.adminTok, body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a duplicate email returned %d; every action must answer 200 with a fragment",
			resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "already registered") {
		t.Errorf("the response does not explain the failure: %s", out)
	}
}

func TestMintTokenShowsPlaintextOnce(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "POST", "/admin/users/"+e.clientID+"/tokens", e.adminTok, strings.NewReader("{}"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint = %d, want 200", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "ocr_c_") {
		t.Fatalf("the minted token is not in the response: %s", out)
	}

	// And a later page load must NOT contain it: the router keeps only a hash.
	page := e.do(t, "GET", "/admin/users", e.adminTok, nil)
	pageBody, _ := io.ReadAll(page.Body)
	if strings.Contains(string(pageBody), "ocr_c_") {
		t.Error("a token plaintext appears on a page load — it must exist only in the mint " +
			"response")
	}
}

func TestRateEditPersistsAndChangesTheCharge(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	body := strings.NewReader(`{"rateLabel":"ocr","rateValue":"4"}`)
	if resp := e.do(t, "POST", "/admin/rates", e.adminTok, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("set rate = %d, want 200", resp.StatusCode)
	}

	rate, err := e.repo.RateForLabel(ctx, "ocr")
	if err != nil || rate != 4 {
		t.Fatalf("RateForLabel = %d, %v; want 4", rate, err)
	}

	// The rate must actually affect the money, not just the row.
	_, cancel := e.bus.Subscribe(bus.WorkerTopic("ocr"))
	defer cancel()
	j, err := e.rt.Upload(ctx, e.clientID, router.UploadInput{
		Filename: "f", Body: strings.NewReader("x"),
	}, base)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := e.rt.Claim(ctx, "w1", "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := e.rt.Complete(ctx, "w1", j.ID, []string{"a", "b"}, base); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	before, _ := e.repo.UserByID(ctx, e.clientID)
	if _, err := e.rt.Deliver(ctx, e.clientID, j.ID, base); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	after, _ := e.repo.UserByID(ctx, e.clientID)

	if before.Credits-after.Credits != 8 {
		t.Errorf("charged %d, want 8 (2 units x rate 4) — the dashboard's rate edit must reach "+
			"the money, not just the table", before.Credits-after.Credits)
	}
}

func TestRateRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{
		`{"rateLabel":"","rateValue":"1"}`,
		`{"rateLabel":"ocr","rateValue":"-1"}`,
		`{"rateLabel":"ocr","rateValue":"abc"}`,
	} {
		resp := e.do(t, "POST", "/admin/rates", e.adminTok, strings.NewReader(body))
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s returned %d; failures are still 200 with a fragment", body, resp.StatusCode)
		}
		out, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(out), "error") {
			t.Errorf("%s produced no visible error: %s", body, out)
		}
	}
}

func TestAdminStreamPushesOnEvent(t *testing.T) {
	e := newEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/admin/stream", nil)
	req.Header.Set("Authorization", "Bearer "+e.adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d, want 200", resp.StatusCode)
	}

	seen := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			seen <- sc.Text()
		}
		close(seen)
	}()

	// The first push happens on connect, so the page is current immediately.
	waitForLine(t, seen, "stats", 3*time.Second)

	e.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin, JobID: "j1"})
	waitForLine(t, seen, "jobs", 3*time.Second)
}

func waitForLine(t *testing.T, ch <-chan string, needle string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				t.Fatalf("the stream closed while waiting for %q", needle)
			}
			if strings.Contains(line, needle) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q on the admin stream", needle)
		}
	}
}

// TestAdminStreamClearsWriteDeadline — same class of silent failure as the API's
// stream, and run against a server that HAS a WriteTimeout so it cannot pass
// vacuously.
func TestAdminStreamClearsWriteDeadline(t *testing.T) {
	e := newEnv(t)

	srv := httptest.NewUnstartedServer(e.srv.Config.Handler)
	srv.Config.WriteTimeout = 150 * time.Millisecond
	srv.Start()
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/admin/stream", nil)
	req.Header.Set("Authorization", "Bearer "+e.adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer resp.Body.Close()

	seen := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			seen <- sc.Text()
		}
		close(seen)
	}()
	waitForLine(t, seen, "stats", 3*time.Second)

	// Well past the WriteTimeout: without the per-stream deadline clear the
	// connection is already dead.
	time.Sleep(400 * time.Millisecond)
	e.bus.Publish(bus.AdminTopic, bus.Event{Kind: bus.KindAdmin})
	waitForLine(t, seen, "jobs", 3*time.Second)
}
