package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/session"
)

// withSessions attaches a real session store to the harness's service.
//
// A real store rather than a fake: the properties under test here — that a
// session dies when its user is deactivated, that expiry is absolute — are
// properties of the two working together, and a fake would assert only that the
// calls were made.
func (h *identityHarness) withSessions(t *testing.T) *session.Store {
	t.Helper()
	st := session.New(h.db)
	h.svc.SetSessions(st)
	return st
}

// adminWithPassword bootstraps an admin and gives it a password.
func (h *identityHarness) adminWithPassword(t *testing.T, password string) core.User {
	t.Helper()
	u := h.bootstrapAdmin(t)
	if err := h.svc.SetPassword(context.Background(), u.Email, password, base); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	return u
}

func TestLoginSucceedsForAdmin(t *testing.T) {
	h := newIdentityHarness(t)
	h.withSessions(t)
	u := h.adminWithPassword(t, goodPassword)

	secret, err := h.svc.Login(context.Background(), u.Email, goodPassword, base)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if secret == "" {
		t.Fatal("Login returned an empty secret")
	}

	p, err := h.svc.ResolveSession(context.Background(), secret, base)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if p.UserID != u.ID {
		t.Errorf("principal user = %q, want %q", p.UserID, u.ID)
	}
	if p.Role != core.RoleAdmin {
		t.Errorf("principal role = %q, want admin", p.Role)
	}
	if p.TokenID == "" {
		t.Error("the principal carries no credential id — the request log and the rate limiter " +
			"both key on it")
	}
}

// TestLoginFailuresAreIndistinguishable is the oracle check.
//
// ⚠ Each row must be a GENUINELY different cause. It is easy to write five cases
// that are all "wrong password" wearing different names, which would pass while
// proving one thing.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		setup func(t *testing.T, h *identityHarness) (email, password string)
	}{
		{"unknown email", func(t *testing.T, h *identityHarness) (string, string) {
			h.adminWithPassword(t, goodPassword)
			return "nobody@example.com", goodPassword
		}},
		{"account with no password set", func(t *testing.T, h *identityHarness) (string, string) {
			u := h.bootstrapAdmin(t) // deliberately no SetPassword
			return u.Email, goodPassword
		}},
		{"wrong password", func(t *testing.T, h *identityHarness) (string, string) {
			u := h.adminWithPassword(t, goodPassword)
			return u.Email, "definitely not the password"
		}},
		{"deactivated user", func(t *testing.T, h *identityHarness) (string, string) {
			u := h.adminWithPassword(t, goodPassword)
			p, err := h.svc.Authenticate(ctx, "Bearer "+h.adminToken, base)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if err := h.svc.SetActive(ctx, p, u.ID, false); err != nil {
				t.Fatalf("SetActive: %v", err)
			}
			return u.Email, goodPassword
		}},
		{"non-admin role", func(t *testing.T, h *identityHarness) (string, string) {
			// A client row with a valid password written straight into the
			// database — the shape SetPassword refuses to create, reached the
			// only way it can be.
			hash, err := identity.HashPassword(goodPassword)
			if err != nil {
				t.Fatalf("HashPassword: %v", err)
			}
			id := core.NewID()
			if _, err := h.db.Write.Exec(
				`INSERT INTO users (id,email,role,active,created_at,password_hash)
				 VALUES (?,?,?,1,?,?)`,
				id, "client@example.com", string(core.RoleClient), base.Unix(), hash); err != nil {
				t.Fatalf("seeding client: %v", err)
			}
			return "client@example.com", goodPassword
		}},
		{"malformed email", func(t *testing.T, h *identityHarness) (string, string) {
			h.adminWithPassword(t, goodPassword)
			return "not-an-email", goodPassword
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newIdentityHarness(t)
			h.withSessions(t)
			email, password := tc.setup(t, h)

			secret, err := h.svc.Login(ctx, email, password, base)
			if !errors.Is(err, core.ErrUnauthorized) {
				t.Errorf("err = %v, want exactly core.ErrUnauthorized. Distinguishing this cause "+
					"from the others turns login into an oracle: a caller could learn which "+
					"emails are registered, which accounts are disabled, and which are admins",
					err)
			}
			if secret != "" {
				t.Errorf("a failed login returned a session secret %q", secret)
			}
		})
	}
}

// TestLoginSucceedsWhereTheFailureCasesDiffer is the positive companion.
//
// Without it, every case above is satisfied by a Login that refuses everything.
func TestLoginSucceedsWhereTheFailureCasesDiffer(t *testing.T) {
	h := newIdentityHarness(t)
	h.withSessions(t)
	u := h.adminWithPassword(t, goodPassword)

	if _, err := h.svc.Login(context.Background(), u.Email, goodPassword, base); err != nil {
		t.Fatalf("the happy path fails, so the six negative cases prove nothing: %v", err)
	}
}

func TestLoginNormalisesEmail(t *testing.T) {
	h := newIdentityHarness(t)
	h.withSessions(t)
	h.adminWithPassword(t, goodPassword)

	// A login form does not normalise, and a human does not type carefully.
	if _, err := h.svc.Login(context.Background(), "  ADMIN@Example.COM  ", goodPassword, base); err != nil {
		t.Errorf("an unnormalised email was refused: %v", err)
	}
}

func TestSessionExpiryIsTwelveHours(t *testing.T) {
	h := newIdentityHarness(t)
	store := h.withSessions(t)
	u := h.adminWithPassword(t, goodPassword)

	secret, err := h.svc.Login(context.Background(), u.Email, goodPassword, base)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	sess, err := store.Resolve(context.Background(), secret, base)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// The number is a decision (ADR-0003 §Decision 2), so it is asserted rather
	// than left to a constant nobody checks.
	want := base.Add(12 * time.Hour)
	if !sess.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", sess.ExpiresAt, want)
	}
	// And it really does die there.
	if _, err := h.svc.ResolveSession(context.Background(), secret, want); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("the session survived its twelve-hour deadline (err=%v)", err)
	}
}

// TestSessionDiesWhenUserDeactivated is the test that catches the caching bug.
//
// ⚠ The caching implementation — storing role and active state on the session
// row at login — is faster, obvious, and passes every other test in this file.
// It also means a deactivated administrator keeps working for up to twelve hours
// after someone thought they had revoked access. If anything here is cut, not
// this.
func TestSessionDiesWhenUserDeactivated(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.withSessions(t)
	u := h.adminWithPassword(t, goodPassword)

	secret, err := h.svc.Login(ctx, u.Email, goodPassword, base)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := h.svc.ResolveSession(ctx, secret, base); err != nil {
		t.Fatalf("the session did not resolve before deactivation: %v", err)
	}

	p, err := h.svc.Authenticate(ctx, "Bearer "+h.adminToken, base)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := h.svc.SetActive(ctx, p, u.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	// Immediately — not at expiry.
	if _, err := h.svc.ResolveSession(ctx, secret, base); !errors.Is(err, core.ErrUnauthorized) {
		t.Error("a deactivated administrator's session still resolves. The role and active flag " +
			"are cached on the session rather than re-read, so revoking access does nothing for " +
			"up to twelve hours")
	}
}

// TestSessionDiesWhenUserDemoted is the same caching bug with a different
// symptom, and the one a reviewer is less likely to think of.
func TestSessionDiesWhenUserDemoted(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.withSessions(t)
	u := h.adminWithPassword(t, goodPassword)

	secret, err := h.svc.Login(ctx, u.Email, goodPassword, base)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if _, err := h.db.Write.Exec(`UPDATE users SET role = ? WHERE id = ?`,
		string(core.RoleClient), u.ID); err != nil {
		t.Fatalf("demoting: %v", err)
	}

	if _, err := h.svc.ResolveSession(ctx, secret, base); !errors.Is(err, core.ErrUnauthorized) {
		t.Error("a demoted administrator's session still resolves as admin")
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.withSessions(t)
	u := h.adminWithPassword(t, goodPassword)

	secret, _ := h.svc.Login(ctx, u.Email, goodPassword, base)
	if err := h.svc.Logout(ctx, secret); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := h.svc.ResolveSession(ctx, secret, base); !errors.Is(err, core.ErrUnauthorized) {
		t.Error("the session survived logout")
	}
}

func TestLoginWithNoSessionStoreFailsClosed(t *testing.T) {
	// A Service constructed without SetSessions — the shape every test written
	// before ADR-0003 uses. It must refuse rather than panic: an unreachable
	// dashboard is visible, a panic takes the process down.
	h := newIdentityHarness(t)
	u := h.adminWithPassword(t, goodPassword)

	if _, err := h.svc.Login(context.Background(), u.Email, goodPassword, base); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("Login with no session store = %v, want core.ErrUnauthorized", err)
	}
	if _, err := h.svc.ResolveSession(context.Background(), "anything", base); !errors.Is(err, core.ErrUnauthorized) {
		t.Errorf("ResolveSession with no session store = %v, want core.ErrUnauthorized", err)
	}
}

func TestSetPasswordRefusesNonAdmin(t *testing.T) {
	h := newIdentityHarness(t)
	h.bootstrapAdmin(t)

	id := core.NewID()
	if _, err := h.db.Write.Exec(
		`INSERT INTO users (id,email,role,active,created_at) VALUES (?,?,?,1,?)`,
		id, "client@example.com", string(core.RoleClient), base.Unix()); err != nil {
		t.Fatalf("seeding client: %v", err)
	}

	err := h.svc.SetPassword(context.Background(), "client@example.com", goodPassword, base)
	if !errors.Is(err, core.ErrForbidden) {
		t.Errorf("SetPassword on a client = %v, want core.ErrForbidden. Writing a password that "+
			"Login will never accept is a lie the operator discovers only by trying it", err)
	}
}

func TestSetPasswordRejectsShortPassword(t *testing.T) {
	h := newIdentityHarness(t)
	u := h.bootstrapAdmin(t)

	if err := h.svc.SetPassword(context.Background(), u.Email, "short", base); !errors.Is(err, identity.ErrWeakPassword) {
		t.Errorf("SetPassword with a short password = %v, want ErrWeakPassword", err)
	}
	// And nothing was written — a partial state here would be a password the
	// operator believes they set.
	_, hash, err := h.repo.PasswordHashByEmail(context.Background(), u.Email)
	if err != nil {
		t.Fatalf("PasswordHashByEmail: %v", err)
	}
	if hash != "" {
		t.Errorf("a rejected password still wrote a hash %q", hash)
	}
}

func TestSetPasswordRefusesUnknownEmail(t *testing.T) {
	h := newIdentityHarness(t)
	h.bootstrapAdmin(t)

	if err := h.svc.SetPassword(context.Background(), "nobody@example.com", goodPassword, base); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("SetPassword on an unknown email = %v, want core.ErrNotFound", err)
	}
}
