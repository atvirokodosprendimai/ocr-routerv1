// Package identity creates users, mints bearer tokens, and turns an
// Authorization header into a principal.
//
// It is the only package that decides WHO a caller is. Everything downstream
// takes a core.Principal and asks only what that principal may do.
package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// defaultBufferLimit is how many jobs a new customer may have in flight.
//
// Four, from the operator: enough that a worker finishing one job always finds
// another queued from the same customer, so the pool never idles waiting for an
// upload, and few enough that one customer cannot monopolise it.
const defaultBufferLimit = 4

// Service is the identity write and authentication API.
type Service struct {
	repo *store.Repo
}

// New returns a Service over the repository.
func New(repo *store.Repo) *Service { return &Service{repo: repo} }

// bearerPrefix is the only authorization scheme accepted.
const bearerPrefix = "Bearer "

// Authenticate resolves an Authorization header to a principal.
//
// ⚠ Unknown token, revoked token and deactivated user all return the SAME
// core.ErrUnauthorized. Distinguishing them would turn this into an oracle: a
// caller holding a random string could learn whether it was ever a real token,
// and a caller holding a revoked one could learn that the account still exists.
// One error for three causes is the whole point, and the three separate tests
// that assert it exist because a single negative test covers only one path.
func (s *Service) Authenticate(ctx context.Context, header string, now time.Time) (core.Principal, error) {
	if !strings.HasPrefix(header, bearerPrefix) {
		return core.Principal{}, core.ErrUnauthorized
	}
	raw := strings.TrimSpace(strings.TrimPrefix(header, bearerPrefix))
	if raw == "" {
		return core.Principal{}, core.ErrUnauthorized
	}

	tok, err := s.repo.TokenByHash(ctx, HashToken(raw))
	if err != nil {
		// Includes core.ErrNotFound. Collapsed deliberately.
		return core.Principal{}, core.ErrUnauthorized
	}

	// The lookup was by hash, so this comparison can only ever succeed — it is
	// defence in depth against a future change that looks a token up some other
	// way and compares it here. Constant-time because comparing secrets with ==
	// is a habit worth not having.
	if subtle.ConstantTimeCompare([]byte(tok.Hash), []byte(HashToken(raw))) != 1 {
		return core.Principal{}, core.ErrUnauthorized
	}
	if tok.Revoked {
		return core.Principal{}, core.ErrUnauthorized
	}

	u, err := s.repo.UserByID(ctx, tok.UserID)
	if err != nil || !u.Active {
		return core.Principal{}, core.ErrUnauthorized
	}

	s.repo.TouchToken(ctx, tok.ID, now)

	// The role comes from the TOKEN ROW, not from the token string's prefix and
	// not from the user's role — a user may hold tokens of different scopes.
	return core.Principal{UserID: u.ID, Role: tok.Role, TokenID: tok.ID}, nil
}

// CreateUser registers a customer, a worker account or another administrator.
//
// Only an admin may call it: the operator's rule is that accounts are created by
// an administrator and never by self-service.
func (s *Service) CreateUser(ctx context.Context, actor core.Principal, email string, role core.Role, now time.Time) (core.User, error) {
	if !actor.IsAdmin() {
		return core.User{}, core.ErrForbidden
	}
	return s.createUser(ctx, email, role, now)
}

func (s *Service) createUser(ctx context.Context, email string, role core.Role, now time.Time) (core.User, error) {
	if !role.Valid() {
		return core.User{}, fmt.Errorf("%w: unknown role %q", core.ErrInvalidParam, role)
	}
	norm, err := normaliseEmail(email)
	if err != nil {
		return core.User{}, err
	}

	u := core.User{
		ID:          core.NewID(),
		Email:       norm,
		Role:        role,
		Credits:     0,
		BufferLimit: defaultBufferLimit,
		Priority:    0,
		JobTTLSecs:  0,
		Active:      true,
		CreatedAt:   now,
	}
	if err := s.repo.CreateUser(ctx, u); err != nil {
		return core.User{}, err
	}
	return u, nil
}

// normaliseEmail lower-cases and trims, and rejects anything that is obviously
// not an address.
//
// Normalising matters more than the shape check: without it "Foo@Example.com"
// and "foo@example.com" are two accounts for one person, with two balances and
// two buffer limits, and nothing in the dashboard would show them as related.
// The shape check is deliberately minimal — full RFC 5322 validation rejects
// addresses that work and accepts ones that do not, and the real proof that an
// address exists is sending to it.
func normaliseEmail(email string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(email))
	if norm == "" {
		return "", fmt.Errorf("%w: email is empty", core.ErrInvalidParam)
	}
	at := strings.Index(norm, "@")
	if at <= 0 || at == len(norm)-1 {
		return "", fmt.Errorf("%w: %q is not an email address", core.ErrInvalidParam, email)
	}
	if strings.ContainsAny(norm, " \t\n") {
		return "", fmt.Errorf("%w: email contains whitespace", core.ErrInvalidParam)
	}
	return norm, nil
}

// MintToken issues a new bearer token and returns (id, plaintext).
//
// The plaintext is returned exactly once and is recoverable from nowhere
// afterwards.
func (s *Service) MintToken(ctx context.Context, actor core.Principal, userID string, role core.Role, label string, now time.Time) (string, string, error) {
	if !actor.IsAdmin() {
		return "", "", core.ErrForbidden
	}
	if !role.Valid() {
		return "", "", fmt.Errorf("%w: unknown role %q", core.ErrInvalidParam, role)
	}
	if _, err := s.repo.UserByID(ctx, userID); err != nil {
		return "", "", err
	}
	return s.mint(ctx, userID, role, label, now)
}

func (s *Service) mint(ctx context.Context, userID string, role core.Role, label string, now time.Time) (string, string, error) {
	plaintext, err := GenerateToken(role)
	if err != nil {
		return "", "", err
	}
	tok := core.Token{
		ID:        core.NewID(),
		UserID:    userID,
		Role:      role,
		Hash:      HashToken(plaintext),
		Label:     label,
		CreatedAt: now,
	}
	if err := s.repo.CreateToken(ctx, tok); err != nil {
		return "", "", err
	}
	return tok.ID, plaintext, nil
}

// RevokeToken marks a token unusable.
func (s *Service) RevokeToken(ctx context.Context, actor core.Principal, tokenID string) error {
	if !actor.IsAdmin() {
		return core.ErrForbidden
	}
	return s.repo.RevokeToken(ctx, tokenID)
}

// ListTokens returns a user's tokens, hashes only.
func (s *Service) ListTokens(ctx context.Context, actor core.Principal, userID string) ([]core.Token, error) {
	if !actor.IsAdmin() {
		return nil, core.ErrForbidden
	}
	return s.repo.ListTokens(ctx, userID)
}

// SetActive enables or disables an account.
//
// Deactivating is the blunt instrument: every token the user holds stops working
// at once, without having to find and revoke each one.
func (s *Service) SetActive(ctx context.Context, actor core.Principal, userID string, active bool) error {
	if !actor.IsAdmin() {
		return core.ErrForbidden
	}
	u, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	u.Active = active
	return s.repo.UpdateUser(ctx, u)
}

// Bootstrap creates the FIRST administrator and returns its token.
//
// This is the only unauthenticated write in the system, and it exists because
// the rules would otherwise have no entry point: only an admin can create users,
// and a fresh database has no admin. It is refused once any admin exists, so it
// cannot become a standing way to mint administrators.
func (s *Service) Bootstrap(ctx context.Context, email string, now time.Time) (core.User, string, error) {
	existing, err := s.repo.ListUsers(ctx)
	if err != nil {
		return core.User{}, "", err
	}
	for _, u := range existing {
		if u.Role == core.RoleAdmin {
			return core.User{}, "", fmt.Errorf("%w: an administrator already exists", core.ErrConflict)
		}
	}

	u, err := s.createUser(ctx, email, core.RoleAdmin, now)
	if err != nil {
		return core.User{}, "", err
	}
	_, plaintext, err := s.mint(ctx, u.ID, core.RoleAdmin, "bootstrap", now)
	if err != nil {
		return core.User{}, "", err
	}
	return u, plaintext, nil
}

// ErrNoAdmin reports that the system has not been bootstrapped.
var ErrNoAdmin = errors.New("no administrator exists")
