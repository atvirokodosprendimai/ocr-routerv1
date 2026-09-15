package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/web"
)

const testPassword = "correct horse battery staple"

// runCLI executes the real command tree with captured stdout.
//
// Going through newCLI() rather than calling the action directly is the point:
// a subcommand that exists in the code and is not on the command tree is
// unreachable by an operator, and only the real tree shows that.
func runCLI(t *testing.T, stdin string, args ...string) (out string, err error) {
	t.Helper()

	// The prompt seam. The real term.ReadPassword needs a TTY, which CI has not;
	// substituting it covers the ASK-TWICE behaviour while leaving the terminal
	// handling to the human sign-off, which is honest about what is proved.
	orig := readPassword
	readPassword = readPasswordFrom(strings.NewReader(stdin))
	defer func() { readPassword = orig }()

	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("pipe: %v", perr)
	}
	origStdout := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	err = newCLI().Run(context.Background(), args)

	os.Stdout = origStdout
	_ = w.Close()
	out = <-done
	_ = r.Close()
	return out, err
}

// bootstrapArgs builds a command line against a temporary database.
func bootstrapArgs(t *testing.T, extra ...string) (args []string, cfg Config) {
	t.Helper()
	cfg = testConfig(t)
	base := []string{
		"router", "admin", "bootstrap",
		"--db", cfg.DBPath, "--blobs", cfg.BlobDir,
		"--email", "admin@example.com",
	}
	return append(base, extra...), cfg
}

func TestBootstrapWithPasswordAllowsLogin(t *testing.T) {
	args, cfg := bootstrapArgs(t, "--password", testPassword)
	if _, err := runCLI(t, "", args...); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	app, err := buildApp(cfg)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer func() { _ = app.Close() }()

	if _, err := app.Ident.Login(context.Background(),
		"admin@example.com", testPassword, time.Now()); err != nil {
		t.Errorf("the bootstrapped administrator cannot log in: %v", err)
	}
}

// TestBootstrapWithoutPasswordLeavesLoginDisabled — today's behaviour preserved.
// The account still works by token; it simply cannot reach the dashboard.
func TestBootstrapWithoutPasswordLeavesLoginDisabled(t *testing.T) {
	args, cfg := bootstrapArgs(t)
	// No stdin and no TTY, so no prompt is attempted.
	out, err := runCLI(t, "", args...)
	if err != nil {
		t.Fatalf("bootstrap with no password: %v", err)
	}
	if !strings.Contains(out, "token:") {
		t.Fatalf("bootstrap printed no token:\n%s", out)
	}

	app, err := buildApp(cfg)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer func() { _ = app.Close() }()

	if _, err := app.Ident.Login(context.Background(),
		"admin@example.com", testPassword, time.Now()); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("login = %v, want core.ErrUnauthorized — an account nobody gave a password to "+
			"must not be able to log in", err)
	}
}

// TestSetPasswordChangesTheCredential asserts BOTH halves.
//
// "the new one works" alone passes against a no-op that never rotated anything.
func TestSetPasswordChangesTheCredential(t *testing.T) {
	args, cfg := bootstrapArgs(t, "--password", testPassword)
	if _, err := runCLI(t, "", args...); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	const newPassword = "a completely different passphrase"
	if _, err := runCLI(t, "",
		"router", "admin", "set-password",
		"--db", cfg.DBPath, "--blobs", cfg.BlobDir,
		"--email", "admin@example.com", "--password", newPassword); err != nil {
		t.Fatalf("set-password: %v", err)
	}

	app, err := buildApp(cfg)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer func() { _ = app.Close() }()
	ctx := context.Background()

	if _, err := app.Ident.Login(ctx, "admin@example.com", newPassword, time.Now()); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
	if _, err := app.Ident.Login(ctx, "admin@example.com", testPassword, time.Now()); err == nil {
		t.Error("the OLD password still works after set-password — nothing was rotated")
	}
}

func TestSetPasswordRefusesUnknownEmail(t *testing.T) {
	args, cfg := bootstrapArgs(t, "--password", testPassword)
	if _, err := runCLI(t, "", args...); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	_, err := runCLI(t, "",
		"router", "admin", "set-password",
		"--db", cfg.DBPath, "--blobs", cfg.BlobDir,
		"--email", "nobody@example.com", "--password", testPassword)
	if err == nil {
		t.Error("set-password on an unknown email succeeded")
	}
}

func TestSetPasswordRefusesNonAdmin(t *testing.T) {
	args, cfg := bootstrapArgs(t, "--password", testPassword)
	if _, err := runCLI(t, "", args...); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// A client account, created the way the product creates one.
	app, err := buildApp(cfg)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	ctx := context.Background()
	admin, adminTok, err := app.Ident.Bootstrap(ctx, "second@example.com", time.Now())
	if err != nil {
		// Already bootstrapped — mint through the existing admin instead.
		_ = admin
		adminTok = ""
	}
	if adminTok == "" {
		// Recover the first admin's principal by looking the user up directly.
		u, uerr := app.Repo.UserByEmail(ctx, "admin@example.com")
		if uerr != nil {
			t.Fatalf("UserByEmail: %v", uerr)
		}
		if _, err := app.Ident.CreateUser(ctx,
			core.Principal{UserID: u.ID, Role: core.RoleAdmin, TokenID: "t"},
			"client@example.com", core.RoleClient, time.Now()); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
	}
	_ = app.Close()

	_, err = runCLI(t, "",
		"router", "admin", "set-password",
		"--db", cfg.DBPath, "--blobs", cfg.BlobDir,
		"--email", "client@example.com", "--password", testPassword)
	if err == nil {
		t.Error("set-password on a client account succeeded — a password Login will never " +
			"accept is a lie the operator discovers only by trying it")
	}
}

func TestSetPasswordRejectsShortPassword(t *testing.T) {
	args, cfg := bootstrapArgs(t, "--password", testPassword)
	if _, err := runCLI(t, "", args...); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	_, err := runCLI(t, "",
		"router", "admin", "set-password",
		"--db", cfg.DBPath, "--blobs", cfg.BlobDir,
		"--email", "admin@example.com", "--password", "short")
	if !errors.Is(err, identity.ErrWeakPassword) {
		t.Fatalf("set-password with a short password = %v, want ErrWeakPassword", err)
	}

	// And the original password still works — a partial write here would be a
	// password the operator believes they set.
	app, berr := buildApp(cfg)
	if berr != nil {
		t.Fatalf("buildApp: %v", berr)
	}
	defer func() { _ = app.Close() }()
	if _, err := app.Ident.Login(context.Background(),
		"admin@example.com", testPassword, time.Now()); err != nil {
		t.Errorf("a rejected short password damaged the existing credential: %v", err)
	}
}

func TestPromptRefusesMismatchedConfirmation(t *testing.T) {
	args, cfg := bootstrapArgs(t)
	_ = cfg
	// Two different lines at the prompt.
	_, err := runCLI(t, "first passphrase here\nsecond passphrase here\n", args...)
	if err == nil {
		t.Error("a mismatched confirmation was accepted — a mistyped invisible password " +
			"silently becomes the real one, and the operator finds out by being locked out")
	}
}

func TestPromptAcceptsMatchingConfirmation(t *testing.T) {
	args, cfg := bootstrapArgs(t)
	if _, err := runCLI(t, testPassword+"\n"+testPassword+"\n", args...); err != nil {
		t.Fatalf("a matching confirmation was refused: %v — which would make the mismatch test "+
			"above pass against a prompt that refuses everything", err)
	}

	app, err := buildApp(cfg)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer func() { _ = app.Close() }()
	if _, err := app.Ident.Login(context.Background(),
		"admin@example.com", testPassword, time.Now()); err != nil {
		t.Errorf("the prompted password does not work: %v", err)
	}
}

// TestPasswordIsNotEchoedToStdout — a bootstrap that prints the password puts it
// in the operator's scrollback and in any CI log that captured the run.
func TestPasswordIsNotEchoedToStdout(t *testing.T) {
	args, _ := bootstrapArgs(t, "--password", testPassword)
	out, err := runCLI(t, "", args...)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if out == "" {
		t.Fatal("the command printed nothing, so this assertion proved nothing")
	}
	if strings.Contains(out, testPassword) {
		t.Errorf("the password appears in the command's output:\n%s", out)
	}
	if strings.Contains(out, "$argon2id$") {
		t.Errorf("the password HASH appears in the command's output:\n%s", out)
	}
}

// TestBinaryAcceptsALoginEndToEnd is red if wire.go never hands the session
// store to identity — which every test in T1–T3 survives.
func TestBinaryAcceptsALoginEndToEnd(t *testing.T) {
	args, cfg := bootstrapArgs(t, "--password", testPassword)
	if _, err := runCLI(t, "", args...); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	app, err := buildApp(cfg)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer func() { _ = app.Close() }()

	// TLS, because the login handler refuses plain HTTP by design.
	srv := httptest.NewTLSServer(app.Handler)
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	// 1. Sign in.
	req, _ := http.NewRequest("POST", srv.URL+"/admin/login",
		strings.NewReader(url.Values{
			"email": {"admin@example.com"}, "password": {testPassword},
		}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/login: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login through the binary = %d, want 303: %s. The session store is built in "+
			"buildApp and never reaches identity, so the login page authenticates nobody",
			resp.StatusCode, body)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == web.SessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie was set")
	}

	// 2. The dashboard renders.
	req, _ = http.NewRequest("GET", srv.URL+"/admin", nil)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /admin: %v", err)
	}
	dash, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin with the session = %d, want 200: %s", resp.StatusCode, dash)
	}
	if !strings.Contains(string(dash), "ocr-router") {
		t.Error("the dashboard did not render for a session-authenticated request")
	}

	// 3. Sign out, and the same cookie stops working.
	req, _ = http.NewRequest("POST", srv.URL+"/admin/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set("Origin", srv.URL)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/logout: %v", err)
	}
	_ = resp.Body.Close()

	req, _ = http.NewRequest("GET", srv.URL+"/admin", nil)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /admin after logout: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the session still works after logout = %d, want 401", resp.StatusCode)
	}
}

// TestDashboardRequestsAreLogged closes a gap found by driving the real binary.
//
// ⚠ EVERY TEST IN T1–T3 PASSED WITH THE DASHBOARD PRODUCING NO LOG LINES AT ALL.
// `logRequests` is applied inside httpapi's own router, and the composition root
// mounts /admin on the PARENT mux, so dashboard requests never reached it. A
// manual run on 2026-09-15 — sign in, load the dashboard, get a CSRF refusal,
// sign out — produced two request lines, both for API routes.
//
// It matters most for the FAILED login: ADR-0002 put the logger outermost
// precisely so it would capture 401s, and the most security-relevant
// unauthenticated request in the system was the one going unrecorded.
func TestDashboardRequestsAreLogged(t *testing.T) {
	args, cfg := bootstrapArgs(t, "--password", testPassword)
	if _, err := runCLI(t, "", args...); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	app, srv, buf := startCaptured(t, cfg)
	_ = app

	// A FAILED login: unauthenticated, and the line that matters most.
	resp, err := http.PostForm(srv.URL+"/admin/login", url.Values{
		"email": {"admin@example.com"}, "password": {"wrong"},
	})
	if err != nil {
		t.Fatalf("POST /admin/login: %v", err)
	}
	_ = resp.Body.Close()

	// And a plain GET of the login page.
	resp, err = http.Get(srv.URL + "/admin/login")
	if err != nil {
		t.Fatalf("GET /admin/login: %v", err)
	}
	_ = resp.Body.Close()
	time.Sleep(150 * time.Millisecond)

	out := buf.String()
	if !strings.Contains(out, `"route":"/admin/login"`) {
		t.Errorf("no request line for /admin/login. The dashboard subtree is mounted outside "+
			"httpapi's router, so it does not inherit the request logger and every sign-in "+
			"attempt — including every failed one — goes unrecorded:\n%s", out)
	}
}

// TestSessionSweepRunsOnTheReaperTick asserts the table SHRANK.
//
// ⚠ Asserting that SweepExpired was called proves someone wrote a line of code
// about the leak, not that the leak is gone — and no test in internal/session
// can see whether the composition root calls it at all.
func TestSessionSweepRunsOnTheReaperTick(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReapInterval = 20 * time.Millisecond

	app, err := buildApp(cfg)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer func() { _ = app.Close() }()
	ctx := context.Background()

	admin, _, err := app.Ident.Bootstrap(ctx, "admin@example.com", time.Now())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// An already-expired session.
	if _, err := app.Sessions.Create(ctx, admin.ID,
		time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("session.Create: %v", err)
	}
	if n, _ := app.Sessions.Len(ctx); n != 1 {
		t.Fatalf("the sessions table holds %d rows, want 1 — there is nothing to sweep and this "+
			"test proves nothing", n)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	app.StartReaper(runCtx, cfg.ReapInterval)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if n, _ := app.Sessions.Len(ctx); n == 0 {
			return // swept
		}
		if time.Now().After(deadline) {
			n, _ := app.Sessions.Len(ctx)
			t.Fatalf("the sessions table still holds %d expired rows after the reaper ran. "+
				"SweepExpired is not wired to the tick, so the table grows one row per login "+
				"forever", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCLIExposesThePasswordFlags(t *testing.T) {
	// Rung 3: a subcommand that exists in the code but not on the command tree
	// is unreachable by an operator.
	cmd := newCLI()
	var adminCmd *cli.Command
	for _, c := range cmd.Commands {
		if c.Name == "admin" {
			adminCmd = c
		}
	}
	if adminCmd == nil {
		t.Fatal("there is no admin command at all")
	}

	found := map[string]*cli.Command{}
	for _, c := range adminCmd.Commands {
		found[c.Name] = c
	}
	for _, want := range []string{"bootstrap", "set-password"} {
		c, ok := found[want]
		if !ok {
			t.Errorf("admin %s is not on the command line", want)
			continue
		}
		var hasPassword bool
		for _, f := range c.Flags {
			for _, n := range f.Names() {
				if n == "password" {
					hasPassword = true
				}
			}
		}
		if !hasPassword {
			t.Errorf("admin %s does not expose --password", want)
		}
	}
}

// TestDeferredItemsReachedTheBacklog — a deferral is not filed by pointing at a
// file. The entry has to exist at the destination.
func TestDeferredItemsReachedTheBacklog(t *testing.T) {
	b, err := os.ReadFile("../../docs/adr/BACKLOG.md")
	if err != nil {
		t.Fatalf("reading BACKLOG.md: %v", err)
	}
	backlog := string(b)

	for _, want := range []string{
		"Self-service password change",
		"Two-factor authentication",
		"Session listing",
		"requested URL across a login redirect",
	} {
		if !strings.Contains(backlog, want) {
			t.Errorf("ADR-0003 defers %q and BACKLOG.md does not mention it — the pointer names "+
				"a file that never received the entry, which passes every check there is", want)
		}
	}
}
