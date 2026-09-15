package identity

import (
	"context"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// SessionTTL is how long a browser login lasts.
//
// ⚠ ABSOLUTE, and deliberately not a sliding window. Twelve hours is one working
// day; a session refreshed on every request ends for nobody who leaves a tab
// open, which is how a browser on an unlocked laptop becomes a permanent
// administrative credential. The convenience of never logging in again is small
// next to that, for this credential.
const SessionTTL = 12 * time.Hour

// Sessions is the slice of the session store this package needs.
//
// An interface at the CONSUMER so identity does not import the session package —
// session already imports identity for HashToken, and the other direction would
// be a cycle. It also means a Service built without sessions still works, which
// keeps every test written before ADR-0003 unchanged.
type Sessions interface {
	Create(ctx context.Context, userID string, now, expiresAt time.Time) (string, error)
	Resolve(ctx context.Context, secret string, now time.Time) (core.Session, error)
	Revoke(ctx context.Context, secret string) error
}

// SetSessions attaches a session store.
func (s *Service) SetSessions(store Sessions) { s.sessions = store }

// Login verifies an administrator's password and issues a session.
//
// ⚠ EVERY FAILURE RETURNS core.ErrUnauthorized, and the list of causes is longer
// than it looks: unknown email, an account with no password set, the wrong
// password, a deactivated user, and a non-admin role. Distinguishing any of them
// turns this endpoint into an oracle — a caller could learn which emails are
// registered, which accounts are disabled, or which are administrators. This
// extends the rule Authenticate already follows for tokens.
//
// The timing has to collapse too, not only the error value. That is why
// VerifyPassword is called even when there is no hash to verify against: see its
// doc comment.
func (s *Service) Login(ctx context.Context, email, password string, now time.Time) (string, error) {
	if s.sessions == nil {
		// A Service with no session store cannot issue one. Failing closed rather
		// than panicking: the dashboard would be unreachable, which is visible,
		// where a panic would take the process down.
		return "", core.ErrUnauthorized
	}

	// Normalising is THIS function's job. The repository matches the stored form
	// exactly, and a login form does not normalise — nor does a human typing
	// their own address. Doing it in both places would mean two implementations
	// that drift.
	norm, err := normaliseEmail(email)
	if err != nil {
		// Still pay the verification cost: an unparseable email must not answer
		// faster than a real one.
		_ = VerifyPassword("", password)
		return "", core.ErrUnauthorized
	}

	userID, hash, err := s.repo.PasswordHashByEmail(ctx, norm)
	if err != nil {
		_ = VerifyPassword("", password)
		return "", core.ErrUnauthorized
	}

	// Runs the KDF whether or not `hash` is usable — an account with no password
	// must not answer faster than one with a wrong password.
	if !VerifyPassword(hash, password) {
		return "", core.ErrUnauthorized
	}

	u, err := s.repo.UserByID(ctx, userID)
	if err != nil || !u.Active || u.Role != core.RoleAdmin {
		// ⚠ Only an admin may hold a session. A client or worker row with a
		// password written directly into the database still cannot log in.
		return "", core.ErrUnauthorized
	}

	return s.sessions.Create(ctx, u.ID, now, now.Add(SessionTTL))
}

// ResolveSession turns a session secret into a principal.
//
// ⚠ IT RE-READS THE USER ON EVERY CALL. Caching the role and active flag on the
// session row is faster, obvious, and means a deactivated or demoted
// administrator keeps working until their session expires — up to twelve hours
// after someone thought they had revoked access.
//
// The returned Principal carries the SESSION id in TokenID. That is deliberate:
// it is what the request log and the rate limiter key on, and a session and a
// token are interchangeable there — both identify one credential, not one user.
func (s *Service) ResolveSession(ctx context.Context, secret string, now time.Time) (core.Principal, error) {
	if s.sessions == nil {
		return core.Principal{}, core.ErrUnauthorized
	}

	sess, err := s.sessions.Resolve(ctx, secret, now)
	if err != nil {
		return core.Principal{}, core.ErrUnauthorized
	}

	u, err := s.repo.UserByID(ctx, sess.UserID)
	if err != nil || !u.Active || u.Role != core.RoleAdmin {
		return core.Principal{}, core.ErrUnauthorized
	}

	return core.Principal{UserID: u.ID, Role: u.Role, TokenID: sess.ID}, nil
}

// Logout revokes a session server-side.
func (s *Service) Logout(ctx context.Context, secret string) error {
	if s.sessions == nil {
		return nil
	}
	return s.sessions.Revoke(ctx, secret)
}

// SetPassword stores a new password for an administrator.
//
// It refuses a non-admin account rather than writing a credential that could
// never be used: T2's Login only admits admins, so a password on a client row is
// a lie the operator would discover only by trying it.
func (s *Service) SetPassword(ctx context.Context, email, password string, now time.Time) error {
	norm, err := normaliseEmail(email)
	if err != nil {
		return err
	}
	u, err := s.repo.UserByEmail(ctx, norm)
	if err != nil {
		return err
	}
	if u.Role != core.RoleAdmin {
		return core.ErrForbidden
	}

	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.repo.SetPasswordHash(ctx, u.ID, hash)
}
