package identity_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func newService(t *testing.T) *identity.Service {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "id.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return identity.New(store.NewRepo(db))
}

// admin bootstraps the first administrator, the way cmd/router does.
func admin(t *testing.T, s *identity.Service) (core.User, string) {
	t.Helper()
	u, tok, err := s.Bootstrap(context.Background(), "admin@example.com", base)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return u, tok
}

func principalOf(t *testing.T, s *identity.Service, token string) core.Principal {
	t.Helper()
	p, err := s.Authenticate(context.Background(), "Bearer "+token, base)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	return p
}

func TestAuthenticateUnknownToken(t *testing.T) {
	s := newService(t)
	_, err := s.Authenticate(context.Background(), "Bearer ocr_c_nosuchtoken", base)
	if !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("unknown token = %v, want core.ErrUnauthorized", err)
	}
}

func TestAuthenticateRevokedToken(t *testing.T) {
	s := newService(t)
	a, adminTok := admin(t, s)
	ap := principalOf(t, s, adminTok)

	u, err := s.CreateUser(context.Background(), ap, "c@example.com", core.RoleClient, base)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tokenID, plaintext, err := s.MintToken(context.Background(), ap, u.ID, core.RoleClient, "l", base)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if _, err := s.Authenticate(context.Background(), "Bearer "+plaintext, base); err != nil {
		t.Fatalf("token should work before revocation: %v", err)
	}

	if err := s.RevokeToken(context.Background(), ap, tokenID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	// ⚠ The SAME error as an unknown token. Three separate causes, one
	// indistinguishable response, so the endpoint cannot be used to learn which
	// tokens once existed.
	if _, err := s.Authenticate(context.Background(), "Bearer "+plaintext, base); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("revoked token = %v, want core.ErrUnauthorized (identical to unknown)", err)
	}
	_ = a
}

func TestAuthenticateInactiveUser(t *testing.T) {
	s := newService(t)
	_, adminTok := admin(t, s)
	ap := principalOf(t, s, adminTok)
	ctx := context.Background()

	u, _ := s.CreateUser(ctx, ap, "c@example.com", core.RoleClient, base)
	_, plaintext, _ := s.MintToken(ctx, ap, u.ID, core.RoleClient, "l", base)

	if err := s.SetActive(ctx, ap, u.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if _, err := s.Authenticate(ctx, "Bearer "+plaintext, base); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("token on a deactivated user = %v, want core.ErrUnauthorized (identical to "+
			"unknown and revoked)", err)
	}
}

func TestAuthenticateMalformedHeader(t *testing.T) {
	s := newService(t)
	cases := []string{
		"",
		"Bearer",
		"Bearer ",
		"bearer",
		"Basic abc",
		"Token abc",
		"abc",
		"Bearer  ",
	}
	for _, h := range cases {
		if _, err := s.Authenticate(context.Background(), h, base); !errors.Is(err, core.ErrUnauthorized) {
			t.Errorf("Authenticate(%q) = %v, want core.ErrUnauthorized", h, err)
		}
	}
}

// TestAuthenticateReturnsRoleFromRow is the one that matters for authorisation.
//
// The token string carries a human-readable prefix so an operator can judge a
// leaked token's blast radius at a glance. That prefix is a LABEL. If the role
// were ever parsed from it, anyone could mint themselves an admin by typing one.
func TestAuthenticateReturnsRoleFromRow(t *testing.T) {
	s := newService(t)
	_, adminTok := admin(t, s)
	ap := principalOf(t, s, adminTok)
	ctx := context.Background()

	u, _ := s.CreateUser(ctx, ap, "c@example.com", core.RoleClient, base)
	// Mint a CLIENT token, then check what the prefix says versus what the row
	// says. The mint gives a client prefix; we authenticate and expect client
	// regardless of any prefix reading.
	_, plaintext, _ := s.MintToken(ctx, ap, u.ID, core.RoleClient, "l", base)

	p, err := s.Authenticate(ctx, "Bearer "+plaintext, base)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Role != core.RoleClient {
		t.Errorf("role = %q, want client", p.Role)
	}
	if p.UserID != u.ID {
		t.Errorf("user = %q, want %q", p.UserID, u.ID)
	}

	// Now the adversarial half: a token whose TEXT claims admin but whose row
	// says client must authenticate as a client.
	forged := "ocr_a_" + strings.TrimPrefix(plaintext, "ocr_c_")
	if _, err := s.Authenticate(ctx, "Bearer "+forged, base); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("a token with an admin-looking prefix but no matching row = %v, want "+
			"core.ErrUnauthorized — the prefix must carry no authority", err)
	}
}

// TestTokenRoleIsIndependentOfUserRole separates the two roles that every other
// fixture in this file conflates.
//
// ⚠ Added after a SURVIVED mutant. Changing `Role: tok.Role` to `Role: u.Role`
// in Authenticate passed the entire suite, because every other test mints a
// token whose role already equals its user's role — so the two expressions are
// indistinguishable in all of them. The fixture could not produce the failure,
// which is not the same as the code being right.
//
// The case is real and is the reason the distinction exists: an administrator
// who keeps a client-scoped token for day-to-day use would, under the mutant,
// have that token silently act as an administrator.
func TestTokenRoleIsIndependentOfUserRole(t *testing.T) {
	s := newService(t)
	_, adminTok := admin(t, s)
	ap := principalOf(t, s, adminTok)
	ctx := context.Background()

	// An ADMIN user...
	second, err := s.CreateUser(ctx, ap, "admin2@example.com", core.RoleAdmin, base)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// ...holding a CLIENT-scoped token.
	_, clientScoped, err := s.MintToken(ctx, ap, second.ID, core.RoleClient, "daily-driver", base)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}

	p, err := s.Authenticate(ctx, "Bearer "+clientScoped, base)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Role != core.RoleClient {
		t.Errorf("principal role = %q, want client — the role must come from the TOKEN row, "+
			"not from the user's own role, or every token widens to its owner's privileges",
			p.Role)
	}
	if p.IsAdmin() {
		t.Error("a client-scoped token on an admin account reported IsAdmin")
	}
	// And the authorisation consequence, not just the field.
	if _, err := s.CreateUser(ctx, p, "x@example.com", core.RoleClient, base); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("a client-scoped token on an admin account created a user (%v) — this is the "+
			"privilege escalation the token/user role split exists to prevent", err)
	}
}

func TestCreateUserRequiresAdmin(t *testing.T) {
	s := newService(t)
	_, adminTok := admin(t, s)
	ap := principalOf(t, s, adminTok)
	ctx := context.Background()

	client, _ := s.CreateUser(ctx, ap, "c@example.com", core.RoleClient, base)
	_, clientTok, _ := s.MintToken(ctx, ap, client.ID, core.RoleClient, "l", base)
	cp := principalOf(t, s, clientTok)

	worker, _ := s.CreateUser(ctx, ap, "w@example.com", core.RoleWorker, base)
	_, workerTok, _ := s.MintToken(ctx, ap, worker.ID, core.RoleWorker, "l", base)
	wp := principalOf(t, s, workerTok)

	for name, p := range map[string]core.Principal{"client": cp, "worker": wp} {
		if _, err := s.CreateUser(ctx, p, "x@example.com", core.RoleClient, base); !errors.Is(err, core.ErrForbidden) {
			t.Errorf("%s creating a user = %v, want core.ErrForbidden", name, err)
		}
		if _, _, err := s.MintToken(ctx, p, client.ID, core.RoleClient, "l", base); !errors.Is(err, core.ErrForbidden) {
			t.Errorf("%s minting a token = %v, want core.ErrForbidden", name, err)
		}
	}
}

func TestCreateUserDuplicateEmail(t *testing.T) {
	s := newService(t)
	_, adminTok := admin(t, s)
	ap := principalOf(t, s, adminTok)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, ap, "dup@example.com", core.RoleClient, base); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	if _, err := s.CreateUser(ctx, ap, "dup@example.com", core.RoleClient, base); !errors.Is(err, core.ErrConflict) {
		t.Errorf("duplicate email = %v, want core.ErrConflict", err)
	}
}

func TestCreateUserNormalisesEmail(t *testing.T) {
	s := newService(t)
	_, adminTok := admin(t, s)
	ap := principalOf(t, s, adminTok)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, ap, "  Foo@Example.COM  ", core.RoleClient, base); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Without normalisation these are two accounts, two balances and two buffer
	// limits for one person — and the operator would have no way to tell.
	if _, err := s.CreateUser(ctx, ap, "foo@example.com", core.RoleClient, base); !errors.Is(err, core.ErrConflict) {
		t.Errorf("differently-cased duplicate = %v, want core.ErrConflict", err)
	}
}

func TestCreateUserRejectsBadEmail(t *testing.T) {
	s := newService(t)
	_, adminTok := admin(t, s)
	ap := principalOf(t, s, adminTok)
	for _, e := range []string{"", "   ", "no-at-sign", "@example.com", "a@"} {
		if _, err := s.CreateUser(context.Background(), ap, e, core.RoleClient, base); err == nil {
			t.Errorf("CreateUser(%q) succeeded, want an error", e)
		}
	}
}

func TestCreateUserRejectsBadRole(t *testing.T) {
	s := newService(t)
	_, adminTok := admin(t, s)
	ap := principalOf(t, s, adminTok)
	if _, err := s.CreateUser(context.Background(), ap, "x@example.com", core.Role("root"), base); err == nil {
		t.Error("CreateUser with an unknown role succeeded, want an error")
	}
}

func TestBootstrapCreatesOneAdmin(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	u, tok, err := s.Bootstrap(ctx, "first@example.com", base)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if u.Role != core.RoleAdmin {
		t.Errorf("bootstrapped role = %q, want admin", u.Role)
	}
	if tok == "" {
		t.Fatal("Bootstrap returned an empty token — it is the only way into a fresh system")
	}
	// The printed token must actually work; printing one that does not is worse
	// than printing none, because the operator believes they have access.
	p, err := s.Authenticate(ctx, "Bearer "+tok, base)
	if err != nil {
		t.Fatalf("the bootstrap token does not authenticate: %v", err)
	}
	if p.Role != core.RoleAdmin {
		t.Errorf("bootstrap principal role = %q, want admin", p.Role)
	}

	// A second bootstrap on a system that already has an admin is refused: it
	// would otherwise be an unauthenticated way to mint administrators.
	if _, _, err := s.Bootstrap(ctx, "second@example.com", base); !errors.Is(err, core.ErrConflict) {
		t.Errorf("second Bootstrap = %v, want core.ErrConflict", err)
	}
}
