package web_test

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// TestTokenListIsReachableFromTheDashboard is the rung-2 check that was missing.
//
// ⚠ `identity.ListTokens` and its repository query were written and tested in
// ADR-0001 T3, and NOTHING CALLED EITHER. The dashboard could mint credentials
// and never show which existed — so an operator could not tell a live
// integration from a token minted for a laptop months ago, and could not revoke
// one without opening the database. A unit test on the service proves the method
// works; only this proves it is reachable.
func TestTokenListIsReachableFromTheDashboard(t *testing.T) {
	e := newEnv(t)

	// Mint two tokens so the list has something to show beyond the seed.
	for range 2 {
		resp := e.do(t, "POST", "/admin/users/"+e.clientID+"/tokens", e.adminTok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("mint = %d", resp.StatusCode)
		}
	}

	resp := e.do(t, "GET", "/admin/users/"+e.clientID+"/tokens", e.adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET tokens = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	if !strings.Contains(out, "Tokens for") {
		t.Fatalf("the token list did not render:\n%s", out)
	}
	for _, want := range []string{"Last used", "Revoke", "c@example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("the token list is missing %q:\n%s", want, out)
		}
	}
}

// TestTokenListNeverShowsASecret — the router keeps only a SHA-256, and the
// plaintext is returned exactly once at minting.
//
// The hash is not a secret, but putting it on screen invites someone to treat it
// as one, and it is useless to an operator: it identifies nothing they can act
// on. The id, label and last-used time are what a revoke decision rests on.
func TestTokenListNeverShowsASecret(t *testing.T) {
	e := newEnv(t)

	resp := e.do(t, "POST", "/admin/users/"+e.clientID+"/tokens", e.adminTok, nil)
	minted, _ := io.ReadAll(resp.Body)
	// The mint response is an SSE frame carrying an HTML fragment, so the token
	// is not a whitespace-delimited field — a regex over the token's own shape is
	// the reliable way to find it.
	secret := regexp.MustCompile(`ocr_[acw]_[A-Za-z0-9_-]+`).FindString(string(minted))
	if secret == "" {
		t.Fatalf("could not recover the minted token from the response, so this test cannot "+
			"check that the list omits it:\n%s", minted)
	}

	list := e.do(t, "GET", "/admin/users/"+e.clientID+"/tokens", e.adminTok, nil)
	body, _ := io.ReadAll(list.Body)

	if strings.Contains(string(body), secret) {
		t.Error("the token list renders the token's PLAINTEXT — which the router is not even " +
			"supposed to be able to produce")
	}
	// And the stored hash is not paraded either.
	toks, err := e.repo.ListTokens(context.Background(), e.clientID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	for _, tok := range toks {
		if strings.Contains(string(body), tok.Hash) {
			t.Errorf("the token list renders the stored hash for %s", tok.ID)
		}
	}
}

// TestRevokeFromTheDashboardActuallyRevokes is the rung-2 check for
// identity.RevokeToken, which also had no caller.
func TestRevokeFromTheDashboardActuallyRevokes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.do(t, "POST", "/admin/users/"+e.clientID+"/tokens", e.adminTok, nil)
	toks, err := e.repo.ListTokens(ctx, e.clientID)
	if err != nil || len(toks) == 0 {
		t.Fatalf("no tokens to revoke (err=%v)", err)
	}
	target := toks[0]
	if target.Revoked {
		t.Fatal("the fixture's token is already revoked, so this proves nothing")
	}

	resp := e.do(t, "POST", "/admin/tokens/"+target.ID+"/revoke", e.adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d, want 200", resp.StatusCode)
	}

	// ⚠ Asserted in the DATABASE, not from the response. A handler that rendered
	// "revoked" without writing anything would pass a response-only check.
	after, err := e.repo.TokenByID(ctx, target.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if !after.Revoked {
		t.Error("the dashboard reported a revoke and the row is still live")
	}
}

// TestRevokedTokenStopsAuthenticating closes the loop: revoking in the UI has to
// actually end the credential, not merely relabel a row.
func TestRevokedTokenStopsAuthenticating(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// The seeded client token works.
	if got := e.do(t, "GET", "/services", e.clientTok, nil).StatusCode; got != http.StatusOK {
		t.Fatalf("the client token does not work to begin with: %d", got)
	}

	toks, err := e.repo.ListTokens(ctx, e.clientID)
	if err != nil || len(toks) == 0 {
		t.Fatalf("no tokens (err=%v)", err)
	}

	for _, tok := range toks {
		resp := e.do(t, "POST", "/admin/tokens/"+tok.ID+"/revoke", e.adminTok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("revoke = %d", resp.StatusCode)
		}
	}

	if got := e.do(t, "GET", "/services", e.clientTok, nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("the revoked token still authenticates (%d) — the UI relabelled a row and the "+
			"credential is live", got)
	}
}

// TestRevokeRerendersTheList — the operator sees the row flip in place, which is
// both the confirmation and the new state.
func TestRevokeRerendersTheList(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	e.do(t, "POST", "/admin/users/"+e.clientID+"/tokens", e.adminTok, nil)
	toks, _ := e.repo.ListTokens(ctx, e.clientID)
	if len(toks) == 0 {
		t.Fatal("no tokens")
	}

	resp := e.do(t, "POST", "/admin/tokens/"+toks[0].ID+"/revoke", e.adminTok, nil)
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	if !strings.Contains(out, "Tokens for") {
		t.Errorf("revoking did not re-render the list, so the operator gets no confirmation "+
			"and no new state:\n%s", out)
	}
	if !strings.Contains(out, "revoked") {
		t.Errorf("the re-rendered list does not show the revoked state:\n%s", out)
	}
}

// TestNeverUsedTokenReadsAsNever — the single most useful fact on the table.
//
// A zero last_seen_at rendered as a 1970 date would bury the one signal that
// says "nobody ever wired this credential up".
func TestNeverUsedTokenReadsAsNever(t *testing.T) {
	e := newEnv(t)

	e.do(t, "POST", "/admin/users/"+e.clientID+"/tokens", e.adminTok, nil)
	resp := e.do(t, "GET", "/admin/users/"+e.clientID+"/tokens", e.adminTok, nil)
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	if !strings.Contains(out, "never") {
		t.Errorf("a freshly minted, never-used token does not read as \"never\":\n%s", out)
	}
	if strings.Contains(out, "1970") {
		t.Error("an unused token's last-seen renders as a 1970 date")
	}
}

// TestTokenListRequiresAdmin — the routes are inside the admin group, and this
// is the assertion that keeps them there.
func TestTokenListRequiresAdmin(t *testing.T) {
	e := newEnv(t)

	for _, r := range []struct{ method, path string }{
		{"GET", "/admin/users/" + e.clientID + "/tokens"},
		{"POST", "/admin/tokens/whatever/revoke"},
	} {
		// A client token is authenticated but not an administrator.
		if got := e.do(t, r.method, r.path, e.clientTok, nil).StatusCode; got != http.StatusForbidden {
			t.Errorf("%s %s as a client = %d, want 403 — a customer could otherwise read and "+
				"revoke credentials", r.method, r.path, got)
		}
		// And with no credential at all.
		if got := e.do(t, r.method, r.path, "", nil).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("%s %s with no token = %d, want 401", r.method, r.path, got)
		}
	}
}

func TestRevokeUnknownTokenIsHandled(t *testing.T) {
	e := newEnv(t)

	resp := e.do(t, "POST", "/admin/tokens/"+core.NewID()+"/revoke", e.adminTok, nil)
	// A 200 carrying an error fragment is this dashboard's convention; what must
	// not happen is a 500 or a panic.
	if resp.StatusCode >= 500 {
		t.Errorf("revoking an unknown token = %d, want a handled response", resp.StatusCode)
	}
}

func TestRelativeTimeRendersRecentUse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Using the token stamps last_seen_at.
	e.do(t, "GET", "/services", e.clientTok, nil)

	toks, _ := e.repo.ListTokens(ctx, e.clientID)
	var seen bool
	for _, tok := range toks {
		if !tok.LastSeenAt.IsZero() {
			seen = true
		}
	}
	if !seen {
		t.Skip("no token recorded a last-seen time, so there is nothing to render")
	}

	resp := e.do(t, "GET", "/admin/users/"+e.clientID+"/tokens", e.adminTok, nil)
	body, _ := io.ReadAll(resp.Body)
	out := string(body)
	if strings.Contains(out, "never") && !strings.Contains(out, "ago") &&
		!strings.Contains(out, "just now") {
		t.Errorf("a used token still reads as never:\n%s", out)
	}
	_ = time.Now
}
