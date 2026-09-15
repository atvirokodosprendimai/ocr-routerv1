// Package session stores browser sessions: a token row with a deadline.
//
// It owns rows and nothing else. Turning a session into a caller — a
// core.Principal — is identity's job, and keeping that split is what stops this
// becoming a second authentication system alongside bearer tokens.
package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/identity"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// secretBytes is the entropy behind a session cookie. The same 32 bytes a bearer
// token carries, for the same reason: far past brute force, and short enough to
// sit in a cookie.
const secretBytes = 32

// Store is the session table's read and write API.
type Store struct {
	db *store.DB
}

// New returns a Store over the database handles.
func New(db *store.DB) *Store { return &Store{db: db} }

// Create issues a session and returns its secret IN PLAINTEXT, exactly once.
//
// ⚠ The plaintext is written nowhere. The row keeps identity.HashToken(secret) —
// SHA-256, the same function the tokens table uses — so reading this table gives
// an attacker nothing they can present. A plain hash rather than a KDF is right
// here for the same reason it is right for tokens: the secret is 32 uniform
// random bytes, so there is no dictionary to attack and a work factor would only
// add latency to every request.
func (s *Store) Create(ctx context.Context, userID string, now, expiresAt time.Time) (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating session secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)

	if _, err := s.db.Write.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, token_hash, created_at, expires_at, revoked)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		core.NewID(), userID, identity.HashToken(secret), now.Unix(), expiresAt.Unix()); err != nil {
		return "", err
	}
	return secret, nil
}

// Resolve returns the session a secret names, or core.ErrNotFound.
//
// ⚠ Expiry is read from the row and NEVER extended. A sliding window refreshed
// on each request means a session ends for nobody who keeps a tab open, which is
// how a browser on an unlocked laptop becomes a permanent admin credential.
//
// Revoked, expired and unknown all return the same error — a caller must not be
// able to tell a secret that was never issued from one that was.
func (s *Store) Resolve(ctx context.Context, secret string, now time.Time) (core.Session, error) {
	if secret == "" {
		return core.Session{}, core.ErrNotFound
	}

	var (
		sess    core.Session
		created int64
		expires int64
		revoked int
	)
	err := s.db.Read.QueryRowContext(ctx,
		`SELECT id, user_id, created_at, expires_at, revoked
		   FROM sessions WHERE token_hash = ?`,
		identity.HashToken(secret),
	).Scan(&sess.ID, &sess.UserID, &created, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Session{}, core.ErrNotFound
	}
	if err != nil {
		return core.Session{}, err
	}

	sess.CreatedAt = time.Unix(created, 0).UTC()
	sess.ExpiresAt = time.Unix(expires, 0).UTC()
	sess.Revoked = revoked != 0

	if sess.Revoked {
		return core.Session{}, core.ErrNotFound
	}
	// At the deadline counts as expired: a session that lives one second past its
	// stated lifetime is a session whose lifetime is not what it says.
	if !now.Before(sess.ExpiresAt) {
		return core.Session{}, core.ErrNotFound
	}
	return sess, nil
}

// Revoke ends a session server-side.
//
// ⚠ This, not clearing the cookie, is what makes logout real. A cleared cookie
// leaves a live credential with anyone who copied the value.
func (s *Store) Revoke(ctx context.Context, secret string) error {
	if secret == "" {
		return nil
	}
	_, err := s.db.Write.ExecContext(ctx,
		`UPDATE sessions SET revoked = 1 WHERE token_hash = ?`, identity.HashToken(secret))
	return err
}

// RevokeAllForUser ends every session a user holds.
//
// Used when an account is deactivated or demoted. Resolution re-reads the user
// row anyway, so this is defence in depth rather than the mechanism — it keeps
// the table from holding rows that can never succeed.
func (s *Store) RevokeAllForUser(ctx context.Context, userID string) error {
	_, err := s.db.Write.ExecContext(ctx,
		`UPDATE sessions SET revoked = 1 WHERE user_id = ?`, userID)
	return err
}

// SweepExpired deletes expired and revoked rows and returns how many went.
//
// Called from the reaper tick that already exists. Returning the count is what
// lets a test assert the table SHRANK rather than that a function was called.
func (s *Store) SweepExpired(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.Write.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ? OR revoked = 1`, now.Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Len reports how many rows the table holds. Tests only.
func (s *Store) Len(ctx context.Context) (int, error) {
	var n int
	err := s.db.Read.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n)
	return n, err
}
